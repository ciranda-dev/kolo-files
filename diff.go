package kolofiles

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
)

// Snapshot returns the tree after the first ver entries, 1 to Len. A deleted node is in no snapshot.
func (v *Volume) Snapshot(ver uint64) (*Snapshot, error) {
	v.destroyMu.RLock()
	defer v.destroyMu.RUnlock()
	v.mu.RLock()
	if v.closed {
		v.mu.RUnlock()
		return nil, ErrClosed
	}
	n, skip, purged := v.n, v.skip, v.purged
	if ver == 0 || ver > n {
		v.mu.RUnlock()
		return nil, fmt.Errorf("%w: version %d of %d", ErrInvalidArgument, ver, n)
	}
	if ver == n {
		st := v.st.clone()
		v.mu.RUnlock()
		return &Snapshot{w: view{v: v, st: st}, ver: ver}, nil
	}
	v.mu.RUnlock()
	st, err := v.replayTo(ver, skip, purged, nil)
	if err != nil {
		return nil, err
	}
	return &Snapshot{w: view{v: v, st: st}, ver: ver}, nil
}

// Diff returns one Change per node added, moved, modified or deleted between versions a and b, 1 <= a <= b <= Len,
// sorted by path, then node ID. A node deeper than MaxDepth, which only a hostile log has, has empty paths.
func (v *Volume) Diff(a, b uint64) ([]Change, error) {
	v.destroyMu.RLock()
	defer v.destroyMu.RUnlock()
	v.mu.RLock()
	closed, n, skip, purged := v.closed, v.n, v.skip, v.purged
	v.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if a == 0 || a > b || b > n {
		return nil, fmt.Errorf("%w: versions %d and %d of %d", ErrInvalidArgument, a, b, n)
	}
	subjects := map[NodeID]bool{} // true for a node removed in between
	rest := skip
	for i := a; i < b; i++ {
		for len(rest) > 0 && rest[0] < i {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == i {
			continue
		}
		bs, err := v.get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			return nil, fmt.Errorf("kolofiles: entry %d was destroyed since the last refresh: %w", i, err)
		}
		if err != nil {
			return nil, readErr(err)
		}
		o, err := decodeOp(bs, v.opts.maxName, false)
		clear(bs)
		if err != nil {
			return nil, entryErr(i, err)
		}
		v.replayed.Add(1)
		if o.kind >= kindMkdir && o.kind <= kindRemove {
			subjects[o.node] = subjects[o.node] || o.kind == kindRemove
		}
	}
	type seen struct {
		path string
		blob koloblobs.ID
	}
	before := map[NodeID]seen{}
	recorded := false
	record := func(st *state) error {
		recorded = true
		reach := newReach(st)
		for id := range subjects {
			p, ok, err := v.pathOf(st, reach, id)
			if err != nil {
				return err
			}
			if ok {
				before[id] = seen{p, st.nodes[id].blob}
			}
		}
		return nil
	}
	st, err := v.replayTo(b, skip, purged, func(i uint64, _ *op, st *state) error {
		if recorded || i < a {
			return nil
		}
		return record(st)
	})
	if err != nil {
		return nil, err
	}
	if !recorded {
		if err := record(st); err != nil {
			return nil, err
		}
	}
	var out []Change
	reach := newReach(st)
	for id, removed := range subjects {
		if removed {
			out = append(out, Change{Node: id, Deleted: true})
			continue
		}
		p, ok, err := v.pathOf(st, reach, id)
		if err != nil {
			return nil, err
		}
		old, had := before[id]
		c := Change{Node: id, Old: old.path}
		if ok {
			c.New = p
			c.Modified = had && old.blob != st.nodes[id].blob
		}
		if c.Old != c.New || c.Modified || (ok && !had) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y Change) int {
		return cmp.Or(cmp.Compare(changePath(x), changePath(y)), bytes.Compare(x.Node[:], y.Node[:]))
	})
	return out, nil
}

// reach remembers which nodes of one tree are reachable from the root, so that a diff walks each chain of parents once.
type reach struct {
	st    *state
	known map[NodeID]bool
}

func newReach(st *state) *reach { return &reach{st: st, known: map[NodeID]bool{}} }

// reachable reports whether the parents of id lead to the root, however deep. A hostile log can make a chain longer
// than MaxDepth, so the walk is bounded by the nodes in the tree, not by MaxDepth, and no chain is walked twice.
func (r *reach) reachable(id NodeID) bool {
	var chain []NodeID
	ok := false
	for cur := id; len(chain) <= len(r.st.nodes); {
		if cur == rootID {
			ok = true
			break
		}
		if known, hit := r.known[cur]; hit {
			ok = known
			break
		}
		chain = append(chain, cur)
		n := r.st.nodes[cur]
		if n == nil {
			break
		}
		cur = n.parent
	}
	for _, c := range chain {
		r.known[c] = ok
	}
	return ok
}

// pathOf returns the path of id, and false if id is not reachable from the root. A reachable node deeper than
// MaxDepth, which only a hostile log has, has the empty path.
func (v *Volume) pathOf(st *state, r *reach, id NodeID) (string, bool, error) {
	if !r.reachable(id) {
		return "", false, nil
	}
	p, ok, err := st.path(id, v.opts.maxDepth)
	if errors.Is(err, ErrLimit) {
		return "", true, nil
	}
	return p, ok, err
}

func changePath(c Change) string {
	if c.New != "" {
		return c.New
	}
	return c.Old
}
