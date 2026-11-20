package kolofiles

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
)

// testHook, when set by a test, is called after each step of a delete: "remove", "purge" and "destroy". An error
// it returns ends the delete there, as a crash would.
var testHook func(step string) error

func hook(step string) error {
	if testHook != nil {
		return testHook(step)
	}
	return nil
}

// Delete removes the file or directory name with everything below it and destroys every name, file key and object
// it held. The root cannot be deleted; a delete that fails after its REMOVE op is finished by the next Open.
func (v *Volume) Delete(name string) error {
	pe := func(err error) error { return &fs.PathError{Op: "kolofiles: delete", Path: name, Err: err} }
	names, err := v.opts.parsePath(name, false)
	if err != nil {
		return pe(err)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if err := v.writable(); err != nil {
		return pe(err)
	}
	if names == nil {
		return pe(fs.ErrInvalid)
	}
	v.mu.RLock()
	id, _, err := v.st.lookup(names)
	var nodes []NodeID
	skip := v.skip
	if err == nil {
		nodes = v.st.subtree(id)
	}
	v.mu.RUnlock()
	if err != nil {
		return pe(err)
	}
	set := setOf(nodes)
	meta, objs, err := v.collect(set, skip, nil, nil)
	if err != nil {
		return pe(err)
	}
	if _, err := v.commit(&op{kind: kindRemove, node: id}); err != nil {
		return pe(err)
	}
	if err := hook("remove"); err != nil {
		return pe(err)
	}
	if err := v.purge(id, nil, nil, meta, objs); err != nil {
		return pe(err)
	}
	v.mu.Lock()
	for n := range set {
		delete(v.byNode, n)
	}
	v.mu.Unlock()
	return nil
}

func setOf[T comparable](list []T) map[T]struct{} {
	m := make(map[T]struct{}, len(list))
	for _, x := range list {
		m[x] = struct{}{}
	}
	return m
}

// collect returns, ascending, the metadata entries of the MKDIR, CREATE, WRITE and MOVE ops about nodes and the
// content entries of every object of their versions and object tables, leaving out the entries in listedMeta and
// listedObjs. It reads only the entries the writer's index lists for nodes, skipping those in skip and the destroyed
// ones, plus each version's object table.
func (v *Volume) collect(nodes map[NodeID]struct{}, skip []uint64, listedMeta, listedObjs map[uint64]struct{}) (meta, objs []uint64, err error) {
	type file struct {
		key    koloblobs.Key
		hasKey bool
		vers   []version
	}
	files := map[NodeID]*file{}
	defer func() {
		for _, f := range files {
			clear(f.key[:])
		}
	}()
	var entries []uint64
	v.mu.RLock()
	for id := range nodes {
		entries = append(entries, v.byNode[id]...)
	}
	v.mu.RUnlock()
	slices.Sort(entries)
	for _, i := range entries {
		for len(skip) > 0 && skip[0] < i {
			skip = skip[1:]
		}
		if len(skip) > 0 && skip[0] == i {
			continue
		}
		b, err := v.meta.Get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			continue
		}
		if err != nil {
			return nil, nil, readErr(err)
		}
		o, err := decodeOp(b, v.opts.maxName, true)
		clear(b)
		if err != nil {
			return nil, nil, entryErr(i, err)
		}
		_, about := nodes[o.node]
		if !about || o.kind < kindMkdir || o.kind > kindMove {
			clear(o.key[:])
			continue
		}
		if _, ok := listedMeta[i]; !ok {
			meta = append(meta, i)
		}
		if o.kind == kindMkdir || o.kind == kindMove {
			continue
		}
		f := files[o.node]
		if f == nil {
			f = &file{}
			files[o.node] = f
		}
		if o.kind == kindCreate {
			f.key, f.hasKey = o.key, true
			clear(o.key[:])
		}
		f.vers = append(f.vers, o.ver)
	}
	found := map[uint64]struct{}{}
	fetch := v.fetchFunc()
	for _, f := range files {
		if !f.hasKey {
			return nil, nil, fmt.Errorf("%w: a version of a file without its create op", ErrCorrupt)
		}
		read := map[koloblobs.ID]struct{}{}
		for _, ver := range f.vers {
			if _, ok := read[ver.table]; ok {
				continue
			}
			read[ver.table] = struct{}{}
			for _, l := range ver.locs {
				found[l.i] = struct{}{}
			}
			locs, err := v.readTable(context.Background(), ver, f.key, koloblobs.NewRemoteBackend(fetch, locateIn(ver.locs)))
			if err != nil {
				return nil, nil, err
			}
			for _, l := range locs {
				found[l.i] = struct{}{}
			}
		}
	}
	for i := range found {
		if _, ok := listedObjs[i]; !ok {
			objs = append(objs, i)
		}
	}
	slices.Sort(objs)
	return meta, objs, nil
}

// purge appends the PURGE ops of the removed node id that list meta and objs, each under the largest entry
// kolo-sync carries, the last with done; then it destroys every entry of the sequence, including seqMeta and
// seqObjs that earlier PURGE ops of it listed.
func (v *Volume) purge(id NodeID, seqMeta, seqObjs, meta, objs []uint64) error {
	allMeta := slices.Concat(seqMeta, meta)
	allObjs := slices.Concat(seqObjs, objs)
	slices.Sort(allMeta)
	slices.Sort(allObjs)
	for first := true; first || len(meta)+len(objs) > 0; first = false {
		o := &op{kind: kindPurge, node: id}
		k := min(len(meta), purgeItems)
		o.meta, meta = meta[:k], meta[k:]
		k = min(len(objs), purgeItems-len(o.meta))
		o.objs, objs = objs[:k], objs[k:]
		o.done = len(meta)+len(objs) == 0
		if _, err := v.commit(o); err != nil {
			return err
		}
		if o.done {
			v.mu.Lock()
			v.skip = merge(v.skip, allMeta)
			purged := maps.Clone(v.purged)
			purged[id] = struct{}{}
			v.purged = purged
			v.mu.Unlock()
		}
		if err := hook("purge"); err != nil {
			return err
		}
	}
	return v.destroy(allMeta, allObjs)
}

// merge returns a new ascending slice of the entries of a and b, which are ascending.
func merge(a, b []uint64) []uint64 {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// destroy deletes the listed content entries, then the listed metadata entries, each in one synced batch.
func (v *Volume) destroy(meta, objs []uint64) error {
	v.destroyMu.Lock()
	defer v.destroyMu.Unlock()
	if len(objs) > 0 {
		if err := v.content.Delete(objs...); err != nil {
			return fmt.Errorf("kolofiles: destroy content entries: %w", err)
		}
	}
	if err := hook("destroy"); err != nil {
		return err
	}
	if len(meta) > 0 {
		if err := v.meta.Delete(meta...); err != nil {
			return fmt.Errorf("kolofiles: destroy metadata entries: %w", err)
		}
	}
	return nil
}

// resume finishes the deletes a crash interrupted: it appends the PURGE ops of every REMOVE without a complete
// sequence, then destroys every listed entry still readable. Before that it checks that what a sequence lists
// belongs to its removed nodes.
func (v *Volume) resume(r *replayed) error {
	ids := slices.Collect(maps.Keys(r.seqs))
	slices.SortFunc(ids, func(a, b NodeID) int { return cmp.Compare(r.seqs[a].remove, r.seqs[b].remove) })
	var meta, objs []uint64
	for _, id := range ids {
		s := r.seqs[id]
		if s.done != 0 {
			if err := v.checkListed(id, s); err != nil {
				return err
			}
			meta = append(meta, s.meta...)
			objs = append(objs, s.objs...)
			continue
		}
		v.mu.RLock()
		skip := v.skip
		v.mu.RUnlock()
		m, o, err := v.collect(setOf(r.gone[id]), skip, nil, nil)
		if err != nil {
			return err
		}
		m, okMeta := without(m, s.meta)
		o, okObjs := without(o, s.objs)
		if !okMeta || !okObjs {
			return fmt.Errorf("%w: a purge lists an entry that is not one of its removed nodes", ErrCorrupt)
		}
		if err := v.purge(id, s.meta, s.objs, m, o); err != nil {
			return err
		}
	}
	v.dropPurged()
	liveMeta, err := liveEntries(v.meta, meta)
	if err != nil {
		return err
	}
	liveObjs, err := liveEntries(v.content, objs)
	if err != nil {
		return err
	}
	if len(liveMeta)+len(liveObjs) == 0 {
		return nil
	}
	slices.Sort(liveMeta)
	slices.Sort(liveObjs)
	return v.destroy(liveMeta, liveObjs)
}

// dropPurged removes the destroyed entries from the writer's index, which Open built with them for checkListed.
func (v *Volume) dropPurged() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, list := range v.byNode {
		list = slices.DeleteFunc(list, func(i uint64) bool { _, ok := slices.BinarySearch(v.skip, i); return ok })
		if len(list) == 0 {
			delete(v.byNode, id)
		} else {
			v.byNode[id] = list
		}
	}
}

// without returns the entries of all that are not in listed, and false if an entry of listed is not in all.
func without(all, listed []uint64) ([]uint64, bool) {
	want := setOf(listed)
	var rest []uint64
	for _, i := range all {
		if _, ok := want[i]; ok {
			delete(want, i)
		} else {
			rest = append(rest, i)
		}
	}
	return rest, len(want) == 0
}

// checkListed returns ErrCorrupt if a content entry that the complete sequence s of node id lists, and that is
// still readable, is not an object of the nodes its readable metadata entries are about, as collect finds them.
func (v *Volume) checkListed(id NodeID, s *sequence) error {
	objs, err := liveEntries(v.content, s.objs)
	if err != nil || len(objs) == 0 {
		return err
	}
	nodes := map[NodeID]struct{}{id: {}}
	for _, j := range s.meta {
		b, err := v.meta.Get(j)
		if errors.Is(err, kololog.ErrDeleted) {
			continue
		}
		if err != nil {
			return readErr(err)
		}
		o, err := decodeOp(b, v.opts.maxName, false)
		clear(b)
		if err != nil {
			return entryErr(j, err)
		}
		nodes[o.node] = struct{}{}
	}
	listed := setOf(s.meta)
	v.mu.RLock()
	skip := slices.DeleteFunc(slices.Clone(v.skip), func(i uint64) bool { _, ok := listed[i]; return ok })
	v.mu.RUnlock()
	_, found, err := v.collect(nodes, skip, nil, nil)
	if err != nil {
		return err
	}
	want := setOf(found)
	for _, i := range objs {
		if _, ok := want[i]; !ok {
			return fmt.Errorf("%w: a purge lists a content entry that is not an object of its nodes", ErrCorrupt)
		}
	}
	return nil
}

// liveEntries returns the entries of list that exist in l and are not destroyed.
func liveEntries(l interface {
	Len() uint64
	EntryInfo(uint64) (kololog.EntryInfo, error)
}, list []uint64) ([]uint64, error) {
	var out []uint64
	for _, i := range list {
		if i >= l.Len() {
			continue
		}
		e, err := l.EntryInfo(i)
		if err != nil {
			return nil, readErr(err)
		}
		if !e.Deleted {
			out = append(out, i)
		}
	}
	return out, nil
}
