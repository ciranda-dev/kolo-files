package kolofiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
)

// marksUsable reports ErrInvalidArgument where marks do not apply: on a writer, or a reader without a local backend.
func (v *Volume) marksUsable() error {
	if v.writer() || v.local == nil {
		return fmt.Errorf("%w: marks need a reader with a local backend", ErrInvalidArgument)
	}
	return nil
}

// loadMarks reads this volume's marks from the local backend.
func (v *Volume) loadMarks() error {
	if v.local == nil {
		return nil
	}
	start := v.localPrefix(localMark)
	marks := map[NodeID]Mark{}
	bad := false
	err := v.local.Scan(start, upper(start), false, 0, func(k, val []byte) bool {
		var id NodeID
		if len(k) != len(start)+len(id)+8 || len(val) != 1 || val[0] == byte(MarkInherit) || val[0] > byte(MarkKeepLocal) {
			bad = true
			return false
		}
		copy(id[:], k[len(start):])
		marks[id] = Mark(val[0])
		return true
	})
	if err != nil {
		return fmt.Errorf("kolofiles: read marks: %w", err)
	}
	if bad {
		return fmt.Errorf("%w: a mark in the local backend is malformed", ErrCorrupt)
	}
	v.marks = marks
	return nil
}

// upper returns the first key after every key that starts with prefix, which does not end in 0xff here.
func upper(prefix []byte) []byte {
	end := bytes.Clone(prefix)
	end[len(end)-1]++
	return end
}

// effectiveMark returns the mark that applies to id: its own, else its nearest marked ancestor's, else the root's
// default MarkFetchOnDemand. Call it holding mu.
func (v *Volume) effectiveMark(st *state, id NodeID) Mark {
	cur := id
	for steps := 0; steps <= v.opts.maxDepth; steps++ {
		if m := v.marks[cur]; m != MarkInherit {
			return m
		}
		n := st.nodes[cur]
		if cur == rootID || n == nil {
			return MarkFetchOnDemand
		}
		cur = n.parent
	}
	return MarkFetchOnDemand
}

// SetMark sets the mark of a path on this reader device, MarkInherit clears it; a directory's mark covers everything
// below it. Marks need a reader opened with a local backend.
func (v *Volume) SetMark(name string, m Mark) error {
	pe := func(err error) error { return &fs.PathError{Op: "kolofiles: setmark", Path: name, Err: err} }
	if m > MarkKeepLocal {
		return pe(fmt.Errorf("%w: unknown mark", ErrInvalidArgument))
	}
	if err := v.marksUsable(); err != nil {
		return pe(err)
	}
	names, err := v.opts.parsePath(name, false)
	if err != nil {
		return pe(err)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.mu.RLock()
	closed := v.closed
	var id NodeID
	var created uint64
	if !closed {
		var n *node
		if id, n, err = v.st.lookup(names); err == nil {
			created = n.created
		}
	}
	v.mu.RUnlock()
	if closed {
		return pe(ErrClosed)
	}
	if err != nil {
		return pe(err)
	}
	key := v.markKey(id, created)
	if m == MarkInherit {
		err = v.local.Delete(key)
	} else {
		err = v.local.Put(key, []byte{byte(m)})
	}
	if err != nil {
		return pe(fmt.Errorf("kolofiles: store mark: %w", err))
	}
	v.mu.Lock()
	marks := maps.Clone(v.marks)
	if m == MarkInherit {
		delete(marks, id)
	} else {
		marks[id] = m
	}
	v.marks = marks
	v.mu.Unlock()
	if err := v.cleanLocal(); err != nil {
		return pe(err)
	}
	return nil
}

// Mark returns the mark set on a path itself, MarkInherit when it has none.
func (v *Volume) Mark(name string) (Mark, error) {
	pe := func(err error) error { return &fs.PathError{Op: "kolofiles: mark", Path: name, Err: err} }
	if err := v.marksUsable(); err != nil {
		return MarkInherit, pe(err)
	}
	names, err := v.opts.parsePath(name, false)
	if err != nil {
		return MarkInherit, pe(err)
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return MarkInherit, pe(ErrClosed)
	}
	id, _, err := v.st.lookup(names)
	if err != nil {
		return MarkInherit, pe(err)
	}
	return v.marks[id], nil
}

// cleanLocal removes the marks and kept objects of nodes that were removed, and the kept objects of nodes that
// are not MarkKeepLocal files.
func (v *Volume) cleanLocal() error {
	var keys [][]byte
	var dropped []NodeID
	v.mu.RLock()
	st, end := v.st, v.n
	for _, kind := range []byte{localMark, localObject} {
		start := v.localPrefix(kind)
		err := v.local.Scan(start, upper(start), false, 0, func(k, _ []byte) bool {
			var id NodeID
			if len(k) < len(start)+len(id)+8 {
				keys = append(keys, bytes.Clone(k))
				return true
			}
			copy(id[:], k[len(start):])
			created := binary.BigEndian.Uint64(k[len(start)+len(id):])
			n := st.nodes[id]
			stale := created < end
			if n != nil {
				stale = created != n.created || (kind == localObject && v.effectiveMark(st, id) != MarkKeepLocal)
			}
			if stale {
				keys = append(keys, bytes.Clone(k))
				if kind == localMark {
					dropped = append(dropped, id)
				}
			}
			return true
		})
		if err != nil {
			v.mu.RUnlock()
			return fmt.Errorf("kolofiles: scan local objects: %w", err)
		}
	}
	v.mu.RUnlock()
	if len(keys) == 0 {
		return nil
	}
	b := v.local.NewBatch()
	for _, k := range keys {
		b.Delete(k)
	}
	if err := b.Commit(); err != nil {
		return fmt.Errorf("kolofiles: remove local objects: %w", err)
	}
	if len(dropped) > 0 {
		v.mu.Lock()
		marks := maps.Clone(v.marks)
		for _, id := range dropped {
			delete(marks, id)
		}
		v.marks = marks
		v.mu.Unlock()
	}
	return nil
}

// Prefetch reads every MarkKeepLocal file's current version through the local backend, keeping its objects and
// dropping those of older versions. It goes on after a file fails and returns every error. It holds the change
// lock, so Refresh, SetMark and Close wait for it: end a long one with ctx.
func (v *Volume) Prefetch(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidArgument
	}
	if err := v.marksUsable(); err != nil {
		return err
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	type file struct {
		id NodeID
		n  node
	}
	var files []file
	v.mu.RLock()
	if v.closed {
		v.mu.RUnlock()
		return ErrClosed
	}
	if len(v.marks) > 0 {
		for id, n := range v.st.nodes {
			if !n.dir && v.effectiveMark(v.st, id) == MarkKeepLocal {
				files = append(files, file{id, *n})
			}
		}
	}
	v.mu.RUnlock()
	slices.SortFunc(files, func(a, b file) int { return bytes.Compare(a.id[:], b.id[:]) })
	var errs []error
	for _, f := range files {
		if err := v.prefetch(ctx, f.id, &f.n); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (v *Volume) prefetch(ctx context.Context, id NodeID, n *node) error {
	r, t, err := v.openContent(ctx, id, n, true)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, r)
	r.Close()
	if err != nil {
		return fmt.Errorf("kolofiles: prefetch: %w", err)
	}
	start := v.objectPrefix(id, n.created)
	var stale [][]byte
	err = v.local.Scan(start, upper(start), false, 0, func(k, _ []byte) bool {
		var h koloblobs.Hash
		if len(k) == len(start)+len(h) {
			copy(h[:], k[len(start):])
		}
		if _, ok := t.objs[h]; !ok {
			stale = append(stale, bytes.Clone(k))
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("kolofiles: scan local objects: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	b := v.local.NewBatch()
	for _, k := range stale {
		b.Delete(k)
	}
	if err := b.Commit(); err != nil {
		return fmt.Errorf("kolofiles: remove local objects: %w", err)
	}
	return nil
}
