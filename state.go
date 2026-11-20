package kolofiles

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
)

// node is one file or directory of a tree.
type node struct {
	parent  NodeID
	name    string
	dir     bool
	blob    koloblobs.ID
	size    uint64
	mtime   int64
	created uint64 // entry of the MKDIR or CREATE op; 0 for the root
	version uint64 // entry of the CREATE or WRITE op of the file's current content
	changed uint64 // entry of the last op about the node
}

// state is a tree: every node, and the children of every directory by name. A node whose parent is not in the tree
// (its ops were destroyed by a delete) is in the state but not reachable from the root. dead holds every ID a
// REMOVE dropped, which no op may create again.
type state struct {
	nodes map[NodeID]*node
	kids  map[NodeID]map[string]NodeID
	dead  map[NodeID]struct{}
}

func newState() *state {
	return &state{nodes: map[NodeID]*node{rootID: {dir: true}}, kids: map[NodeID]map[string]NodeID{}, dead: map[NodeID]struct{}{}}
}

// clone returns a deep copy of s.
func (s *state) clone() *state {
	c := &state{nodes: make(map[NodeID]*node, len(s.nodes)), kids: make(map[NodeID]map[string]NodeID, len(s.kids)), dead: maps.Clone(s.dead)}
	for id, n := range s.nodes {
		cp := *n
		c.nodes[id] = &cp
	}
	for id, m := range s.kids {
		cm := make(map[string]NodeID, len(m))
		for k, v := range m {
			cm[k] = v
		}
		c.kids[id] = cm
	}
	return c
}

// depth walks up from id and returns how many names lie between the root and id: reachable is false when the walk
// meets a node that is not in the tree. It meets stop if stop is an ancestor of id or id itself. A walk longer than
// maxDepth is ErrLimit.
func (s *state) depth(id, stop NodeID, maxDepth int) (d int, reachable, met bool, err error) {
	for cur := id; ; d++ {
		if cur == stop {
			met = true
		}
		if cur == rootID {
			return d, true, met, nil
		}
		if d >= maxDepth {
			return 0, false, met, fmt.Errorf("%w: deeper than %d", ErrLimit, maxDepth)
		}
		n := s.nodes[cur]
		if n == nil {
			return d, false, met, nil
		}
		cur = n.parent
	}
}

// path returns the path of id, or "" with reachable false if it is not reachable from the root.
func (s *state) path(id NodeID, maxDepth int) (p string, reachable bool, err error) {
	var names []string
	for cur := id; cur != rootID; {
		if len(names) >= maxDepth {
			return "", false, fmt.Errorf("%w: deeper than %d", ErrLimit, maxDepth)
		}
		n := s.nodes[cur]
		if n == nil {
			return "", false, nil
		}
		names = append(names, n.name)
		cur = n.parent
	}
	if len(names) == 0 {
		return ".", true, nil
	}
	slices.Reverse(names)
	return strings.Join(names, "/"), true, nil
}

// lookup returns the node at names below the root.
func (s *state) lookup(names []string) (NodeID, *node, error) {
	cur, n := rootID, s.nodes[rootID]
	for _, name := range names {
		if !n.dir {
			return NodeID{}, nil, ErrNotDir
		}
		id, ok := s.kids[cur][name]
		if !ok {
			return NodeID{}, nil, fs.ErrNotExist
		}
		cur, n = id, s.nodes[id]
	}
	return cur, n, nil
}

// subtree returns id and every node below it, id first.
func (s *state) subtree(id NodeID) []NodeID {
	out := []NodeID{id}
	for k := 0; k < len(out); k++ {
		for _, c := range s.kids[out[k]] {
			out = append(out, c)
		}
	}
	return out
}

// height returns how many levels lie below id: 0 for a file or an empty directory.
func (s *state) height(id NodeID) int {
	type item struct {
		id NodeID
		d  int
	}
	h := 0
	stack := []item{{id, 0}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		h = max(h, it.d)
		for _, c := range s.kids[it.id] {
			stack = append(stack, item{c, it.d + 1})
		}
	}
	return h
}

// children returns the names below the directory id in byte order.
func (s *state) children(id NodeID) []string {
	m := s.kids[id]
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *state) attach(id, parent NodeID, name string) {
	m := s.kids[parent]
	if m == nil {
		m = map[string]NodeID{}
		s.kids[parent] = m
	}
	m[name] = id
}

func (s *state) detach(parent NodeID, name string) {
	m := s.kids[parent]
	delete(m, name)
	if len(m) == 0 {
		delete(s.kids, parent)
	}
}

// apply applies op o, entry i, checking every rule of the format. purged tells whether a node has a complete PURGE
// sequence, which allows a REMOVE of a node whose ops were destroyed. It returns the nodes a REMOVE drops.
func (s *state) apply(i uint64, o *op, maxDepth int, purged func(NodeID) bool) ([]NodeID, error) {
	bad := func(why string) error { return fmt.Errorf("%w: entry %d: %s", ErrCorrupt, i, why) }
	switch o.kind {
	case kindMkdir, kindCreate:
		if s.nodes[o.node] != nil || o.node == rootID {
			return nil, bad("creates an existing node")
		}
		if _, dead := s.dead[o.node]; dead {
			return nil, bad("creates a removed node")
		}
		if len(s.kids[o.node]) > 0 {
			return nil, bad("creates a node that is already a parent")
		}
		if err := s.checkTarget(o.node, o.parent, o.name, maxDepth, bad); err != nil {
			return nil, err
		}
		n := &node{parent: o.parent, name: o.name, dir: o.kind == kindMkdir, mtime: o.time, created: i, changed: i}
		if !n.dir {
			n.blob, n.size, n.version = o.ver.blob, o.ver.size, i
		}
		s.nodes[o.node] = n
		s.attach(o.node, o.parent, o.name)
	case kindWrite:
		n := s.nodes[o.node]
		if n == nil || o.node == rootID {
			return nil, bad("writes a node that does not exist")
		}
		if n.dir {
			return nil, bad("writes a directory")
		}
		n.blob, n.size, n.mtime, n.version, n.changed = o.ver.blob, o.ver.size, o.time, i, i
	case kindMove:
		n := s.nodes[o.node]
		if n == nil || o.node == rootID {
			return nil, bad("moves a node that does not exist or the root")
		}
		if n.parent == o.parent && n.name == o.name {
			n.changed = i
			return nil, nil
		}
		if err := s.checkTarget(o.node, o.parent, o.name, maxDepth, bad); err != nil {
			return nil, err
		}
		s.detach(n.parent, n.name)
		n.parent, n.name, n.changed = o.parent, o.name, i
		s.attach(o.node, o.parent, o.name)
	case kindRemove:
		if o.node == rootID {
			return nil, bad("removes the root")
		}
		n := s.nodes[o.node]
		if n == nil && !purged(o.node) {
			return nil, bad("removes a node that does not exist")
		}
		gone := s.subtree(o.node)
		if n != nil {
			s.detach(n.parent, n.name)
		} else {
			gone = gone[1:]
		}
		s.dead[o.node] = struct{}{}
		for _, id := range gone {
			delete(s.nodes, id)
			delete(s.kids, id)
			s.dead[id] = struct{}{}
		}
		if n == nil {
			delete(s.kids, o.node)
		}
		return gone, nil
	}
	return nil, nil
}

// checkTarget checks that id can be put under parent as name: the parent is not a file, the name is free, the
// parent is not id or below it, and the walk to the root stays within maxDepth.
func (s *state) checkTarget(id, parent NodeID, name string, maxDepth int, bad func(string) error) error {
	if p := s.nodes[parent]; p != nil && !p.dir {
		return bad("puts a node in a file")
	}
	if _, taken := s.kids[parent][name]; taken {
		return bad("puts a node on a taken name")
	}
	d, _, met, err := s.depth(parent, id, maxDepth)
	if met {
		return bad("moves a node into its own subtree")
	}
	if err != nil {
		return err
	}
	if d+1 > maxDepth {
		return fmt.Errorf("%w: deeper than %d", ErrLimit, maxDepth)
	}
	return nil
}
