package kolofiles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"sync/atomic"
	"testing"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// switchable is a content backend that can be taken offline.
type switchable struct {
	b   koloblobs.Backend
	off atomic.Bool
}

var errOffline = errors.New("offline")

func (s *switchable) Get(ctx context.Context, h koloblobs.Hash) ([]byte, error) {
	if s.off.Load() {
		return nil, errOffline
	}
	return s.b.Get(ctx, h)
}

func (s *switchable) Put(context.Context, koloblobs.Hash, []byte) error { return koloblobs.ErrReadOnly }

// offlineReader is a reader with replicas of both logs, a local backend and a content backend that can go offline.
func offlineReader(t testing.TB, e *env) (*reader, *switchable) {
	t.Helper()
	r := &reader{st: newStore(t), local: kolostore.NewMemory()}
	t.Cleanup(func() { r.local.Close() })
	r.meta = newReplica(t, r.st, e.meta)
	r.content = newReplica(t, r.st, e.content)
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	sw := &switchable{b: koloblobs.NewLogBackend(r.content)}
	v, err := OpenReader(r.meta, sw, r.local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	r.v = v
	return r, sw
}

func keptObjects(t testing.TB, r *reader) map[NodeID][]koloblobs.Hash {
	t.Helper()
	start := r.v.localPrefix(localObject)
	out := map[NodeID][]koloblobs.Hash{}
	err := r.local.Scan(start, upper(start), false, 0, func(k, _ []byte) bool {
		var id NodeID
		var h koloblobs.Hash
		copy(id[:], k[len(start):])
		copy(h[:], k[len(start)+len(id)+8:])
		out[id] = append(out[id], h)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// blobObjects returns the distinct objects of a file's current version, sorted.
func blobObjects(t testing.TB, e *env, v *Volume, name string) []koloblobs.Hash {
	t.Helper()
	info := nodeOf(t, v, name)
	v.mu.RLock()
	created := v.st.nodes[info.ID].created
	v.mu.RUnlock()
	key := fileKey(t, e.meta, created)
	var out []koloblobs.Hash
	for h, err := range koloblobs.New(koloblobs.NewLogBackend(e.content)).Objects(testCtx(t), info.Blob, key) {
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b koloblobs.Hash) int { return bytes.Compare(a[:], b[:]) })
	return out
}

func TestMarks(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	mkdir(t, v, "d/sub")
	big := randBytes(3, 5<<20)
	write(t, v, "d/f", big)
	write(t, v, "d/sub/g", []byte("g"))
	write(t, v, "h", []byte("h"))
	r, sw := offlineReader(t, e)
	rv := r.v

	if m, err := rv.Mark("d"); err != nil || m != MarkInherit {
		t.Fatalf("Mark = %v, %v", m, err)
	}
	if err := rv.SetMark("d", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	if m, _ := rv.Mark("d"); m != MarkKeepLocal {
		t.Fatalf("Mark(d) = %v", m)
	}
	if m, _ := rv.Mark("d/f"); m != MarkInherit {
		t.Fatalf("Mark(d/f) = %v, want MarkInherit: the mark is on d", m)
	}
	if len(keptObjects(t, r)) != 0 {
		t.Fatal("SetMark fetched objects")
	}
	if err := rv.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	f, g, h := nodeOf(t, rv, "d/f").ID, nodeOf(t, rv, "d/sub/g").ID, nodeOf(t, rv, "h").ID
	kept := keptObjects(t, r)
	if !slices.Equal(kept[f], blobObjects(t, e, v, "d/f")) || !slices.Equal(kept[g], blobObjects(t, e, v, "d/sub/g")) || len(kept) != 2 {
		t.Fatalf("kept %d nodes after Prefetch", len(kept))
	}

	// Offline: MarkKeepLocal files read from the local backend; MarkFetchOnDemand files fail.
	sw.off.Store(true)
	before := rv.Stats()
	if !bytes.Equal(read(t, rv, "d/f"), big) || string(read(t, rv, "d/sub/g")) != "g" {
		t.Fatal("offline read differs")
	}
	if s := rv.Stats(); s.ObjectsLocal <= before.ObjectsLocal || s.ObjectsFetched != before.ObjectsFetched {
		t.Fatalf("offline stats %+v after %+v", s, before)
	}
	if _, err := fs.ReadFile(rv, "h"); !errors.Is(err, errOffline) {
		t.Fatalf("offline read of a MarkFetchOnDemand file: %v", err)
	}
	sw.off.Store(false)
	// A MarkFetchOnDemand file is read but nothing of it is kept.
	read(t, rv, "h")
	if _, ok := keptObjects(t, r)[h]; ok {
		t.Fatal("a MarkFetchOnDemand file was kept")
	}

	// A new version: Prefetch keeps exactly the current version's objects.
	big2 := append(bytes.Clone(big[:1<<20]), randBytes(4, 3<<20)...)
	write(t, v, "d/f", big2)
	r.sync(t, e)
	if err := rv.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := keptObjects(t, r)[f]; !slices.Equal(got, blobObjects(t, e, v, "d/f")) {
		t.Fatalf("kept %d objects, the current version has %d", len(got), len(blobObjects(t, e, v, "d/f")))
	}

	// Marks follow moves, and a mark on a file wins over its directory's.
	move(t, v, "d", "moved")
	r.sync(t, e)
	if m, _ := rv.Mark("moved"); m != MarkKeepLocal {
		t.Fatalf("mark after move = %v", m)
	}
	if err := rv.SetMark("moved/sub/g", MarkFetchOnDemand); err != nil {
		t.Fatal(err)
	}
	if _, ok := keptObjects(t, r)[g]; ok {
		t.Fatal("objects kept after the file's mark became MarkFetchOnDemand")
	}
	move(t, v, "moved/sub/g", "g")
	r.sync(t, e)
	if m, _ := rv.Mark("g"); m != MarkFetchOnDemand {
		t.Fatalf("file mark after move = %v", m)
	}
	// Moving a file out of a MarkKeepLocal directory removes its objects at the next clean.
	move(t, v, "moved/f", "f")
	r.sync(t, e)
	if err := rv.SetMark("g", MarkInherit); err != nil {
		t.Fatal(err)
	}
	if _, ok := keptObjects(t, r)[f]; ok {
		t.Fatal("objects kept for a file no longer under a MarkKeepLocal mark")
	}
	move(t, v, "f", "moved/f")
	r.sync(t, e)
	if err := rv.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}

	// Marks are dropped with the node, and so are kept objects.
	del(t, v, "moved")
	r.sync(t, e)
	if len(rv.marks) != 0 {
		t.Fatalf("marks left after delete: %v", rv.marks)
	}
	if kept := keptObjects(t, r); len(kept) != 0 {
		t.Fatalf("objects kept after delete for %d nodes", len(kept))
	}
	if err := r.local.Scan(nil, nil, false, 0, func(k, _ []byte) bool {
		t.Errorf("local key %x left", k)
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMarkRules(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("f"))
	wantErr(t, v.SetMark("f", MarkKeepLocal), ErrInvalidArgument)
	_, err := v.Mark("f")
	wantErr(t, err, ErrInvalidArgument)
	wantErr(t, v.Prefetch(testCtx(t)), ErrInvalidArgument)
	plain := newReader(t, e, false)
	wantErr(t, plain.v.SetMark("f", MarkKeepLocal), ErrInvalidArgument)
	wantErr(t, plain.v.Prefetch(testCtx(t)), ErrInvalidArgument)

	r := newReader(t, e, true)
	wantErr(t, r.v.SetMark("f", MarkKeepLocal+1), ErrInvalidArgument)
	wantErr(t, r.v.SetMark("nope", MarkKeepLocal), fs.ErrNotExist)
	wantErr(t, r.v.SetMark("../x", MarkKeepLocal), fs.ErrInvalid)
	_, err = r.v.Mark("nope")
	wantErr(t, err, fs.ErrNotExist)
	//lint:ignore SA1012 a nil context is what is tested
	wantErr(t, r.v.Prefetch(nil), ErrInvalidArgument)
	for m, s := range map[Mark]string{MarkInherit: "MarkInherit", MarkFetchOnDemand: "MarkFetchOnDemand", MarkKeepLocal: "MarkKeepLocal", 7: "Mark(7)"} {
		if m.String() != s {
			t.Errorf("%d.String() = %q", m, m.String())
		}
	}

	// Marks persist in the local backend and are read back on open; a malformed one is ErrCorrupt.
	if err := r.v.SetMark(".", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	if err := r.v.Close(); err != nil {
		t.Fatal(err)
	}
	wantErr(t, r.v.SetMark("f", MarkKeepLocal), ErrClosed)
	wantErr(t, r.v.Prefetch(testCtx(t)), ErrClosed)
	_, err = r.v.Mark("f")
	wantErr(t, err, ErrClosed)
	reopen := func() (*Volume, error) {
		meta, err := r.st.OpenReplica(e.meta.Header().LogKey, e.meta.ID(), e.meta.Header().Encode())
		if err != nil {
			t.Fatal(err)
		}
		v, err := OpenReader(meta, koloblobs.NewLogBackend(r.content), r.local)
		if err != nil {
			meta.Close()
		}
		return v, err
	}
	rv, err := reopen()
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := rv.Mark("."); m != MarkKeepLocal {
		t.Fatalf("mark after reopen = %v", m)
	}
	rv.Close()
	for _, bad := range [][]byte{{byte(MarkInherit)}, {byte(MarkKeepLocal + 1)}, {1, 1}} {
		if err := r.local.Put(rv.markKey(rootID, 0), bad); err != nil {
			t.Fatal(err)
		}
		_, err := reopen()
		wantErr(t, err, ErrCorrupt)
	}
	if err := r.local.Put(append(rv.markKey(rootID, 0), 0), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := r.local.Delete(rv.markKey(rootID, 0)); err != nil {
		t.Fatal(err)
	}
	_, err = reopen()
	wantErr(t, err, ErrCorrupt)
}

// A ranged read fetches one node per tree level and the chunks that cover the range, nothing else.
func TestRangedReadFetchesCoveringObjects(t *testing.T) {
	v, e := newVolume(t)
	data := randBytes(5, 12<<20)
	write(t, v, "big", data)
	r := newReader(t, e, false)
	all := len(blobObjects(t, e, v, "big"))
	f, err := r.v.Open("big")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := r.v.Stats().ObjectsFetched
	buf := make([]byte, 10)
	if _, err := f.(io.ReaderAt).ReadAt(buf, 7<<20); err != nil || !bytes.Equal(buf, data[7<<20:7<<20+10]) {
		t.Fatalf("ReadAt: %v", err)
	}
	// Open fetched the root; the read fetches at most one chunk, maybe two at a boundary.
	if got := r.v.Stats().ObjectsFetched - before; got == 0 || got > 2 || all < 8 {
		t.Fatalf("a 10-byte read fetched %d objects of %d", got, all)
	}
}

func TestRefresh(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "a", []byte("a"))
	r := newReader(t, e, false)
	// Incremental: only the new entries are replayed.
	mkdir(t, v, "d")
	write(t, v, "d/b", []byte("b"))
	move(t, v, "a", "d/a")
	before := r.v.Stats().Replayed
	r.sync(t, e)
	if got := r.v.Stats().Replayed - before; got != 3 {
		t.Fatalf("incremental refresh replayed %d entries, want 3", got)
	}
	if !slices.Equal(tree(t, r.v), tree(t, v)) || r.v.Len() != v.Len() {
		t.Fatal("reader differs after refresh")
	}
	// Nothing new: nothing replayed.
	before = r.v.Stats().Replayed
	if err := r.v.Refresh(); err != nil || r.v.Stats().Replayed != before {
		t.Fatalf("idle refresh: %v", err)
	}
	// A delete rebuilds in one pass.
	del(t, v, "d/b")
	before = r.v.Stats().Replayed
	r.sync(t, e)
	if got := r.v.Stats().Replayed - before; got < r.v.Len()-2 {
		t.Fatalf("rebuild replayed %d entries of %d", got, r.v.Len())
	}
	if !slices.Equal(tree(t, r.v), tree(t, v)) || r.v.Len() != v.Len() {
		t.Fatal("reader differs after rebuild")
	}
	// Entries that arrive without their keys stop the prefix there.
	write(t, v, "c", []byte("c"))
	n := r.v.Len()
	grow(t, r.meta, e.meta, false)
	if err := r.v.Refresh(); err != nil || r.v.Len() != n {
		t.Fatalf("refresh without keys: Len %d of %d, %v", r.v.Len(), n, err)
	}
	// A writer's Refresh does nothing; a closed volume's is ErrClosed.
	if err := v.Refresh(); err != nil {
		t.Fatal(err)
	}
	r.v.Close()
	wantErr(t, r.v.Refresh(), ErrClosed)
}

func localKeys(t testing.TB, local kolostore.Backend) [][]byte {
	t.Helper()
	var out [][]byte
	if err := local.Scan(nil, nil, false, 0, func(k, _ []byte) bool {
		out = append(out, bytes.Clone(k))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A node deleted while the device was off loses its mark and kept objects on the next open, though the ops that
// named it are destroyed by then; nodes that are only not replayed yet keep theirs.
func TestMarksAcrossOpens(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "d")
	write(t, v, "d/f", randBytes(6, 3<<20))
	write(t, v, "g", []byte("g"))
	r, _ := offlineReader(t, e)
	for _, name := range []string{"d", "g"} {
		if err := r.v.SetMark(name, MarkKeepLocal); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.v.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	before := localKeys(t, r.local)
	if len(keptObjects(t, r)) != 2 {
		t.Fatalf("kept %d nodes", len(keptObjects(t, r)))
	}
	// A replica whose entries have no keys yet replays nothing: every node is only not replayed yet.
	st := newStore(t)
	late := newReplica(t, st, e.meta)
	grow(t, late, e.meta, false)
	lv, err := OpenReader(late, koloblobs.NewLogBackend(r.content), r.local)
	if err != nil {
		t.Fatal(err)
	}
	if lv.Len() != 0 || len(localKeys(t, r.local)) != len(before) {
		t.Fatalf("Len %d, %d local keys of %d after an open that replays nothing", lv.Len(), len(localKeys(t, r.local)), len(before))
	}
	for i := range late.Len() {
		k, err := e.meta.Key(i)
		if err != nil {
			t.Fatal(err)
		}
		if err := late.AddKey(i, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := lv.Refresh(); err != nil || lv.Len() != e.meta.Len() || len(localKeys(t, r.local)) != len(before) {
		t.Fatalf("after the keys: Len %d, %d local keys, %v", lv.Len(), len(localKeys(t, r.local)), err)
	}
	if m, err := lv.Mark("d"); err != nil || m != MarkKeepLocal {
		t.Fatalf("Mark = %v, %v", m, err)
	}
	lv.Close()

	// The owner deletes d while the device is off; the device opens after the markers arrived.
	del(t, v, "d")
	if err := r.v.Close(); err != nil {
		t.Fatal(err)
	}
	r.meta = newReplica(t, r.st, e.meta)
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	applyMarkers(t, r.meta, e.meta)
	applyMarkers(t, r.content, e.content)
	rv, err := OpenReader(r.meta, koloblobs.NewLogBackend(r.content), r.local)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	if got := tree(t, rv); !slices.Equal(got, []string{"./", "g=g"}) {
		t.Fatalf("tree %q", got)
	}
	gid := nodeOf(t, rv, "g").ID
	for _, k := range localKeys(t, r.local) {
		if !bytes.HasPrefix(k[len(rv.localPrefix(localMark)):], gid[:]) {
			t.Errorf("local key %x left for a node other than g", k)
		}
	}
	if m, _ := rv.Mark("g"); m != MarkKeepLocal || len(keptObjects(t, r)) != 1 {
		t.Fatalf("g lost its mark or objects: %v, %d nodes kept", m, len(keptObjects(t, r)))
	}
}

// tamper is a content backend that changes the objects it serves, or fails them.
type tamper struct {
	b  koloblobs.Backend
	fn func(h koloblobs.Hash, obj []byte) ([]byte, error)
}

func (t tamper) Get(ctx context.Context, h koloblobs.Hash) ([]byte, error) {
	obj, err := t.b.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	return t.fn(h, bytes.Clone(obj))
}

func (tamper) Put(context.Context, koloblobs.Hash, []byte) error { return koloblobs.ErrReadOnly }

// keepReader is a reader with a local backend whose content backend is wrapped by wrap.
func keepReader(t testing.TB, e *env, wrap func(koloblobs.Backend) koloblobs.Backend) *reader {
	t.Helper()
	r := &reader{st: newStore(t), local: kolostore.NewMemory()}
	t.Cleanup(func() { r.local.Close() })
	r.meta = newReplica(t, r.st, e.meta)
	r.content = newReplica(t, r.st, e.content)
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	v, err := OpenReader(r.meta, wrap(koloblobs.NewLogBackend(r.content)), r.local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	r.v = v
	return r
}

// A kept object is checked against its hash before it is kept and before it is served; one that fails is dropped.
func TestKeptObjectsAreChecked(t *testing.T) {
	v, e := newVolume(t)
	big := randBytes(8, 2<<20)
	write(t, v, "f", big)
	flip := func(_ koloblobs.Hash, obj []byte) ([]byte, error) { obj[len(obj)/2] ^= 1; return obj, nil }
	short := func(_ koloblobs.Hash, obj []byte) ([]byte, error) { return obj[:koloblobs.ObjectOverhead-1], nil }
	for name, fn := range map[string]func(koloblobs.Hash, []byte) ([]byte, error){"flipped": flip, "short": short} {
		r := keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend { return tamper{b, fn} })
		if err := r.v.SetMark("f", MarkKeepLocal); err != nil {
			t.Fatal(err)
		}
		err := r.v.Prefetch(testCtx(t))
		wantErr(t, err, koloblobs.ErrCorrupt)
		wantPrefix(t, err)
		if n := len(keptObjects(t, r)); n != 0 {
			t.Fatalf("%s: %d nodes keep objects that do not match their hash", name, n)
		}
	}
	// A kept object that was damaged on the device is dropped and fetched again.
	r, _ := offlineReader(t, e)
	if err := r.v.SetMark("f", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	if err := r.v.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	var key, val []byte
	err := r.local.Scan(r.v.localPrefix(localObject), upper(r.v.localPrefix(localObject)), false, 1, func(k, v []byte) bool {
		key, val = bytes.Clone(k), bytes.Clone(v)
		return false
	})
	if err != nil || key == nil {
		t.Fatalf("no kept object: %v", err)
	}
	damaged := bytes.Clone(val)
	damaged[len(damaged)/2] ^= 1
	if err := r.local.Put(key, damaged); err != nil {
		t.Fatal(err)
	}
	if got := read(t, r.v, "f"); !bytes.Equal(got, big) {
		t.Fatal("read after damage differs")
	}
	if got, err := r.local.Get(key); err != nil || !bytes.Equal(got, val) {
		t.Fatalf("the damaged object was not replaced: %v", err)
	}
	checkKept(t, r.v, r.local)
}

// Prefetch goes on after a file fails and returns every error.
func TestPrefetchContinuesPastErrors(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f1", randBytes(1, 1<<20))
	write(t, v, "f2", randBytes(2, 1<<20))
	// Prefetch visits files by node ID: the first one fails.
	first, second := "f1", "f2"
	if a, b := nodeOf(t, v, "f1").ID, nodeOf(t, v, "f2").ID; bytes.Compare(a[:], b[:]) > 0 {
		first, second = second, first
	}
	bad := map[koloblobs.Hash]bool{}
	for _, h := range blobObjects(t, e, v, first) {
		bad[h] = true
	}
	r := keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend {
		return tamper{b, func(h koloblobs.Hash, obj []byte) ([]byte, error) {
			if bad[h] {
				return nil, errOffline
			}
			return obj, nil
		}}
	})
	if err := r.v.SetMark(".", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	wantErr(t, r.v.Prefetch(testCtx(t)), errOffline)
	id := nodeOf(t, r.v, second).ID
	if got := keptObjects(t, r)[id]; !slices.Equal(got, blobObjects(t, e, v, second)) {
		t.Fatalf("%s keeps %d objects, want %d", second, len(got), len(blobObjects(t, e, v, second)))
	}
}

// An interrupted Prefetch keeps what it fetched, and the next one fetches only the rest.
func TestPrefetchResumes(t *testing.T) {
	v, e := newVolume(t)
	data := randBytes(7, 12<<20)
	write(t, v, "f", data)
	all := blobObjects(t, e, v, "f")
	var gets, budget atomic.Int64
	budget.Store(3)
	r := keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend {
		return tamper{b, func(_ koloblobs.Hash, obj []byte) ([]byte, error) {
			if gets.Add(1) > budget.Load() {
				return nil, errOffline
			}
			return obj, nil
		}}
	})
	if err := r.v.SetMark(".", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	wantErr(t, r.v.Prefetch(testCtx(t)), errOffline)
	id := nodeOf(t, r.v, "f").ID
	have := len(keptObjects(t, r)[id])
	if have == 0 || have >= len(all) {
		t.Fatalf("first Prefetch kept %d of %d objects", have, len(all))
	}

	gets.Store(0)
	budget.Store(1 << 30)
	if err := r.v.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got, want := gets.Load(), int64(len(all)-have); got != want {
		t.Fatalf("second Prefetch fetched %d objects, want %d", got, want)
	}
	if got := keptObjects(t, r)[id]; !slices.Equal(got, all) {
		t.Fatalf("kept %d objects, want %d", len(got), len(all))
	}
	if !bytes.Equal(read(t, r.v, "f"), data) {
		t.Fatal("content differs after the resumed Prefetch")
	}
}

// An in-place Refresh that applies a move drops the kept objects of a file moved out of its KeepLocal directory.
func TestRefreshMoveDropsKeptObjects(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "keep")
	mkdir(t, v, "other")
	write(t, v, "keep/f", randBytes(4, 1<<20))
	r := newReader(t, e, true)
	if err := r.v.SetMark("keep", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	if err := r.v.Prefetch(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if len(keptObjects(t, r)) != 1 {
		t.Fatal("nothing kept")
	}
	move(t, v, "keep/f", "other/f")
	before := r.v.Stats().Replayed
	r.sync(t, e)
	if got := r.v.Stats().Replayed - before; got != 1 {
		t.Fatalf("refresh replayed %d entries: not in place", got)
	}
	if n := len(keptObjects(t, r)); n != 0 {
		t.Fatalf("%d nodes still keep objects after the move out", n)
	}
}

// A local key of a node that lacks the node's create entry belongs to another node and is dropped.
func TestCleanLocalDropsOtherCreateEntry(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("f"))
	r := newReader(t, e, true)
	f := nodeOf(t, r.v, "f").ID
	stale := r.v.markKey(f, 1000)
	if err := r.local.Put(stale, []byte{byte(MarkKeepLocal)}); err != nil {
		t.Fatal(err)
	}
	if err := r.v.cleanLocal(); err != nil {
		t.Fatal(err)
	}
	if got := localKeys(t, r.local); len(got) != 0 {
		t.Fatalf("%d local keys left", len(got))
	}
}

// An object fetched for a file whose mark was cleared or that was removed during the fetch is not kept.
func TestKeptObjectNotStoredAfterChange(t *testing.T) {
	for _, change := range []string{"mark cleared", "file removed"} {
		t.Run(change, func(t *testing.T) {
			v, e := newVolume(t)
			write(t, v, "f", randBytes(3, 1<<20))
			var r *reader
			var changed atomic.Bool
			r = keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend {
				return tamper{b, func(_ koloblobs.Hash, obj []byte) ([]byte, error) {
					if !changed.CompareAndSwap(false, true) {
						return obj, nil
					}
					if change == "mark cleared" {
						return obj, r.v.SetMark("f", MarkInherit)
					}
					del(t, v, "f")
					r.sync(t, e)
					return obj, nil
				}}
			})
			if err := r.v.SetMark("f", MarkKeepLocal); err != nil {
				t.Fatal(err)
			}
			_, _ = fs.ReadFile(r.v, "f")
			if !changed.Load() {
				t.Fatal("nothing changed during a fetch")
			}
			if n := len(keptObjects(t, r)); n != 0 {
				t.Fatalf("%d nodes keep objects of a file that is no longer MarkKeepLocal", n)
			}
			checkKept(t, r.v, r.local)
		})
	}

	t.Run("volume closed", func(t *testing.T) {
		v, e := newVolume(t)
		write(t, v, "f", randBytes(3, 1<<20))
		var r *reader
		var closed atomic.Bool
		r = keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend {
			return tamper{b, func(_ koloblobs.Hash, obj []byte) ([]byte, error) {
				if closed.CompareAndSwap(false, true) {
					return obj, r.v.Close()
				}
				return obj, nil
			}}
		})
		if err := r.v.SetMark("f", MarkKeepLocal); err != nil {
			t.Fatal(err)
		}
		_, _ = fs.ReadFile(r.v, "f")
		if !closed.Load() {
			t.Fatal("the volume was not closed during a fetch")
		}
		if n := len(keptObjects(t, r)); n != 0 {
			t.Fatalf("%d nodes keep objects after the volume closed", n)
		}
	})
}

// keep stores nothing for a file that was replaced (another created entry) or is not in the tree, even if it is marked.
func TestKeepChecksNode(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("f"))
	r := keepReader(t, e, func(b koloblobs.Backend) koloblobs.Backend { return b })
	if err := r.v.SetMark("f", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	n := nodeOf(t, r.v, "f")
	r.v.mu.RLock()
	created := r.v.st.nodes[n.ID].created
	r.v.mu.RUnlock()
	h, obj := koloblobs.Hash{1}, []byte("object")
	stale := &kept{v: r.v, id: n.ID, created: created + 1, prefix: r.v.objectPrefix(n.ID, created+1)}
	if err := stale.keep(append(bytes.Clone(stale.prefix), h[:]...), obj); err != nil {
		t.Fatal(err)
	}
	missing := NodeID{0xee}
	r.v.mu.Lock()
	r.v.marks = map[NodeID]Mark{missing: MarkKeepLocal}
	r.v.mu.Unlock()
	gone := &kept{v: r.v, id: missing, prefix: r.v.objectPrefix(missing, 0)}
	if err := gone.keep(append(bytes.Clone(gone.prefix), h[:]...), obj); err != nil {
		t.Fatal(err)
	}
	if n := len(keptObjects(t, r)); n != 0 {
		t.Fatalf("%d nodes keep objects of a stale or missing node", n)
	}
}
