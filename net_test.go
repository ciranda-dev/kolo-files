package kolofiles

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	koloid "github.com/ciranda-dev/kolo-id"
	kololog "github.com/ciranda-dev/kolo-log"
	"github.com/ciranda-dev/kolo-pin"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolostore "github.com/ciranda-dev/kolo-store"
	kolosync "github.com/ciranda-dev/kolo-sync"
)

// fixture is a writer's volume as the network tests read it: some files in folders, a file that was moved, a
// file with two versions and a file that was deleted.
type fixture struct {
	e     *env
	v     *Volume
	files map[string][]byte // path to current content
	tree  []string
}

const deletedName = "secret-name"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	v, e := newVolume(t)
	f := &fixture{e: e, v: v, files: map[string][]byte{}}
	put := func(name string, data []byte) {
		write(t, v, name, data)
		f.files[name] = data
	}
	mkdir(t, v, "documents")
	mkdir(t, v, "photographs")
	put("documents/readme-file.txt", []byte("hello"))
	old := randBytes(7, 6<<20)
	put("documents/big-file.bin", old)
	put("documents/big-file.bin", append(bytes.Clone(old[:5<<20]), randBytes(8, 1<<20)...))
	put("photographs/album-1.bin", randBytes(9, 3<<20))
	put("tmp", randBytes(10, 1<<20))
	move(t, v, "tmp", "photographs/moved-file.bin")
	f.files["photographs/moved-file.bin"] = f.files["tmp"]
	delete(f.files, "tmp")
	write(t, v, deletedName, randBytes(11, 1<<20))
	del(t, v, deletedName)
	f.tree = tree(t, v)
	return f
}

// secrets lists what must never reach a pinning peer: the file keys, every entry key of both logs, the names of
// the tree, and windows of each file's plaintext.
func (f *fixture) secrets(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	for i := range f.e.meta.Len() {
		b, err := f.e.meta.Get(i)
		if errors.Is(err, kololog.ErrDeleted) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if o, err := decodeOp(b, DefaultMaxNameLength, true); err != nil {
			t.Fatal(err)
		} else if o.kind == kindCreate {
			out = append(out, bytes.Clone(o.key[:]))
		}
	}
	for _, l := range []*kolostore.Log{f.e.meta, f.e.content} {
		for i := range l.Len() {
			if k, err := l.Key(i); err == nil {
				out = append(out, k)
			}
		}
	}
	for _, n := range []string{"documents", "photographs", "readme-file.txt", "big-file.bin", "album-1.bin", "moved-file.bin", deletedName} {
		out = append(out, []byte(n))
	}
	for _, s := range out {
		if len(s) < 8 {
			t.Fatalf("a secret of %d bytes would match random ciphertext by chance", len(s))
		}
	}
	for _, data := range f.files {
		for _, off := range []int{0, len(data) / 3, len(data) / 2, len(data) - 32} {
			if off >= 0 && off+32 <= len(data) {
				out = append(out, data[off:off+32])
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("only %d secrets listed", len(out))
	}
	return out
}

func startPin(t testing.TB, dir string) (pin.Peer, func()) {
	t.Helper()
	id, err := pin.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := kolosecure.Listen(ctx, "127.0.0.1:0", id, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- pin.Serve(ctx, ln, pin.Options{Dir: dir}) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("pin.Serve: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Error("pin.Serve did not return")
			}
			_ = ln.Close()
		})
	}
	t.Cleanup(stop)
	return pin.Peer{ID: id.PublicKey(), Addr: ln.Addr().String()}, stop
}

func serveSync(t testing.TB, id *koloid.Identity, srv *kolosync.Server) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := kolosecure.Listen(ctx, "127.0.0.1:0", id, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			wg.Go(func() {
				_ = srv.Serve(ctx, conn)
				_ = conn.Close()
			})
		}
	})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = ln.Close()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

// heldKeys is a reader's store of entry keys; it records every copy it hands to kolo-sync.
type heldKeys struct {
	mu     sync.Mutex
	keys   []kololog.EntryKey
	calls  int
	noneAt int // the call, counted from 0, that gets no key; -1 for none
	given  [][]byte
}

func newHeld(keys []kololog.EntryKey) *heldKeys { return &heldKeys{keys: keys, noneAt: -1} }

func (k *heldKeys) key(i uint64) (kololog.EntryKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := k.calls
	k.calls++
	if n == k.noneAt || i >= uint64(len(k.keys)) || k.keys[i] == nil {
		return nil, nil
	}
	c := bytes.Clone(k.keys[i])
	k.given = append(k.given, c)
	return c, nil
}

// zeroed reports whether kolo-sync cleared every key copy it was given.
func (k *heldKeys) zeroed() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.given {
		if !bytes.Equal(c, make([]byte, len(c))) {
			return false
		}
	}
	return true
}

// fetchWithKeys is the function a reader wires to a ciphertext-only peer: kolo-sync's FetchWithKeys of one entry
// against the head the reader trusts, opened with the key the reader holds. It counts the entries fetched.
func fetchWithKeys(conn *kolosecure.Conn, head kololog.VerifiedHead, keys func(uint64) (kololog.EntryKey, error), n *atomic.Int64) func(context.Context, uint64) ([]byte, error) {
	return func(ctx context.Context, i uint64) ([]byte, error) {
		f, err := kolosync.FetchWithKeys(ctx, conn, head, i, i, keys)
		if err != nil {
			return nil, err
		}
		n.Add(1)
		return f[0].Data, f[0].Err
	}
}

// alice is the owner's side of the network tests: her logs offered with Keys access to one reader.
type alice struct {
	addr string
	stop func()
	pub  ed25519.PublicKey
}

func (f *fixture) serveAlice(t *testing.T, reader ed25519.PublicKey) *alice {
	t.Helper()
	srv := kolosync.NewServer(func(p, _ ed25519.PublicKey, _ kololog.LogID) kolosync.Access {
		if p.Equal(reader) {
			return kolosync.Keys
		}
		return kolosync.NoAccess
	})
	for _, l := range []*kolostore.Log{f.e.meta, f.e.content} {
		if err := srv.Offer(l); err != nil {
			t.Fatal(err)
		}
	}
	addr, stop := serveSync(t, f.e.id, srv)
	return &alice{addr: addr, stop: stop, pub: f.e.id.PublicKey()}
}

// contentKeys returns alice's verified head of the content log and every entry key she holds for it.
func (f *fixture) contentKeys(t *testing.T, a *alice, bob *koloid.Identity) (kololog.VerifiedHead, []kololog.EntryKey) {
	t.Helper()
	ctx := testCtx(t)
	conn, err := kolosecure.Dial(ctx, a.addr, bob, a.pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	head, err := kolosync.FetchHead(ctx, conn, a.pub, f.e.content.ID())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := kolosync.FetchKeys(ctx, conn, head, 0, head.Size()-1)
	if err != nil {
		t.Fatal(err)
	}
	return head, keys
}

// pull pulls logID of owner from the peer at addr into rep, opening rep in st if it is nil; ciphertext pulls no
// keys.
func pull(t testing.TB, st *kolostore.Store, rep *kolostore.Replica, addr string, peer, owner ed25519.PublicKey, logID kololog.LogID, bob *koloid.Identity, ciphertext bool) *kolostore.Replica {
	t.Helper()
	ctx := testCtx(t)
	conn, err := kolosecure.Dial(ctx, addr, bob, peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if rep == nil {
		header, err := kolosync.FetchHeader(ctx, conn, owner, logID)
		if err != nil {
			t.Fatal(err)
		}
		if rep, err = st.OpenReplica(owner, logID, header); err != nil {
			t.Fatal(err)
		}
	}
	pull := kolosync.Pull
	if ciphertext {
		pull = kolosync.PullCiphertext
	}
	if err := pull(ctx, conn, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// dump returns every byte a pinning peer keeps in dir, apart from its identity file: its store and any other file.
func dump(t *testing.T, dir string) []byte {
	t.Helper()
	var out []byte
	db, err := kolostore.OpenPebble(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Scan(nil, nil, false, 0, func(k, v []byte) bool {
		out = append(append(out, k...), v...)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) == ".key" {
			return err
		}
		b, err := os.ReadFile(path)
		out = append(out, b...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Alice pins both logs and goes away. Bob, who holds the entry keys, reads the tree and its files from the pinning
// peer, which holds ciphertext only; the entry keys and file keys never go to it.
func TestPinnedVolumeReadWhileOwnerOffline(t *testing.T) {
	checkGoroutines(t)
	ctx := testCtx(t)
	f := newFixture(t)
	dir := privateDir(t)
	peer, stopPin := startPin(t, dir)
	capb, err := pin.Issue(f.e.id, pin.Capability{Peer: peer.ID, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []*kolostore.Log{f.e.meta, f.e.content} {
		if info, err := pin.Add(ctx, f.e.id, peer, l, capb); err != nil || info.Entries != l.Len() {
			t.Fatalf("pin.Add = %+v, %v", info, err)
		}
	}

	// Bob has the metadata log from the pinning peer, without keys: he cannot read it yet.
	bob := newIdentity(t)
	st := newStore(t)
	metaID, contentID := f.e.meta.ID(), f.e.content.ID()
	meta := pull(t, st, nil, peer.Addr, peer.ID, f.e.id.PublicKey(), metaID, bob, true)
	if meta.Len() != f.e.meta.Len() {
		t.Fatalf("pulled %d of %d entries", meta.Len(), f.e.meta.Len())
	}
	// Alice gives him the keys of the metadata log and of the content log, and goes away.
	a := f.serveAlice(t, bob.PublicKey())
	pull(t, st, meta, a.addr, a.pub, a.pub, metaID, bob, false)
	head, keys := f.contentKeys(t, a, bob)
	a.stop()
	if head.Size() != f.e.content.Len() {
		t.Fatalf("head of %d entries, the log has %d", head.Size(), f.e.content.Len())
	}

	pc, err := kolosecure.Dial(ctx, peer.Addr, bob, peer.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	held := newHeld(keys)
	var fetched atomic.Int64
	local := kolostore.NewMemory()
	defer local.Close()
	rv, err := OpenFetchReader(meta, fetchWithKeys(pc, head, held.key, &fetched), local)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	if rv.ID() != metaID || rv.Len() != f.e.meta.Len() {
		t.Fatalf("reader at %d of %d entries", rv.Len(), f.e.meta.Len())
	}
	if got, err := ContentLog(meta); err != nil || got != contentID {
		t.Fatalf("ContentLog = %x, %v", got, err)
	}
	if got := tree(t, rv); !slices.Equal(got, f.tree) {
		t.Fatalf("tree %q, want %q", got, f.tree)
	}
	if _, err := rv.Stat(deletedName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the deleted file: %v", err)
	}

	// A range of a big file fetches a few entries of the content log, not the file.
	fetched.Store(0)
	r, err := rv.Open("documents/big-file.bin")
	if err != nil {
		t.Fatal(err)
	}
	big := f.files["documents/big-file.bin"]
	buf := make([]byte, 10)
	off := int64(5<<20 + 100)
	if _, err := r.(interface {
		ReadAt([]byte, int64) (int, error)
	}).ReadAt(buf, off); err != nil || !bytes.Equal(buf, big[off:off+10]) {
		t.Fatalf("range read: %v", err)
	}
	r.Close()
	if n := fetched.Load(); n == 0 || n > 8 || n >= int64(f.e.content.Len())/2 {
		t.Fatalf("a 10-byte read fetched %d of %d entries", n, f.e.content.Len())
	}

	// Selective sync over the network: docs is kept, so it reads with the pinning peer gone; photographs is not.
	if err := rv.SetMark("documents", MarkKeepLocal); err != nil {
		t.Fatal(err)
	}
	if err := rv.Prefetch(ctx); err != nil {
		t.Fatal(err)
	}
	stopPin()
	for _, name := range []string{"documents/readme-file.txt", "documents/big-file.bin"} {
		if got := read(t, rv, name); !bytes.Equal(got, f.files[name]) {
			t.Fatalf("%s read back differently with the pin gone", name)
		}
	}
	if _, err := rv.Open("photographs/album-1.bin"); err == nil {
		t.Fatal("a file that is not kept opened with the pin gone")
	}
	if !held.zeroed() {
		t.Fatal("a key copy handed to kolo-sync was not cleared after use")
	}

	// The pinning peer's store holds the ciphertext, and no key, name or plaintext.
	d := dump(t, dir)
	e, err := f.e.content.Entry(0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(d, e.Ciphertext) {
		t.Fatal("the dump does not hold the log's ciphertext, so the scan is wrong")
	}
	for i, s := range f.secrets(t) {
		if bytes.Contains(d, s) {
			t.Errorf("secret %d found on the pinning peer", i)
		}
	}
}
