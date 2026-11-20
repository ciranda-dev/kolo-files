package kolofiles

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	kolocodec "github.com/ciranda-dev/kolo-codec"
)

// Every hostile log is opened as a writer and as a reader, and both must refuse it the same way.
func TestHostileLogs(t *testing.T) {
	a, b := NodeID{1}, NodeID{2}
	mkdir := func(name string) *op { return &op{kind: kindMkdir, node: a, name: name} }
	chain := func(n int) []*op {
		var ops []*op
		parent := rootID
		for i := range n {
			id := NodeID{byte(i + 1)}
			ops = append(ops, &op{kind: kindMkdir, node: id, parent: parent, name: "d"})
			parent = id
		}
		return ops
	}
	for _, tc := range []struct {
		name string
		opts []Option
		ops  []*op
		want error
	}{
		{"name over the bound", []Option{WithMaxNameLength(8)}, []*op{mkdir("123456789")}, ErrLimit},
		{"name at the bound", []Option{WithMaxNameLength(8)}, []*op{mkdir("12345678")}, nil},
		{"name over the default bound", nil, []*op{mkdir(strings.Repeat("a", DefaultMaxNameLength+1))}, ErrLimit},
		{"name at the default bound", nil, []*op{mkdir(strings.Repeat("a", DefaultMaxNameLength))}, nil},
		{"multibyte name over the bound in bytes", []Option{WithMaxNameLength(3)}, []*op{mkdir("éé")}, ErrLimit},
		{"not NFC", nil, []*op{mkdir("é")}, ErrCorrupt},
		{"not NFC in a move", nil, []*op{mkdir("a"), {kind: kindMove, node: a, name: "é"}}, ErrCorrupt},
		{"not NFC in a create", nil, []*op{fileOp(a, rootID, "é")}, ErrCorrupt},
		{"NFC", nil, []*op{mkdir("é")}, nil},
		{"control character", nil, []*op{mkdir("a\x1fb")}, ErrCorrupt},
		{"empty name", nil, []*op{mkdir("")}, ErrCorrupt},
		{"dot", nil, []*op{mkdir(".")}, ErrCorrupt},
		{"dot dot", nil, []*op{mkdir("..")}, ErrCorrupt},
		{"slash", nil, []*op{mkdir("a/b")}, ErrCorrupt},
		{"invalid UTF-8", nil, []*op{mkdir("a\xffb")}, ErrCorrupt},
		{"deeper than the bound", []Option{WithMaxDepth(3)}, chain(4), ErrLimit},
		{"as deep as the bound", []Option{WithMaxDepth(3)}, chain(3), nil},
		{"a move that makes a path too deep", []Option{WithMaxDepth(3)}, append(chain(3), &op{kind: kindMkdir, node: NodeID{9}, name: "x"}, &op{kind: kindMove, node: NodeID{9}, parent: NodeID{3}, name: "x"}), ErrLimit},
		{"a move into its own subtree", nil, []*op{mkdir("a"), {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindMove, node: a, parent: b, name: "a"}}, ErrCorrupt},
	} {
		if err := hostileOpts(t, tc.opts, tc.ops...); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		} else if err != nil {
			wantPrefix(t, err)
		}
	}
}

// Entries that are not well-formed ops are ErrCorrupt for writer and reader alike.
func TestHostileEntries(t *testing.T) {
	node := make([]byte, 16)
	good := encodeOp(&op{kind: kindMkdir, node: NodeID{1}, name: "a"})
	for _, tc := range []struct {
		name  string
		entry []byte
	}{
		{"empty", []byte{}},
		{"other tag", raw(func(e *kolocodec.Encoder) { e.PutString("kolo-kv/op/v0") })},
		{"unknown kind", raw(func(e *kolocodec.Encoder) { head(e, kindPurge+1) })},
		{"dependencies on other writers", raw(func(e *kolocodec.Encoder) {
			e.PutString(opTag)
			e.PutUvarint(kindRemove)
			e.PutUvarint(1)
			e.PutFixed(make([]byte, 32))
			e.PutUvarint(1)
			e.PutFixed(node)
		})},
		{"trailing bytes", append(append([]byte{}, good...), 0)},
		{"truncated", good[:len(good)-2]},
		{"a second volume entry", encodeOp(&op{kind: kindVolume})},
		{"over the largest entry", make([]byte, maxEntry+1)},
	} {
		if err := hostileEntries(t, nil, tc.entry); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", tc.name, err)
		} else {
			wantPrefix(t, err)
		}
	}
}

// A hostile log can move a directory so that nodes below it lie deeper than MaxDepth: replay checks only the moved
// node. Such nodes have no path, and Diff reports them with empty paths instead of failing.
func TestDiffDeepNodes(t *testing.T) {
	a, b, top, mid, low := NodeID{1}, NodeID{2}, NodeID{3}, NodeID{4}, NodeID{5}
	e := newEnv(t)
	entries := [][]byte{encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()})}
	for _, o := range []*op{
		{kind: kindMkdir, node: a, name: "a"},
		{kind: kindMkdir, node: b, parent: a, name: "b"},
		{kind: kindMkdir, node: top, name: "t"},
		{kind: kindMkdir, node: mid, parent: top, name: "m"},
		{kind: kindMkdir, node: low, parent: mid, name: "l"},
		{kind: kindMove, node: top, parent: b, name: "t"}, // t is at depth 3, l at depth 5
	} {
		entries = append(entries, encodeOp(o))
	}
	appendRaw(t, e.meta, entries...)
	v, err := Open(e.meta, e.content, WithMaxDepth(3))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.Stat("a/b/t/m"); !errors.Is(err, ErrLimit) {
		t.Fatalf("Stat of a node too deep: %v", err)
	}
	got, err := v.Diff(1, v.Len())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range got {
		paths = append(paths, c.New)
	}
	if want := []string{"", "", "a", "a/b", "a/b/t"}; !slices.Equal(paths, want) {
		t.Fatalf("diff paths %q, want %q", paths, want)
	}
	// The move alone: the deep nodes did not change their paths below MaxDepth, so only t is reported.
	got, err = v.Diff(6, v.Len())
	if err != nil || len(got) != 1 || got[0].Node != top || got[0].Old != "t" || got[0].New != "a/b/t" {
		t.Fatalf("diff of the move: %+v, %v", got, err)
	}
}

// A PURGE that lists an op about a node in a cycle of parents is refused, however large MaxDepth is.
func TestPurgeListsNodeInCycle(t *testing.T) {
	a, b, c := NodeID{1}, NodeID{2}, NodeID{3}
	done := make(chan error, 1)
	go func() {
		done <- hostileOpts(t, []Option{WithMaxDepth(math.MaxInt)},
			&op{kind: kindMkdir, node: b, parent: a, name: "b"},
			&op{kind: kindMkdir, node: a, parent: b, name: "a"},
			&op{kind: kindMkdir, node: c, name: "c"},
			&op{kind: kindRemove, node: c},
			&op{kind: kindPurge, node: c, meta: []uint64{1}, done: true})
	}()
	select {
	case err := <-done:
		wantErr(t, err, ErrCorrupt)
	case <-time.After(10 * time.Second):
		t.Fatal("the walk up a cycle of parents does not stop")
	}
}

// A node that is deeper than MaxDepth and not reachable from the root is no change in a diff.
func TestDiffUnreachableDeepNodes(t *testing.T) {
	n1, n2, top, mid, low, missing := NodeID{1}, NodeID{2}, NodeID{3}, NodeID{4}, NodeID{5}, NodeID{6}
	e := newEnv(t)
	entries := [][]byte{encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()})}
	for _, o := range []*op{
		{kind: kindMkdir, node: n1, parent: missing, name: "a"},
		{kind: kindMkdir, node: n2, parent: n1, name: "b"},
		{kind: kindMkdir, node: top, name: "t"},
		{kind: kindMkdir, node: mid, parent: top, name: "m"},
		{kind: kindMkdir, node: low, parent: mid, name: "l"},
		{kind: kindMove, node: top, parent: n2, name: "t"}, // m and l are deeper than 3 below a missing node
	} {
		entries = append(entries, encodeOp(o))
	}
	appendRaw(t, e.meta, entries...)
	v, err := Open(e.meta, e.content, WithMaxDepth(3))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	got, err := v.Diff(1, v.Len())
	if err != nil || len(got) != 0 {
		t.Fatalf("diff of nodes not reachable: %+v, %v", got, err)
	}
}

// A PURGE may list a file whose parent MKDIR was destroyed before the open: the walk from the file reaches the removed directory.
func TestPurgeListsNodeBelowDestroyedParent(t *testing.T) {
	d, f := NodeID{1}, NodeID{2}
	e := newEnv(t)
	appendRaw(t, e.meta,
		encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()}),
		encodeOp(&op{kind: kindMkdir, node: d, name: "d"}),
		encodeOp(&op{kind: kindCreate, node: f, parent: d, name: "f", ver: smallVersion()}),
		encodeOp(&op{kind: kindRemove, node: d}),
		encodeOp(&op{kind: kindPurge, node: d, meta: []uint64{1, 2}, done: true}))
	if err := e.meta.Delete(1); err != nil {
		t.Fatal(err)
	}
	v, err := Open(e.meta, e.content)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
}

// A diff finds whether a node is reachable by walking its parents, which a hostile log can chain far deeper than
// MaxDepth: every chain is walked once, so the cost grows with the nodes, not with the nodes times their depth.
func TestReachWalksEachChainOnce(t *testing.T) {
	const n = 20_000
	st := newState()
	for i := range n {
		parent := rootID
		if i > 0 {
			parent = benchID(i - 1)
		}
		st.nodes[benchID(i)] = &node{parent: parent, name: "d", dir: true}
		missing := benchID(n + i)
		if i > 0 {
			missing = benchID(n + i - 1)
		}
		st.nodes[benchID(n+i)] = &node{parent: missing, name: "d", dir: true}
	}
	st.nodes[benchID(n)].parent = NodeID{0xff}
	start := time.Now()
	r := newReach(st)
	for i := range n {
		if !r.reachable(benchID(i)) {
			t.Fatalf("node %d of the chain to the root is not reachable", i)
		}
		if r.reachable(benchID(n + i)) {
			t.Fatalf("node %d of the chain to a missing node is reachable", i)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("%d nodes took %v", 2*n, d)
	}
}
