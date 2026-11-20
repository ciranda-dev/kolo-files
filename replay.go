package kolofiles

import (
	"errors"
	"fmt"
	"slices"

	kololog "github.com/ciranda-dev/kolo-log"
)

// sequence is the REMOVE of a node and the PURGE ops that follow it.
type sequence struct {
	remove uint64   // entry of the REMOVE
	meta   []uint64 // metadata entries its PURGE ops list so far
	objs   []uint64 // content entries its PURGE ops list so far
	done   uint64   // entry of the PURGE with done set, plus one; 0 while incomplete
}

// replayed is the result of a full replay.
type replayed struct {
	st         *state
	n          uint64
	skip       []uint64
	purged     map[NodeID]struct{}
	contentLog kololog.LogID
	seqs       map[NodeID]*sequence // every REMOVE in the prefix
	gone       map[NodeID][]NodeID  // per REMOVE of an incomplete sequence: every node it dropped
	byNode     map[NodeID][]uint64  // writer: see Volume.byNode, with the entries of purged nodes still in
}

// replay rebuilds the tree from the longest prefix of the first n entries it can replay exactly: a reader stops at an
// entry held without its key and before a destroyed one that no complete PURGE sequence lists; a writer needs all n.
// Entries a complete sequence lists are skipped. Peak memory is every decoded op and a parent per node, besides the tree.
func (v *Volume) replay(n uint64, reader bool) (*replayed, error) {
	type entry struct {
		gone   bool
		op     *op
		listed bool   // a PURGE lists it
		by     uint64 // the done PURGE of the sequence that lists it, plus one; 0 for none
	}
	es := make([]entry, 0, min(n, 1<<16))
	seqs := map[NodeID]*sequence{}
	parent := map[NodeID]NodeID{} // the last parent a readable MKDIR, CREATE or MOVE gave each node
	var contentLog kololog.LogID
	for i := uint64(0); i < n; i++ {
		b, err := v.get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			es = append(es, entry{gone: true})
			continue
		}
		if reader && errors.Is(err, kololog.ErrKeyUnavailable) {
			break
		}
		if err != nil {
			return nil, readErr(err)
		}
		o, err := decodeOp(b, v.opts.maxName, false)
		clear(b)
		if err != nil {
			return nil, entryErr(i, err)
		}
		v.replayed.Add(1)
		o.ver.locs = nil
		bad := func(why string) error { return fmt.Errorf("%w: entry %d: %s", ErrCorrupt, i, why) }
		switch {
		case i == 0 && (o.kind != kindVolume || o.volume != v.id):
			return nil, bad("the first entry is not this volume")
		case i == 0:
			contentLog = o.contentLog
		case o.kind == kindVolume:
			return nil, bad("a volume entry after the first")
		case o.kind == kindRemove:
			if seqs[o.node] != nil {
				return nil, bad("removes a node twice")
			}
			seqs[o.node] = &sequence{remove: i}
		case o.kind == kindPurge:
			s := seqs[o.node]
			if s == nil || s.done != 0 {
				return nil, bad("purges a node that was not removed or is already purged")
			}
			for _, j := range o.meta {
				if j == 0 || j >= i || es[j].listed {
					return nil, bad("lists a wrong entry")
				}
				if e := es[j]; !e.gone && (e.op.kind == kindRemove || e.op.kind == kindPurge) {
					return nil, bad("lists a remove or purge op")
				}
				if e := es[j]; !e.gone && !under(parent, e.op.node, o.node, v.opts.maxDepth) {
					return nil, bad("lists an op about a node outside the removed tree")
				}
				es[j].listed = true
			}
			s.meta = append(s.meta, o.meta...)
			if !reader {
				s.objs = append(s.objs, o.objs...)
			}
			if o.done {
				s.done = i + 1
				for _, j := range s.meta {
					es[j].by = i + 1
				}
			}
			o.meta, o.objs = nil, nil
		case o.kind == kindMkdir || o.kind == kindCreate || o.kind == kindMove:
			parent[o.node] = o.parent
		}
		es = append(es, entry{op: o})
	}

	// m is the last done PURGE that explains a destroyed entry seen so far, plus one.
	var m, end uint64
	for i, e := range es {
		if e.gone && e.by == 0 {
			break
		}
		if e.gone {
			m = max(m, e.by)
		}
		if m <= uint64(i)+1 {
			end = uint64(i) + 1
		}
	}
	if !reader && end != n {
		return nil, fmt.Errorf("%w: destroyed entry without its purge op, or an entry without its key", ErrCorrupt)
	}

	r := &replayed{st: newState(), n: end, purged: map[NodeID]struct{}{}, contentLog: contentLog, seqs: map[NodeID]*sequence{}, gone: map[NodeID][]NodeID{}}
	for id, s := range seqs {
		if s.remove >= end {
			continue
		}
		if s.done != 0 && s.done-1 >= end {
			s.done = 0 // completed after the prefix: nothing of it applies yet
		}
		if s.done != 0 {
			r.purged[id] = struct{}{}
			r.skip = append(r.skip, s.meta...)
			for _, j := range s.meta {
				if es[j].op != nil {
					r.st.dead[es[j].op.node] = struct{}{} // skipped, so REMOVE would not drop it
				}
			}
		}
		r.seqs[id] = s
	}
	slices.Sort(r.skip)
	if !reader {
		r.byNode = map[NodeID][]uint64{}
		for i := uint64(1); i < end; i++ {
			if o := es[i].op; o != nil && o.kind >= kindMkdir && o.kind <= kindMove {
				r.byNode[o.node] = append(r.byNode[o.node], i)
			}
		}
	}
	isPurged := func(id NodeID) bool { _, ok := r.purged[id]; return ok }
	skip := r.skip
	for i := range end {
		for len(skip) > 0 && skip[0] < i {
			skip = skip[1:]
		}
		if len(skip) > 0 && skip[0] == i {
			continue
		}
		o := es[i].op
		gone, err := r.st.apply(i, o, v.opts.maxDepth, isPurged)
		if err != nil {
			return nil, err
		}
		if o.kind != kindRemove {
			continue
		}
		if r.seqs[o.node].done == 0 {
			r.gone[o.node] = gone
		}
	}
	return r, nil
}

// under reports whether node is x or lies below it by the last parents in parent. A node whose ops were destroyed,
// so that its chain breaks off, may be below x. The walk is bounded by maxDepth and by the size of parent, since
// the parents may hold a cycle.
func under(parent map[NodeID]NodeID, node, x NodeID, maxDepth int) bool {
	maxDepth = min(maxDepth, len(parent))
	for cur := node; maxDepth >= 0; maxDepth-- {
		if cur == x {
			return true
		}
		p, ok := parent[cur]
		if !ok {
			return cur != rootID
		}
		cur = p
	}
	return false
}

// install makes the result of a full replay the volume's state.
func (v *Volume) install(r *replayed) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.st, v.n, v.skip, v.purged = r.st, r.n, r.skip, r.purged
	if r.byNode != nil {
		v.byNode = r.byNode
	}
}

// headSize returns the size of the replica's verified head, 0 when it has none yet.
func (v *Volume) headSize() (uint64, error) {
	hdr := v.rep.Header()
	h, err := v.rep.Head()
	if errors.Is(err, kololog.ErrNoHead) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("kolofiles: %w", err)
	}
	vh, err := kololog.VerifyHead(hdr.LogKey, hdr.LogID, h)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return vh.Size(), nil
}

// rebuild replays a reader's replica in full and drops the marks and kept objects of the nodes that were removed.
func (v *Volume) rebuild() error {
	n, err := v.headSize()
	if err != nil {
		return err
	}
	r, err := v.replay(n, true)
	if err != nil {
		return err
	}
	v.install(r)
	if v.local == nil {
		return nil
	}
	return v.cleanLocal()
}

// Refresh brings a reader up to its replica's latest verified head, in place when it can, else by a full replay.
// Len may go down. On a writer it does nothing. On an error before the new tree is installed it stays as it was;
// an error from the clean-up of the local backend after that leaves the new tree.
func (v *Volume) Refresh() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.mu.RLock()
	closed, have := v.closed, v.n
	v.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if v.writer() {
		return nil
	}
	ok, err := v.refreshInPlace(have)
	if ok || err != nil {
		return err
	}
	return v.rebuild()
}

// refreshInPlace applies the entries after have to a copy of the tree, and reports false when a full replay is
// needed: a REMOVE, PURGE or destroyed entry is new, or one below have was destroyed. Its cost grows with Len,
// since it checks every entry below have.
func (v *Volume) refreshInPlace(have uint64) (bool, error) {
	n, err := v.headSize()
	if err != nil || n < have {
		return false, err
	}
	v.mu.RLock()
	skip := v.skip
	v.mu.RUnlock()
	for i := range have {
		info, err := v.rep.EntryInfo(i)
		if err != nil {
			return false, readErr(err)
		}
		if _, listed := slices.BinarySearch(skip, i); info.Deleted && !listed {
			return false, nil
		}
	}
	var ops []*op
	moved := false
	for i := have; i < n; i++ {
		b, err := v.rep.Get(i)
		if errors.Is(err, kololog.ErrKeyUnavailable) {
			break
		}
		if errors.Is(err, kololog.ErrDeleted) {
			return false, nil
		}
		if err != nil {
			return false, readErr(err)
		}
		o, err := decodeOp(b, v.opts.maxName, false)
		clear(b)
		if err != nil {
			return false, entryErr(i, err)
		}
		if o.kind == kindRemove || o.kind == kindPurge {
			return false, nil
		}
		if o.kind == kindVolume || have == 0 {
			return false, nil // the first entry is checked by a full replay
		}
		moved = moved || o.kind == kindMove
		o.ver.locs = nil
		ops = append(ops, o)
	}
	v.mu.RLock()
	st := v.st.clone()
	v.mu.RUnlock()
	never := func(NodeID) bool { return false }
	for k, o := range ops {
		v.replayed.Add(1)
		if _, err := st.apply(have+uint64(k), o, v.opts.maxDepth, never); err != nil {
			return false, err
		}
	}
	v.mu.Lock()
	v.st, v.n = st, have+uint64(len(ops))
	v.mu.Unlock()
	if moved && v.local != nil {
		return true, v.cleanLocal()
	}
	return true, nil
}

// replayTo replays the first n entries, skipping the entries in skip, and calls each with every op it applies.
// An entry destroyed since the volume's state was built is an error wrapping kololog.ErrDeleted.
func (v *Volume) replayTo(n uint64, skip []uint64, purged map[NodeID]struct{}, each func(i uint64, o *op, st *state) error) (*state, error) {
	isPurged := func(id NodeID) bool { _, ok := purged[id]; return ok }
	st := newState()
	for i := range n {
		for len(skip) > 0 && skip[0] < i {
			skip = skip[1:]
		}
		if len(skip) > 0 && skip[0] == i {
			continue
		}
		b, err := v.get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			return nil, fmt.Errorf("kolofiles: entry %d was destroyed since the last refresh: %w", i, err)
		}
		if err != nil {
			return nil, readErr(err)
		}
		o, err := decodeOp(b, v.opts.maxName, false)
		clear(b)
		if err != nil {
			return nil, entryErr(i, err)
		}
		v.replayed.Add(1)
		if each != nil {
			if err := each(i, o, st); err != nil {
				return nil, err
			}
		}
		if _, err := st.apply(i, o, v.opts.maxDepth, isPurged); err != nil {
			return nil, err
		}
	}
	return st, nil
}
