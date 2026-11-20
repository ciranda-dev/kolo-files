package kolofiles

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	koloid "github.com/ciranda-dev/kolo-id"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// env is a writer's store, identity and logs.
type env struct {
	st            *kolostore.Store
	id            *koloid.Identity
	meta, content *kolostore.Log
}

func newIdentity(t testing.TB) *koloid.Identity {
	t.Helper()
	id, err := koloid.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newStore(t testing.TB) *kolostore.Store {
	t.Helper()
	s := kolostore.New(kolostore.NewMemory())
	t.Cleanup(func() { s.Close() })
	return s
}

// newEnv creates two empty logs of one owner in a store in memory.
func newEnv(t testing.TB) *env {
	t.Helper()
	e := &env{st: newStore(t), id: newIdentity(t)}
	var err error
	if e.meta, err = e.st.Create(e.id); err != nil {
		t.Fatal(err)
	}
	if e.content, err = e.st.Create(e.id); err != nil {
		t.Fatal(err)
	}
	return e
}

func newVolume(t testing.TB, opts ...Option) (*Volume, *env) {
	t.Helper()
	e := newEnv(t)
	v, err := Create(e.meta, e.content, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, e
}

// reopenLogs closes v and opens its logs again, without a volume on them.
func (e *env) reopenLogs(t testing.TB, v *Volume) {
	t.Helper()
	metaID, contentID := e.meta.ID(), e.content.ID()
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	e.openLogs(t, metaID, contentID)
}

func (e *env) openLogs(t testing.TB, metaID, contentID kololog.LogID) {
	t.Helper()
	var err error
	if e.meta, err = e.st.Open(e.id, metaID); err != nil {
		t.Fatal(err)
	}
	if e.content, err = e.st.Open(e.id, contentID); err != nil {
		t.Fatal(err)
	}
}

func (e *env) reopen(t testing.TB, v *Volume, opts ...Option) *Volume {
	t.Helper()
	e.reopenLogs(t, v)
	v, err := Open(e.meta, e.content, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

func testCtx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func write(t testing.TB, v *Volume, name string, data []byte) {
	t.Helper()
	if err := v.WriteFile(testCtx(t), name, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t testing.TB, v *Volume, name string) {
	t.Helper()
	if err := v.Mkdir(name); err != nil {
		t.Fatal(err)
	}
}

func move(t testing.TB, v *Volume, from, to string) {
	t.Helper()
	if err := v.Move(from, to); err != nil {
		t.Fatal(err)
	}
}

func del(t testing.TB, v *Volume, name string) {
	t.Helper()
	if err := v.Delete(name); err != nil {
		t.Fatal(err)
	}
}

func read(t testing.TB, fsys fs.FS, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantErr(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

// tree lists fsys as "path" for directories and "path=content" for files, in walk order.
func tree(t testing.TB, fsys fs.FS) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			out = append(out, p+"/")
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		out = append(out, p+"="+string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func nodeOf(t testing.TB, fsys fs.StatFS, name string) NodeInfo {
	t.Helper()
	fi, err := fsys.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(NodeInfo)
}

// newReplica opens an empty replica of l in a store of its own.
func newReplica(t testing.TB, st *kolostore.Store, l *kolostore.Log) *kolostore.Replica {
	t.Helper()
	hdr := l.Header()
	r, err := st.OpenReplica(hdr.LogKey, hdr.LogID, hdr.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// grow sends r the entries of l it lacks through Replica.Update, the path kolo-sync uses; keys tells whether the
// entries carry their keys.
func grow(t testing.TB, r *kolostore.Replica, l *kolostore.Log, keys bool) {
	t.Helper()
	h, err := l.Head()
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() == h.Size {
		return
	}
	p, err := l.ConsistencyProof(r.Len(), h.Size)
	if err != nil {
		t.Fatal(err)
	}
	var entries []kololog.Entry
	for i := r.Len(); i < h.Size; i++ {
		e, err := l.Entry(i)
		if err != nil {
			t.Fatal(err)
		}
		if !keys {
			e.Key = nil
		}
		entries = append(entries, e)
	}
	if err := r.Update(h, p, entries); err != nil {
		t.Fatal(err)
	}
}

func applyMarkers(t testing.TB, r *kolostore.Replica, l *kolostore.Log) {
	t.Helper()
	var markers [][]byte
	for i := uint64(0); i < r.Len(); i++ {
		e, err := l.Entry(i)
		if err != nil {
			t.Fatal(err)
		}
		if e.Marker != nil {
			markers = append(markers, e.Marker)
		}
	}
	if len(markers) > 0 {
		if err := r.ApplyDelete(markers...); err != nil {
			t.Fatal(err)
		}
	}
}

// reader is a reader device with replicas of both logs in its own store.
type reader struct {
	st            *kolostore.Store
	meta, content *kolostore.Replica
	local         kolostore.Backend
	v             *Volume
}

// newReader replicates both logs of e and opens a reader on them.
func newReader(t testing.TB, e *env, local bool) *reader {
	t.Helper()
	r := &reader{st: newStore(t)}
	r.meta = newReplica(t, r.st, e.meta)
	r.content = newReplica(t, r.st, e.content)
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	if local {
		r.local = kolostore.NewMemory()
		t.Cleanup(func() { r.local.Close() })
	}
	v, err := OpenReader(r.meta, koloblobs.NewLogBackend(r.content), r.local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	r.v = v
	return r
}

// sync grows the reader's replicas, applies the markers and refreshes.
func (r *reader) sync(t testing.TB, e *env) {
	t.Helper()
	grow(t, r.meta, e.meta, true)
	grow(t, r.content, e.content, true)
	applyMarkers(t, r.meta, e.meta)
	applyMarkers(t, r.content, e.content)
	if err := r.v.Refresh(); err != nil {
		t.Fatal(err)
	}
}

func appendRaw(t testing.TB, l *kolostore.Log, entries ...[]byte) {
	t.Helper()
	for _, b := range entries {
		if _, err := l.Append(b); err != nil {
			t.Fatal(err)
		}
	}
}

// hostile builds a log by hand, entry 0 a valid VOLUME, then opens it as a writer and returns the error.
func hostile(t testing.TB, ops ...*op) error {
	t.Helper()
	return hostileOpts(t, nil, ops...)
}

// hostileOpts is hostile with options for Open.
func hostileOpts(t testing.TB, opts []Option, ops ...*op) error {
	t.Helper()
	entries := make([][]byte, len(ops))
	for i, o := range ops {
		entries[i] = encodeOp(o)
	}
	return hostileEntries(t, opts, entries...)
}

// hostileEntries is hostileOpts with the entries after the VOLUME entry given as bytes.
func hostileEntries(t testing.TB, opts []Option, entries ...[]byte) error {
	t.Helper()
	e := newEnv(t)
	appendRaw(t, e.meta, encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()}))
	appendRaw(t, e.meta, entries...)
	// A reader on a replica of the same log must reach the same verdict as the writer.
	rst := newStore(t)
	meta, content := newReplica(t, rst, e.meta), newReplica(t, rst, e.content)
	grow(t, meta, e.meta, true)
	rv, rerr := OpenReader(meta, koloblobs.NewLogBackend(content), nil, opts...)
	if rerr == nil {
		rv.Close()
	}
	v, err := Open(e.meta, e.content, opts...)
	if err == nil {
		v.Close()
	}
	for _, target := range []error{ErrCorrupt, ErrLimit} {
		if errors.Is(err, target) != errors.Is(rerr, target) {
			t.Errorf("writer: %v, reader: %v", err, rerr)
		}
	}
	if (err == nil) != (rerr == nil) {
		t.Errorf("writer: %v, reader: %v", err, rerr)
	}
	return err
}

// stream is a seekable pseudo-random byte source: the AES-CTR keystream of a seed.
func randBytes(seed byte, n int) []byte {
	k := make([]byte, 32)
	k[0] = seed
	b, err := aes.NewCipher(k)
	if err != nil {
		panic(err)
	}
	out := make([]byte, n)
	cipher.NewCTR(b, make([]byte, 16)).XORKeyStream(out, out)
	return out
}

func privateDir(t testing.TB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkGoroutines fails the test if goroutines started after it are still running after the cleanups.
func checkGoroutines(t *testing.T) {
	t.Helper()
	before := goroutines()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var leaked []string
			for id, stack := range goroutines() {
				if _, ok := before[id]; !ok {
					leaked = append(leaked, stack)
				}
			}
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d goroutines leaked:\n%s", len(leaked), strings.Join(leaked, "\n\n"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func goroutines() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := make(map[string]string)
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if id, _, ok := strings.Cut(strings.TrimPrefix(stack, "goroutine "), " "); ok {
			out[id] = stack
		}
	}
	return out
}

// allocated returns how many bytes f allocated.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// wantPrefix fails the test unless err's message starts with "kolofiles:".
func wantPrefix(t testing.TB, err error) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), "kolofiles:") {
		t.Fatalf("error %q does not start with kolofiles:", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

var _ io.Reader = errReader{}
