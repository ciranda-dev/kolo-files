package kolofiles

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"

	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
)

func deletedEntries(t testing.TB, l interface {
	Len() uint64
	EntryInfo(uint64) (kololog.EntryInfo, error)
}) []uint64 {
	t.Helper()
	var out []uint64
	for i := range l.Len() {
		info, err := l.EntryInfo(i)
		if err != nil {
			t.Fatal(err)
		}
		if info.Deleted {
			out = append(out, i)
		}
	}
	return out
}

func opsOf(t testing.TB, l *kolostore.Log) map[uint64]*op {
	t.Helper()
	out := map[uint64]*op{}
	for i := range l.Len() {
		b, err := l.Get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		o, err := decodeOp(b, DefaultMaxNameLength, false)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = o
	}
	return out
}

func TestDeleteFile(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "keep", randBytes(1, 2<<20))
	keepContent := e.content.Len()
	write(t, v, "secret-name", randBytes(2, 3<<20))
	write(t, v, "secret-name", append(randBytes(2, 3<<20), "more"...))
	old, err := v.Snapshot(v.Len())
	if err != nil {
		t.Fatal(err)
	}
	created := uint64(2)
	del(t, v, "secret-name")
	if _, err := v.Stat("secret-name"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after Delete: %v", err)
	}
	// The CREATE and both WRITEs are destroyed, and every content entry of the file and its tables.
	if got := deletedEntries(t, e.meta); !slices.Equal(got, []uint64{2, 3}) {
		t.Fatalf("destroyed metadata entries %v", got)
	}
	var want []uint64
	for i := keepContent; i < e.content.Len(); i++ {
		want = append(want, i)
	}
	if got := deletedEntries(t, e.content); !slices.Equal(got, want) {
		t.Fatalf("destroyed content entries %v, want %v", got, want)
	}
	// The key is gone: an older snapshot cannot read the file.
	_, err = old.Open("secret-name")
	wantErr(t, err, kololog.ErrDeleted)
	wantErr(t, err, fs.ErrNotExist)
	if _, err := e.meta.Get(created); !errors.Is(err, kololog.ErrDeleted) {
		t.Fatal("the create op is readable")
	}
	// Deletion wins: no snapshot shows the file.
	for ver := uint64(1); ver <= v.Len(); ver++ {
		s, err := v.Snapshot(ver)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stat("secret-name"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("version %d shows the deleted file", ver)
		}
	}
	// REMOVE and PURGE hold no name; the other file is intact.
	for i, o := range opsOf(t, e.meta) {
		b, _ := e.meta.Get(i)
		if (o.kind == kindRemove || o.kind == kindPurge) && bytes.Contains(b, []byte("secret")) {
			t.Fatal("a remove or purge op holds the name")
		}
	}
	if !bytes.Equal(read(t, v, "keep"), randBytes(1, 2<<20)) {
		t.Fatal("the other file changed")
	}
	// The name can be used again, and the volume reopens.
	write(t, v, "secret-name", []byte("new"))
	v = e.reopen(t, v)
	if string(read(t, v, "secret-name")) != "new" {
		t.Fatal("reuse after delete")
	}
}

func TestDeleteTree(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	mkdir(t, v, "d/e")
	mkdir(t, v, "d/e/f")
	mkdir(t, v, "d/empty")
	write(t, v, "d/e/f/x", []byte("x"))
	write(t, v, "d/y", []byte("y"))
	write(t, v, "d/e/out", []byte("out")) // moved out before the delete
	move(t, v, "d/e/out", "out")
	mkdir(t, v, "other")
	move(t, v, "other", "d/e/in") // moved in, deleted with d
	beforeMove := v.Len() - 3     // d/e/out exists, inside d
	del(t, v, "d")
	if got := tree(t, v); !slices.Equal(got, []string{"./", "out=out"}) {
		t.Fatalf("tree %q", got)
	}
	// In an old snapshot, the node moved out lives under a destroyed directory, so it is hidden.
	s, err := v.Snapshot(beforeMove)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, s); !slices.Equal(got, []string{"./"}) {
		t.Fatalf("old snapshot %q", got)
	}
	s, err = v.Snapshot(beforeMove + 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, s); !slices.Equal(got, []string{"./", "out=out"}) {
		t.Fatalf("snapshot after the move out %q", got)
	}
	// Every op about d's nodes is destroyed; the moved-out file's ops and the REMOVE and PURGE stay.
	ops := opsOf(t, e.meta)
	for _, o := range ops {
		if o.kind == kindMkdir && o.name != "" {
			t.Fatalf("mkdir %q left", o.name)
		}
	}
	v = e.reopen(t, v)
	if got := tree(t, v); !slices.Equal(got, []string{"./", "out=out"}) {
		t.Fatalf("tree after reopen %q", got)
	}
	// Deleting an empty directory and a file inside a directory.
	mkdir(t, v, "e")
	del(t, v, "e")
	mkdir(t, v, "f")
	write(t, v, "f/g", []byte("g"))
	del(t, v, "f/g")
	if got := tree(t, v); !slices.Equal(got, []string{"./", "f/", "out=out"}) {
		t.Fatalf("tree %q", got)
	}
}

func TestDeleteManyPurgeOps(t *testing.T) {
	defer func(n int) { purgeItems = n }(purgeItems)
	purgeItems = 3
	v, e := newVolume(t)
	mkdir(t, v, "d")
	for _, n := range []string{"a", "b", "c", "e"} {
		write(t, v, "d/"+n, []byte(n))
	}
	n := v.Len()
	del(t, v, "d")
	purges := 0
	var listed []uint64
	for i := n + 1; i < v.Len(); i++ {
		o := opsOf(t, e.meta)[i]
		if o.kind != kindPurge || len(o.meta)+len(o.objs) > 3 || o.done != (i == v.Len()-1) {
			t.Fatalf("entry %d: kind %d, %d items, done %v", i, o.kind, len(o.meta)+len(o.objs), o.done)
		}
		listed = append(listed, o.meta...)
		purges++
	}
	if purges < 3 || len(listed) != 5 {
		t.Fatalf("%d purge ops listing %v", purges, listed)
	}
	if len(deletedEntries(t, e.content)) != int(e.content.Len()) {
		t.Fatal("content entries left")
	}
	e.reopen(t, v)
}

func TestReaderSeesDelete(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	write(t, v, "d/f", []byte("f"))
	write(t, v, "g", []byte("g"))
	r := newReader(t, e, false)
	have := r.v.Len()
	if have != v.Len() || !slices.Equal(tree(t, r.v), tree(t, v)) {
		t.Fatal("reader differs")
	}
	del(t, v, "d")
	// Markers before the purge op: the reader goes back to just before the first destroyed entry.
	applyMarkers(t, r.meta, e.meta)
	if err := r.v.Refresh(); err != nil {
		t.Fatal(err)
	}
	if r.v.Len() != 1 {
		t.Fatalf("reader Len %d, want 1 (before the destroyed mkdir)", r.v.Len())
	}
	if got := tree(t, r.v); !slices.Equal(got, []string{"./"}) {
		t.Fatalf("reader tree %q", got)
	}
	// Then the remove and purge ops arrive.
	r.sync(t, e)
	if r.v.Len() != v.Len() || !slices.Equal(tree(t, r.v), tree(t, v)) {
		t.Fatalf("reader at %d of %d: %q", r.v.Len(), v.Len(), tree(t, r.v))
	}
	// Ops before the markers: the purge applies while the entries are still readable.
	write(t, v, "h", []byte("h"))
	r.sync(t, e)
	mkdir(t, v, "k")
	write(t, v, "k/x", []byte("x"))
	del(t, v, "k")
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	if err := r.v.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tree(t, r.v), tree(t, v)) || r.v.Len() != v.Len() {
		t.Fatalf("reader before markers: %q", tree(t, r.v))
	}
	applyMarkers(t, r.meta, e.meta)
	if err := r.v.Refresh(); err != nil || r.v.Len() != v.Len() {
		t.Fatalf("reader after markers: %d, %v", r.v.Len(), err)
	}
}

var errCrash = errors.New("crash")

// crashAfter makes a delete end, as a crash would, after the nth time it reaches step; it returns a function that
// removes the hook and says how many times the step was reached.
func crashAfter(step string, nth int) func() int {
	count := 0
	testHook = func(s string) error {
		if s != step {
			return nil
		}
		if count++; count == nth {
			return errCrash
		}
		return nil
	}
	return func() int { testHook = nil; return count }
}

// crashScenario is a volume with a file to keep and a folder of six files to delete, with purgeItems lowered so
// that the delete needs several PURGE ops.
func crashScenario(t *testing.T) (*Volume, *env, uint64) {
	v, e := newVolume(t)
	write(t, v, "keep", []byte("keep"))
	keepContent := e.content.Len()
	mkdir(t, v, "d")
	for _, n := range []string{"a", "b", "c", "e", "f"} {
		write(t, v, "d/"+n, []byte("content of "+n))
	}
	return v, e, keepContent
}

// checkFinished checks the logs of a finished delete of the folder of crashScenario: every entry of it destroyed,
// the PURGE ops in a row ending with the one that is done, and a reader that follows both logs seeing the tree.
func checkFinished(t *testing.T, e *env, keepContent uint64) {
	t.Helper()
	if got := deletedEntries(t, e.meta); !slices.Equal(got, []uint64{2, 3, 4, 5, 6, 7}) {
		t.Fatalf("destroyed metadata entries %v", got)
	}
	var want []uint64
	for i := keepContent; i < e.content.Len(); i++ {
		want = append(want, i)
	}
	if got := deletedEntries(t, e.content); !slices.Equal(got, want) {
		t.Fatalf("destroyed content entries %v, want %v", got, want)
	}
	var purges []*op
	ops := opsOf(t, e.meta)
	for i := range e.meta.Len() {
		if o := ops[i]; o != nil && o.kind == kindPurge {
			purges = append(purges, o)
		}
	}
	if len(purges) == 0 || !purges[len(purges)-1].done {
		t.Fatal("the last PURGE op is not done")
	}
	for _, o := range purges[:len(purges)-1] {
		if o.done {
			t.Fatal("a PURGE op before the last is done")
		}
	}
	if o := ops[e.meta.Len()-1]; o.kind != kindPurge || !o.done {
		t.Fatal("the log does not end with the done PURGE op")
	}
	r := newReader(t, e, false)
	r.sync(t, e)
	if got := tree(t, r.v); !slices.Equal(got, []string{"./", "keep=keep"}) || r.v.Len() != e.meta.Len() {
		t.Fatalf("reader after the delete: %q at %d of %d", got, r.v.Len(), e.meta.Len())
	}
}

// A delete that stops after any of its steps, counted in a run that does not stop, is finished by the next Open.
func TestDeleteCrash(t *testing.T) {
	defer func(n int) { purgeItems = n }(purgeItems)
	purgeItems = 3
	steps := map[string]int{}
	testHook = func(s string) error { steps[s]++; return nil }
	v, _, _ := crashScenario(t)
	if err := v.Delete("d"); err != nil {
		t.Fatal(err)
	}
	testHook = nil
	if steps["remove"] != 1 || steps["purge"] < 4 || steps["destroy"] != 1 {
		t.Fatalf("steps of an uncrashed delete: %v", steps)
	}
	for _, step := range []string{"remove", "purge", "destroy"} {
		for nth := 1; nth <= steps[step]; nth++ {
			t.Run(fmt.Sprintf("%s %d", step, nth), func(t *testing.T) {
				v, e, keepContent := crashScenario(t)
				stop := crashAfter(step, nth)
				err := v.Delete("d")
				if n := stop(); n != nth {
					t.Fatalf("step %q reached %d times, want %d: the case does not exercise a crash", step, n, nth)
				}
				wantErr(t, err, errCrash)
				// A reader of the logs as the crash left them sees the tree without the folder.
				r := newReader(t, e, false)
				if got := tree(t, r.v); !slices.Equal(got, []string{"./", "keep=keep"}) {
					t.Fatalf("reader after the crash %q", got)
				}
				if got := tree(t, v); !slices.Equal(got, []string{"./", "keep=keep"}) {
					t.Fatalf("tree after the crash %q", got)
				}
				v = e.reopen(t, v)
				if got := tree(t, v); !slices.Equal(got, []string{"./", "keep=keep"}) {
					t.Fatalf("tree after reopen %q", got)
				}
				checkFinished(t, e, keepContent)
				ops := e.meta.Len()
				v = e.reopen(t, v)
				if e.meta.Len() != ops {
					t.Fatalf("a second open appended %d ops", e.meta.Len()-ops)
				}
				write(t, v, "d", []byte("again"))
			})
		}
	}
}

// An Open that crashes while it finishes a delete is finished by the next Open.
func TestDeleteCrashInResume(t *testing.T) {
	defer func(n int) { purgeItems = n }(purgeItems)
	purgeItems = 3
	for _, c := range []struct {
		step string
		nth  int
	}{{"purge", 1}, {"purge", 2}, {"purge", 3}, {"destroy", 1}} {
		t.Run(fmt.Sprintf("%s %d", c.step, c.nth), func(t *testing.T) {
			v, e, keepContent := crashScenario(t)
			stop := crashAfter("remove", 1)
			wantErr(t, v.Delete("d"), errCrash)
			stop()
			metaID, contentID := e.meta.ID(), e.content.ID()
			e.reopenLogs(t, v)
			stop = crashAfter(c.step, c.nth)
			_, err := Open(e.meta, e.content)
			if n := stop(); n != c.nth {
				t.Fatalf("step %q reached %d times, want %d", c.step, n, c.nth)
			}
			wantErr(t, err, errCrash)
			// The logs stay open when Open fails; a reader of them sees the tree without the folder.
			r := newReader(t, e, false)
			if got := tree(t, r.v); !slices.Equal(got, []string{"./", "keep=keep"}) {
				t.Fatalf("reader after the crash %q", got)
			}
			e.meta.Close()
			e.content.Close()
			e.openLogs(t, metaID, contentID)
			v, err = Open(e.meta, e.content)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if got := tree(t, v); !slices.Equal(got, []string{"./", "keep=keep"}) {
				t.Fatalf("tree after the second open %q", got)
			}
			checkFinished(t, e, keepContent)
		})
	}
}

// A reader opened on the logs of a delete that was never finished sees the tree without the node, and is not
// confused by the entries still readable.
func TestReaderOfUnfinishedDelete(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	write(t, v, "d/f", []byte("f"))
	write(t, v, "g", []byte("g"))
	stop := crashAfter("remove", 1)
	wantErr(t, v.Delete("d"), errCrash)
	stop()
	r := newReader(t, e, false)
	if got := tree(t, r.v); !slices.Equal(got, []string{"./", "g=g"}) {
		t.Fatalf("reader tree %q", got)
	}
	e.reopen(t, v)
	r.sync(t, e)
	if got := tree(t, r.v); !slices.Equal(got, []string{"./", "g=g"}) || r.v.Len() != e.meta.Len() {
		t.Fatalf("reader tree %q at %d of %d", got, r.v.Len(), e.meta.Len())
	}
}

// A complete PURGE sequence that lists a content entry of another file is not carried out by Open: the entry stays.
// One that lists the entries of the removed file is.
func TestResumeChecksListedObjects(t *testing.T) {
	for _, own := range []bool{false, true} {
		v, e := newVolume(t)
		write(t, v, "keep", randBytes(1, 1<<20))
		keep := e.content.Len()
		mkdir(t, v, "d")
		write(t, v, "d/f", []byte("f"))
		d := nodeOf(t, v, "d").ID
		objs := []uint64{0}
		if own {
			objs = nil
			for i := keep; i < e.content.Len(); i++ {
				objs = append(objs, i)
			}
		}
		e.reopenLogs(t, v)
		appendRaw(t, e.meta,
			encodeOp(&op{kind: kindRemove, node: d}),
			encodeOp(&op{kind: kindPurge, node: d, meta: []uint64{2, 3}, objs: objs, done: true}))
		w, err := Open(e.meta, e.content)
		if !own {
			wantErr(t, err, ErrCorrupt)
			wantPrefix(t, err)
			if got := deletedEntries(t, e.content); len(got) != 0 {
				t.Fatalf("destroyed content entries %v", got)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if got := deletedEntries(t, e.content); len(got) != int(e.content.Len()-keep) || got[0] != keep {
			t.Fatalf("destroyed content entries %v", got)
		}
	}
}

// An unfinished PURGE sequence that lists entries which are not the removed nodes' is not carried out by Open: the
// entries stay. One that lists the removed nodes' entries is finished.
func TestResumeChecksUnfinishedSequence(t *testing.T) {
	for _, own := range []bool{false, true} {
		v, e := newVolume(t)
		write(t, v, "keep", randBytes(1, 1<<20))
		keep := e.content.Len()
		mkdir(t, v, "d")
		write(t, v, "d/f", []byte("f"))
		d := nodeOf(t, v, "d").ID
		objs := []uint64{0}
		if own {
			objs = nil
			for i := keep; i < e.content.Len(); i++ {
				objs = append(objs, i)
			}
		}
		e.reopenLogs(t, v)
		appendRaw(t, e.meta,
			encodeOp(&op{kind: kindRemove, node: d}),
			encodeOp(&op{kind: kindPurge, node: d, meta: []uint64{2, 3}, objs: objs}))
		w, err := Open(e.meta, e.content)
		if !own {
			wantErr(t, err, ErrCorrupt)
			wantPrefix(t, err)
			if got := deletedEntries(t, e.content); len(got) != 0 {
				t.Fatalf("destroyed content entries %v", got)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if got := deletedEntries(t, e.content); len(got) != int(e.content.Len()-keep) || got[0] != keep {
			t.Fatalf("destroyed content entries %v", got)
		}
		if got := deletedEntries(t, e.meta); !slices.Equal(got, []uint64{2, 3}) {
			t.Fatalf("destroyed metadata entries %v", got)
		}
	}
}

// An unfinished PURGE that lists the CREATE of a node whose chain breaks off, and that a later MOVE brings to the
// root, does not let Open destroy that CREATE.
func TestResumeKeepsMovedOrphan(t *testing.T) {
	v, e := newVolume(t)
	d, z, missing := NodeID{1}, NodeID{2}, NodeID{3}
	e.reopenLogs(t, v)
	appendRaw(t, e.meta,
		encodeOp(&op{kind: kindMkdir, node: d, name: "d"}),
		encodeOp(fileOp(z, missing, "z")),
		encodeOp(&op{kind: kindRemove, node: d}),
		encodeOp(&op{kind: kindPurge, node: d, meta: []uint64{1, 2}}),
		encodeOp(&op{kind: kindMove, node: z, name: "z"}))
	_, err := Open(e.meta, e.content)
	wantErr(t, err, ErrCorrupt)
	wantPrefix(t, err)
	if got := deletedEntries(t, e.meta); len(got) != 0 {
		t.Fatalf("destroyed metadata entries %v", got)
	}
}

// wantIndex fails unless the writer's index is exactly the entries of the readable MKDIR, CREATE, WRITE and MOVE
// ops of v's metadata log that no complete PURGE sequence lists, grouped by node.
func wantIndex(t testing.TB, v *Volume, l *kolostore.Log) {
	t.Helper()
	want := map[NodeID][]uint64{}
	for i, o := range opsOf(t, l) {
		if _, listed := slices.BinarySearch(v.skip, i); !listed && o.kind >= kindMkdir && o.kind <= kindMove {
			want[o.node] = append(want[o.node], i)
		}
	}
	for _, list := range want {
		slices.Sort(list)
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.byNode) != len(want) {
		t.Fatalf("index has %d nodes, want %d", len(v.byNode), len(want))
	}
	for id, list := range want {
		if !slices.Equal(v.byNode[id], list) {
			t.Fatalf("index of %v is %v, want %v", id, v.byNode[id], list)
		}
	}
}

func TestEntryIndex(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	mkdir(t, v, "d/sub")
	write(t, v, "d/a", []byte("1"))
	write(t, v, "d/a", []byte("2"))
	write(t, v, "d/sub/b", []byte("3"))
	write(t, v, "keep", []byte("4"))
	move(t, v, "d/a", "d/c")
	wantIndex(t, v, e.meta)
	del(t, v, "d/sub")
	wantIndex(t, v, e.meta)
	write(t, v, "d/after", []byte("5"))
	del(t, v, "keep")
	wantIndex(t, v, e.meta)
	v = e.reopen(t, v)
	wantIndex(t, v, e.meta)
	del(t, v, "d")
	wantIndex(t, v, e.meta)
	v = e.reopen(t, v)
	wantIndex(t, v, e.meta)
	if len(v.byNode) != 0 {
		t.Fatalf("index holds %d nodes of an empty volume", len(v.byNode))
	}
}

// Deleting a file reads that file's entries, not the whole metadata log: the cost does not grow with the volume.
func TestDeleteCostDoesNotGrowWithTheVolume(t *testing.T) {
	cost := func(files int) uint64 {
		v, _ := newVolume(t)
		for i := range files {
			write(t, v, fmt.Sprintf("f%04d", i), []byte("x"))
		}
		return allocated(func() { del(t, v, "f0001") })
	}
	small, large := cost(20), cost(600)
	if large > 3*small {
		t.Fatalf("a delete allocated %d bytes in a volume of 600 files and %d in one of 20", large, small)
	}
}
