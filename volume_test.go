package kolofiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
)

func TestWriteReadList(t *testing.T) {
	v, _ := newVolume(t)
	if v.Len() != 1 {
		t.Fatalf("a new volume has %d entries", v.Len())
	}
	mkdir(t, v, "docs")
	mkdir(t, v, "docs/old")
	write(t, v, "docs/a.txt", []byte("alpha"))
	write(t, v, "b", nil)
	big := randBytes(1, 3<<20)
	write(t, v, "docs/old/big", big)
	if got := read(t, v, "docs/a.txt"); string(got) != "alpha" {
		t.Fatalf("read %q", got)
	}
	if got := read(t, v, "b"); len(got) != 0 {
		t.Fatalf("empty file read %d bytes", len(got))
	}
	if !bytes.Equal(read(t, v, "docs/old/big"), big) {
		t.Fatal("big file differs")
	}
	want := []string{"./", "b=", "docs/", "docs/a.txt=alpha", "docs/old/"}
	if got := tree(t, v); !slices.Equal(got[:4], want[:4]) || got[4] != "docs/old/" {
		t.Fatalf("tree = %q", got)
	}
	ents, err := v.ReadDir("docs")
	if err != nil || len(ents) != 2 || ents[0].Name() != "a.txt" || ents[1].Name() != "old" || !ents[1].IsDir() {
		t.Fatalf("ReadDir = %v, %v", ents, err)
	}
	fi, err := v.Stat("docs/a.txt")
	if err != nil || fi.Size() != 5 || fi.Mode() != 0o444 || fi.IsDir() || fi.Name() != "a.txt" || fi.ModTime().IsZero() {
		t.Fatalf("Stat = %v, %v", fi, err)
	}
	if fi, err = v.Stat("docs"); err != nil || fi.Mode() != fs.ModeDir|0o555 || !fi.IsDir() {
		t.Fatalf("Stat dir = %v, %v", fi, err)
	}
	if fi, err = v.Stat("."); err != nil || fi.Name() != "." || !fi.IsDir() {
		t.Fatalf("Stat root = %v, %v", fi, err)
	}
	// An overwrite is a WRITE: same node, new blob, the version moves on.
	before := nodeOf(t, v, "docs/a.txt")
	write(t, v, "docs/a.txt", []byte("alpha two"))
	after := nodeOf(t, v, "docs/a.txt")
	if before.ID != after.ID || before.Blob == after.Blob || after.Version != v.Len() {
		t.Fatalf("overwrite: %+v then %+v at %d", before, after, v.Len())
	}
	if s := v.Stats(); s.Ops != v.Len() || s.BytesWritten != uint64(5+len(big)+9) || s.BytesRead == 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestFSInterfaces(t *testing.T) {
	v, _ := newVolume(t)
	mkdir(t, v, "a")
	mkdir(t, v, "a/b")
	mkdir(t, v, "empty")
	write(t, v, "a/b/c.txt", []byte("hello"))
	write(t, v, "a/x", randBytes(2, 100_000))
	write(t, v, "top", []byte("t"))
	if err := fstest.TestFS(v, "a/b/c.txt", "a/x", "top", "empty"); err != nil {
		t.Fatal(err)
	}
	s, err := v.Snapshot(v.Len() - 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(s, "a/b/c.txt", "a/x", "empty"); err != nil {
		t.Fatal(err)
	}
	// The file is a ReaderAt and a Seeker.
	f, err := v.Open("a/x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 10)
	if _, err := f.(io.ReaderAt).ReadAt(buf, 50_000); err != nil || !bytes.Equal(buf, randBytes(2, 100_000)[50_000:50_010]) {
		t.Fatalf("ReadAt: %v", err)
	}
	if off, err := f.(io.Seeker).Seek(-10, io.SeekEnd); err != nil || off != 99_990 {
		t.Fatalf("Seek = %d, %v", off, err)
	}
	d, err := v.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Read(buf); !errors.Is(err, ErrIsDir) {
		t.Fatalf("Read of a directory: %v", err)
	}
	d.Close()
	if _, err := d.(fs.ReadDirFile).ReadDir(1); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("ReadDir after Close: %v", err)
	}
	if m, err := fs.Glob(v, "a/*"); err != nil || !slices.Equal(m, []string{"a/b", "a/x"}) {
		t.Fatalf("Glob = %v, %v", m, err)
	}
}

func TestPathErrors(t *testing.T) {
	v, _ := newVolume(t)
	mkdir(t, v, "d")
	write(t, v, "d/f", []byte("x"))
	ctx := testCtx(t)
	for _, tc := range []struct {
		err  error
		want error
	}{
		{v.Mkdir("d"), fs.ErrExist},
		{v.Mkdir("."), fs.ErrExist},
		{v.Mkdir("missing/x"), fs.ErrNotExist},
		{v.Mkdir("d/f/x"), ErrNotDir},
		{v.WriteFile(ctx, "d", bytes.NewReader(nil)), ErrIsDir},
		{v.WriteFile(ctx, ".", bytes.NewReader(nil)), ErrIsDir},
		{v.WriteFile(ctx, "missing/x", bytes.NewReader(nil)), fs.ErrNotExist},
		{v.WriteFile(ctx, "d/f/x", bytes.NewReader(nil)), ErrNotDir},
		{v.WriteFile(ctx, "d/g", nil), ErrInvalidArgument},
		{v.Delete("."), fs.ErrInvalid},
		{v.Delete("nope"), fs.ErrNotExist},
		{v.Move("nope", "x"), fs.ErrNotExist},
		{v.Move(".", "x"), fs.ErrInvalid},
		{v.Move("d", "."), fs.ErrExist},
		{v.Move("d", "d/f"), fs.ErrExist},
		{v.Move("d", "d/e"), fs.ErrInvalid},
		{v.Move("d/f", "nope/f"), fs.ErrNotExist},
		{v.Move("d/f", "d/f/g"), ErrNotDir},
		{v.Move("d/f", "/abs"), fs.ErrInvalid},
	} {
		var pe *fs.PathError
		if !errors.As(tc.err, &pe) || !errors.Is(tc.err, tc.want) || pe.Op[:10] != "kolofiles:" {
			t.Errorf("error %v, want a PathError with %v", tc.err, tc.want)
		}
	}
	if _, err := v.Stat("d/f/x"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("Stat through a file: %v", err)
	}
	if _, err := v.ReadDir("d/f"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("ReadDir of a file: %v", err)
	}
	if _, err := v.Open("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing: %v", err)
	}
	// A failing source writes nothing.
	boom := errors.New("disk on fire")
	n := v.Len()
	wantErr(t, v.WriteFile(ctx, "d/g", errReader{boom}), boom)
	if v.Len() != n {
		t.Fatal("a failed write appended an op")
	}
	// A cancelled write appends nothing.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	wantErr(t, v.WriteFile(cctx, "d/g", bytes.NewReader(randBytes(3, 1<<20))), context.Canceled)
	if v.Len() != n {
		t.Fatal("a cancelled write appended an op")
	}
}

func TestOptions(t *testing.T) {
	e := newEnv(t)
	for _, o := range []Option{WithMaxDepth(0), WithMaxDepth(-1), WithMaxNameLength(0), WithMaxNameLength(maxEntry + 1), nil} {
		if _, err := Create(e.meta, e.content, o); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("Create with a bad option: %v", err)
		}
	}
	if _, err := Create(nil, e.content); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := Create(e.meta, e.meta); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Create on one log twice: %v", err)
	}
	other, err := e.st.Create(newIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(e.meta, other); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Create on logs of two owners: %v", err)
	}
	jsonLog, err := e.st.Create(e.id, kololog.WithValueEncoding(kololog.JSON))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(e.meta, jsonLog); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Create on a JSON log: %v", err)
	}
	appendRaw(t, e.content, []byte("x"))
	if _, err := Create(e.meta, e.content); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Create on a non-empty log: %v", err)
	}
	if _, err := Open(e.meta, e.content); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of an empty metadata log: %v", err)
	}
	// Open with another content log than the volume names.
	v, e2 := newVolume(t)
	metaID := e2.meta.ID()
	v.Close()
	meta, err := e2.st.Open(e2.id, metaID)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := e2.st.Create(e2.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(meta, c2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Open with another content log: %v", err)
	}
}

func TestReopen(t *testing.T) {
	v, e := newVolume(t)
	mkdir(t, v, "a")
	write(t, v, "a/f", []byte("one"))
	write(t, v, "a/f", []byte("two"))
	move(t, v, "a/f", "g")
	write(t, v, "a/h", []byte("h"))
	del(t, v, "a")
	want, n := tree(t, v), v.Len()
	ids := nodeOf(t, v, "g")
	v = e.reopen(t, v)
	if got := tree(t, v); !slices.Equal(got, want) || v.Len() != n || nodeOf(t, v, "g") != ids {
		t.Fatalf("after reopen: %q at %d, want %q at %d", got, v.Len(), want, n)
	}
	write(t, v, "g", []byte("three"))
	if string(read(t, v, "g")) != "three" {
		t.Fatal("write after reopen")
	}
	if v.ID() != e.meta.ID() {
		t.Fatal("ID is not the metadata log's")
	}
}

func TestClose(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("x"))
	mkdir(t, v, "d")
	s, err := v.Snapshot(v.Len())
	if err != nil {
		t.Fatal(err)
	}
	open, err := v.Open("f")
	if err != nil {
		t.Fatal(err)
	}
	r := newReader(t, e, true)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := r.v.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := testCtx(t)
	buf := make([]byte, 1)
	for name, call := range map[string]func() error{
		"WriteFile":        func() error { return v.WriteFile(ctx, "g", bytes.NewReader(nil)) },
		"Mkdir":            func() error { return v.Mkdir("e") },
		"Move":             func() error { return v.Move("f", "g") },
		"Delete":           func() error { return v.Delete("f") },
		"Refresh":          v.Refresh,
		"Open":             func() error { _, err := v.Open("f"); return err },
		"OpenContext":      func() error { _, err := v.OpenContext(ctx, "f"); return err },
		"ReadDir":          func() error { _, err := v.ReadDir("."); return err },
		"Stat":             func() error { _, err := v.Stat("f"); return err },
		"Snapshot":         func() error { _, err := v.Snapshot(1); return err },
		"Diff":             func() error { _, err := v.Diff(1, 1); return err },
		"snapshot Open":    func() error { _, err := s.Open("f"); return err },
		"snapshot ReadDir": func() error { _, err := s.ReadDir("."); return err },
		"snapshot Stat":    func() error { _, err := s.Stat("f"); return err },
		"file Read":        func() error { _, err := open.Read(buf); return err },
		"file ReadAt":      func() error { _, err := open.(io.ReaderAt).ReadAt(buf, 0); return err },
		"file Seek":        func() error { _, err := open.(io.Seeker).Seek(0, io.SeekStart); return err },
		"reader Refresh":   r.v.Refresh,
		"reader Open":      func() error { _, err := r.v.Open("f"); return err },
		"reader SetMark":   func() error { return r.v.SetMark("f", MarkKeepLocal) },
		"reader Mark":      func() error { _, err := r.v.Mark("f"); return err },
		"reader Prefetch":  func() error { return r.v.Prefetch(ctx) },
	} {
		err := call()
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: %v, want ErrClosed", name, err)
			continue
		}
		wantPrefix(t, err)
	}
	if err := open.Close(); err != nil {
		t.Fatalf("Close of a file after the volume: %v", err)
	}
}

// Every write on a reader is ErrReadOnly.
func TestReadOnly(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("x"))
	mkdir(t, v, "d")
	r := newReader(t, e, false)
	n := r.v.Len()
	ctx := testCtx(t)
	for name, call := range map[string]func() error{
		"WriteFile new":      func() error { return r.v.WriteFile(ctx, "g", bytes.NewReader([]byte("y"))) },
		"WriteFile existing": func() error { return r.v.WriteFile(ctx, "f", bytes.NewReader([]byte("y"))) },
		"Mkdir":              func() error { return r.v.Mkdir("e") },
		"Move":               func() error { return r.v.Move("f", "g") },
		"Delete":             func() error { return r.v.Delete("f") },
	} {
		err := call()
		var pe *fs.PathError
		if !errors.Is(err, ErrReadOnly) || !errors.As(err, &pe) {
			t.Errorf("%s on a reader: %v, want a PathError wrapping ErrReadOnly", name, err)
			continue
		}
		wantPrefix(t, err)
	}
	if r.v.Len() != n || string(read(t, r.v, "f")) != "x" {
		t.Fatal("a refused write changed the reader")
	}
}

func TestKeyNotReturnedOrPrinted(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("content"))
	key := fileKey(t, e.meta, 1)
	fi, _ := v.Stat("f")
	for _, s := range []string{fmt.Sprintf("%v %+v %#v", fi, fi.Sys(), fi.Sys()), fmt.Sprintf("%+v", v.Stats())} {
		if bytes.Contains([]byte(s), key[:]) || bytes.Contains([]byte(s), []byte(fmt.Sprintf("%x", key[:]))) {
			t.Fatal("the file key is printed")
		}
	}
	// The errors of a corrupt or deleted read hold no key or content.
	del(t, v, "f")
	s, err := v.Snapshot(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open("f"); err == nil || bytes.Contains([]byte(err.Error()), []byte("content")) {
		t.Fatalf("Open of a deleted file: %v", err)
	}
}

// fileKey reads the file key from the CREATE op at entry i of meta.
func fileKey(t testing.TB, meta interface{ Get(uint64) ([]byte, error) }, i uint64) koloblobs.Key {
	t.Helper()
	b, err := meta.Get(i)
	if err != nil {
		t.Fatal(err)
	}
	o, err := decodeOp(b, DefaultMaxNameLength, true)
	if err != nil || o.kind != kindCreate {
		t.Fatalf("entry %d is not a create: %v", i, err)
	}
	return o.key
}

func TestCorruptContent(t *testing.T) {
	// A version op whose blob the content log does not hold reads as an error, never as other bytes.
	v, e := newVolume(t)
	write(t, v, "f", []byte("content"))
	b, err := e.meta.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	st := kolostore.New(kolostore.NewMemory())
	defer st.Close()
	meta, err := st.Create(e.id)
	if err != nil {
		t.Fatal(err)
	}
	content, err := st.Create(e.id)
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, meta, encodeOp(&op{kind: kindVolume, volume: meta.ID(), contentLog: content.ID()}), b)
	v2, err := Open(meta, content)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if _, err := v2.Open("f"); err == nil {
		t.Fatal("a file without content opened")
	}
}

// Rewriting a file with one changed chunk adds the entries of that chunk, its tree path and the table, not the file.
func TestRewriteStoresOnlyChanges(t *testing.T) {
	v, e := newVolume(t)
	data := randBytes(3, 12<<20)
	write(t, v, "f", data)
	before := e.content.Len()
	changed := bytes.Clone(data)
	changed[6<<20] ^= 1
	write(t, v, "f", changed)
	grew := e.content.Len() - before
	t.Logf("%d entries for the file, %d added by the rewrite", before, grew)
	if before < 8 || grew == 0 || grew > 6 {
		t.Fatalf("%d entries for the file, %d added by the rewrite", before, grew)
	}
	if !bytes.Equal(read(t, v, "f"), changed) {
		t.Fatal("rewritten content differs")
	}
}

// cancelAfter cancels a context once n bytes were read and stored reports true. The write pipeline drops the
// chunks it has not stored when the context ends, so the cancel waits for the first chunk to reach the log.
type cancelAfter struct {
	r      io.Reader
	n      int
	stored func() bool
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.n -= n; c.n <= 0 {
		for end := time.Now().Add(10 * time.Second); !c.stored(); time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				return n, errors.New("no chunk was stored before the cancel")
			}
		}
		c.cancel()
	}
	return n, err
}

// A cancelled WriteFile repeated with the same content stores no object twice.
func TestRepeatedCancelledWriteDuplicatesNothing(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("small"))
	data := randBytes(4, 8<<20)
	before := e.content.Len()
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	src := &cancelAfter{r: bytes.NewReader(data), n: 4 << 20, stored: func() bool { return e.content.Len() > before }, cancel: cancel}
	err := v.WriteFile(ctx, "f", src)
	wantErr(t, err, context.Canceled)
	partial := e.content.Len()
	if partial == before {
		t.Fatal("the cancelled write stored nothing, so the test resumes nothing")
	}
	write(t, v, "f", data)
	full := e.content.Len()
	write(t, v, "f", data)
	if e.content.Len() != full {
		t.Fatalf("writing the same content again added %d entries", e.content.Len()-full)
	}
	seen := map[[sha256.Size]byte]uint64{}
	for i := range e.content.Len() {
		b, err := e.content.Get(i)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		if j, ok := seen[h]; ok {
			t.Fatalf("entries %d and %d hold the same object", j, i)
		}
		seen[h] = i
	}
	if !bytes.Equal(read(t, v, "f"), data) {
		t.Fatal("content differs")
	}
}

// The reader constructors and ContentLog refuse a bad argument or a log that is not a volume's.
func TestReaderArguments(t *testing.T) {
	v, e := newVolume(t)
	write(t, v, "f", []byte("x"))
	r := newReader(t, e, false)
	fetch := func(context.Context, uint64) ([]byte, error) { return nil, nil }
	backend := koloblobs.NewLogBackend(r.content)
	jsonLog, err := e.st.Create(e.id, kololog.WithValueEncoding(kololog.JSON))
	if err != nil {
		t.Fatal(err)
	}
	jsonRep := newReplica(t, newStore(t), jsonLog)
	for name, call := range map[string]func() error{
		"OpenReader nil replica":       func() error { _, err := OpenReader(nil, backend, nil); return err },
		"OpenReader nil content":       func() error { _, err := OpenReader(r.meta, nil, nil); return err },
		"OpenReader JSON replica":      func() error { _, err := OpenReader(jsonRep, backend, nil); return err },
		"OpenReader bad option":        func() error { _, err := OpenReader(r.meta, backend, nil, WithMaxDepth(0)); return err },
		"OpenReader nil option":        func() error { _, err := OpenReader(r.meta, backend, nil, nil); return err },
		"OpenFetchReader nil replica":  func() error { _, err := OpenFetchReader(nil, fetch, nil); return err },
		"OpenFetchReader nil fetch":    func() error { _, err := OpenFetchReader(r.meta, nil, nil); return err },
		"OpenFetchReader JSON replica": func() error { _, err := OpenFetchReader(jsonRep, fetch, nil); return err },
		"OpenFetchReader bad option":   func() error { _, err := OpenFetchReader(r.meta, fetch, nil, WithMaxNameLength(0)); return err },
		"ContentLog nil replica":       func() error { _, err := ContentLog(nil); return err },
	} {
		err := call()
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v, want ErrInvalidArgument", name, err)
			continue
		}
		wantPrefix(t, err)
	}
	// The replica stays open when a constructor fails.
	if _, err := r.meta.Head(); err != nil {
		t.Fatalf("replica after a failed OpenReader: %v", err)
	}
	if _, err := ContentLog(newReplica(t, newStore(t), e.meta)); !errors.Is(err, kololog.ErrNoHead) {
		t.Fatalf("ContentLog of an empty replica: %v", err)
	} else {
		wantPrefix(t, err)
	}
	if id, err := ContentLog(r.meta); err != nil || id != e.content.ID() {
		t.Fatalf("ContentLog = %x, %v", id, err)
	}
	// An entry 0 that is not a VOLUME of this log.
	for name, first := range map[string]*op{
		"mkdir":             {kind: kindMkdir, node: NodeID{1}, name: "a"},
		"volume of another": {kind: kindVolume, volume: kololog.LogID{1}},
	} {
		x := newEnv(t)
		appendRaw(t, x.meta, encodeOp(first))
		rep := newReplica(t, newStore(t), x.meta)
		grow(t, rep, x.meta, true)
		_, err := ContentLog(rep)
		wantErr(t, err, ErrCorrupt)
		wantPrefix(t, err)
		if _, err := OpenReader(rep, backend, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: OpenReader = %v, want ErrCorrupt", name, err)
		}
	}
}
