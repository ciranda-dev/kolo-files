package kolofiles

import (
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMove(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "a")
	mkdir(t, v, "b")
	write(t, v, "a/f", []byte("f"))
	write(t, v, "a/README", []byte("r"))
	id := nodeOf(t, v, "a/f").ID

	move(t, v, "a/f", "b/g")
	if got := tree(t, v); !slices.Equal(got, []string{"./", "a/", "a/README=r", "b/", "b/g=f"}) {
		t.Fatalf("tree after file move %q", got)
	}
	if nodeOf(t, v, "b/g").ID != id {
		t.Fatal("a move changed the node ID")
	}
	// A case-only rename is an ordinary move, and names differing in case coexist.
	move(t, v, "a/README", "a/readme")
	write(t, v, "a/README", []byte("R"))
	if got := tree(t, v); !slices.Equal(got, []string{"./", "a/", "a/README=R", "a/readme=r", "b/", "b/g=f"}) {
		t.Fatalf("tree after case rename %q", got)
	}
	// A directory moves with everything below it, in one op.
	n := v.Len()
	move(t, v, "a", "b/a2")
	if v.Len() != n+1 {
		t.Fatalf("a directory move appended %d ops", v.Len()-n)
	}
	if got := tree(t, v); !slices.Equal(got, []string{"./", "b/", "b/a2/", "b/a2/README=R", "b/a2/readme=r", "b/g=f"}) {
		t.Fatalf("tree after dir move %q", got)
	}
	// A move to the same path does nothing.
	n = v.Len()
	move(t, v, "b/g", "b/g")
	if v.Len() != n {
		t.Fatal("a move to the same path appended an op")
	}
	// Refusals.
	for _, c := range []struct {
		from, to string
		want     error
	}{
		{"b/g", "b/a2/readme", fs.ErrExist},
		{"b", "b/a2/x", fs.ErrInvalid},
		{"b", "b/x", fs.ErrInvalid},
		{".", "x", fs.ErrInvalid},
		{"b/g", ".", fs.ErrExist},
		{"nope", "x", fs.ErrNotExist},
		{"b/g", "nope/x", fs.ErrNotExist},
		{"b/g", "b/g/x", ErrNotDir},
		{"b/g", "b/../x", fs.ErrInvalid},
	} {
		err := v.Move(c.from, c.to)
		wantErr(t, err, c.want)
		if _, ok := err.(*fs.PathError); !ok {
			t.Fatalf("Move(%q, %q) = %T, want *fs.PathError", c.from, c.to, err)
		}
	}
	// A chain of moves, then the old parent deleted: the moved file survives.
	mkdir(t, v, "c")
	move(t, v, "b/g", "c/g")
	move(t, v, "c/g", "c/h")
	move(t, v, "c/h", "h")
	del(t, v, "c")
	if got := read(t, v, "h"); string(got) != "f" {
		t.Fatalf("read %q", got)
	}
	v = e.reopen(t, v)
	if got := tree(t, v); !slices.Equal(got, []string{"./", "b/", "b/a2/", "b/a2/README=R", "b/a2/readme=r", "h=f"}) {
		t.Fatalf("tree after reopen %q", got)
	}
}

// A node moved out of a directory that is later deleted is hidden in the snapshots before the move, whose parent's
// ops are destroyed, and visible after it.
func TestMoveOutOfDeletedDir(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	write(t, v, "d/f", []byte("f"))
	before := v.Len()
	move(t, v, "d/f", "f")
	after := v.Len()
	del(t, v, "d")
	check := func(v *Volume) {
		t.Helper()
		s, err := v.Snapshot(before)
		if err != nil {
			t.Fatal(err)
		}
		if got := tree(t, s); !slices.Equal(got, []string{"./"}) {
			t.Fatalf("snapshot before the move %q", got)
		}
		if s, err = v.Snapshot(after); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, s); !slices.Equal(got, []string{"./", "f=f"}) {
			t.Fatalf("snapshot after the move %q", got)
		}
	}
	check(v)
	check(e.reopen(t, v))
}

// deepPath returns the path of depth names "d".
func deepPath(depth int) string { return strings.TrimSuffix(strings.Repeat("d/", depth), "/") }

func TestDeepFolders(t *testing.T) {
	if testing.Short() {
		t.Skip("deep tree")
	}
	v, e := newVolume(t)
	for d := 1; d <= DefaultMaxDepth; d++ {
		mkdir(t, v, deepPath(d))
	}
	wantErr(t, v.Mkdir(deepPath(DefaultMaxDepth+1)), ErrLimit)
	wantErr(t, v.WriteFile(testCtx(t), deepPath(DefaultMaxDepth)+"/f", strings.NewReader("x")), ErrLimit)
	write(t, v, deepPath(DefaultMaxDepth-1)+"/f", []byte("deep"))
	if got := read(t, v, deepPath(DefaultMaxDepth-1)+"/f"); string(got) != "deep" {
		t.Fatalf("read %q", got)
	}
	if ents, err := v.ReadDir(deepPath(DefaultMaxDepth - 1)); err != nil || len(ents) != 2 {
		t.Fatalf("ReadDir = %v, %v", ents, err)
	}
	if got := len(tree(t, v)); got != DefaultMaxDepth+2 {
		t.Fatalf("walk found %d entries", got)
	}
	// The deepest directory moves up; moving a subtree down by one level would go over the bound.
	mkdir(t, v, "e")
	wantErr(t, v.Move("d", "e/d"), ErrLimit)
	move(t, v, deepPath(DefaultMaxDepth), "top")
	move(t, v, "top", deepPath(DefaultMaxDepth-1)+"/back")
	v = e.reopen(t, v)
	if _, err := v.Stat(deepPath(DefaultMaxDepth-1) + "/back"); err != nil {
		t.Fatal(err)
	}
	del(t, v, "d")
	if got := tree(t, v); !slices.Equal(got, []string{"./", "e/"}) {
		t.Fatalf("tree after delete %q", got)
	}
	e.reopen(t, v)
}

func TestMoveDepthBound(t *testing.T) {
	v, e := newVolume(t, WithMaxDepth(4))
	mkdir(t, v, "a")
	mkdir(t, v, "a/b")
	mkdir(t, v, "a/b/c")
	mkdir(t, v, "x")
	mkdir(t, v, "x/y")
	// a has height 2, so under x/y it would reach depth 5.
	wantErr(t, v.Move("a", "x/y/a"), ErrLimit)
	move(t, v, "a", "x/a")
	if _, err := v.Stat("x/a/b/c"); err != nil {
		t.Fatal(err)
	}
	// Replay checks the bound only for the node each op creates or moves, so with a smaller bound the log opens and
	// the nodes a move took deeper are reached by no path.
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	meta, err := e.st.Open(e.id, e.meta.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	content, err := e.st.Open(e.id, e.content.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	small, err := Open(meta, content, WithMaxDepth(3))
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	if _, err := small.ReadDir("x/a/b"); err != nil {
		t.Fatal(err)
	}
	_, err = small.Stat("x/a/b/c")
	wantErr(t, err, ErrLimit)
}

// A hostile log of many moves of a node between the two deepest levels opens in time linear in the moves.
func TestHostileDeepMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("long log")
	}
	moves := 20000
	if raceEnabled {
		moves = 2000
	}
	e := newEnv(t)
	ops := [][]byte{encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()})}
	var dirs []NodeID
	parent := rootID
	for d := 0; d < DefaultMaxDepth-1; d++ {
		id := NodeID{1, byte(d >> 8), byte(d)}
		ops = append(ops, encodeOp(&op{kind: kindMkdir, node: id, parent: parent, name: "d"}))
		dirs = append(dirs, id)
		parent = id
	}
	f := NodeID{2}
	ops = append(ops, encodeOp(&op{kind: kindMkdir, node: f, parent: parent, name: "f"}))
	for k := range moves {
		ops = append(ops, encodeOp(&op{kind: kindMove, node: f, parent: dirs[len(dirs)-1-k%2], name: "f"}))
	}
	appendRaw(t, e.meta, ops...)
	start := time.Now()
	v, err := Open(e.meta, e.content)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	took := time.Since(start)
	t.Logf("opened %d ops in %v", len(ops), took)
	// The walk of 2048 ancestors costs about 30 us; allow far more per move so a slow machine passes.
	if limit := time.Duration(len(ops)) * time.Millisecond; took > limit {
		t.Fatalf("opening %d ops took %v, over %v", len(ops), took, limit)
	}
	if _, err := v.Stat(deepPath(DefaultMaxDepth-2+moves%2) + "/f"); err != nil {
		t.Fatal(err)
	}
}
