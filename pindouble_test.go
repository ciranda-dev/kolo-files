package kolofiles

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kolocodec "github.com/ciranda-dev/kolo-codec"
	koloid "github.com/ciranda-dev/kolo-id"
	kololog "github.com/ciranda-dev/kolo-log"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolostore "github.com/ciranda-dev/kolo-store"
	kolosync "github.com/ciranda-dev/kolo-sync"
	kolotransport "github.com/ciranda-dev/kolo-transport"
)

// pinDouble serves the content log as a ciphertext-only replica over the kolo-sync wire, by hand, so a test sees
// every byte the reader sends and can change every answer. It fails the test if it receives anything but the
// exact bytes of a HEAD_REQ for the log or a FETCH_REQ of one entry, so no field can carry key material, if a
// request contains a secret byte string, or if it ever holds an entry key.
type pinDouble struct {
	t       *testing.T
	rep     *kolostore.Replica
	secrets [][]byte
	headReq []byte // the only HEAD_REQ payload a reader of this log sends
	mu      sync.Mutex
	// answer returns the frame to send for a FETCH_REQ of entry start; nil sends the honest answer.
	answer func(start uint64, honest []byte) []byte
	// head returns the frame to send for the HEAD_REQ; nil sends the honest answer.
	head func(honest []byte) []byte
	// errorf reports a request off the list, a secret seen or a key held; t.Errorf unless a test checks the
	// double itself.
	errorf func(format string, args ...any)
}

func (f *fixture) newPinDouble(t *testing.T, secrets [][]byte) *pinDouble {
	t.Helper()
	l := f.e.content
	rep := newReplica(t, newStore(t), l)
	head, err := l.Head()
	if err != nil {
		t.Fatal(err)
	}
	var entries []kololog.Entry
	for i := range l.Len() {
		e, err := l.Entry(i)
		if err != nil {
			t.Fatal(err)
		}
		e.Key = nil
		entries = append(entries, e)
	}
	if err := rep.Update(head, kololog.ConsistencyProof{}, entries); err != nil {
		t.Fatal(err)
	}
	var e kolocodec.Encoder
	e.PutUvarint(1)
	e.PutUvarint(1)
	e.PutFixed(f.e.id.PublicKey())
	id := l.ID()
	e.PutFixed(id[:])
	return &pinDouble{t: t, rep: rep, secrets: secrets, headReq: e.Bytes(), errorf: t.Errorf}
}

func (d *pinDouble) scan(b []byte) {
	for i, s := range d.secrets {
		if len(s) != 0 && bytes.Contains(b, s) {
			d.errorf("the pin received secret %d", i)
		}
	}
}

func (d *pinDouble) hooks() (func(uint64, []byte) []byte, func([]byte) []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answer, d.head
}

// serve answers every stream of every connection on a loopback listener as id until the test ends.
func (d *pinDouble) serve(id *koloid.Identity) string {
	t := d.t
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
				defer conn.Close()
				for {
					st, err := conn.AcceptStream(ctx)
					if err != nil {
						return
					}
					wg.Go(func() {
						defer st.Close()
						d.stream(st)
					})
				}
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

func (d *pinDouble) stream(st *kolotransport.Stream) {
	for {
		var n [4]byte
		if _, err := io.ReadFull(st, n[:]); err != nil {
			return
		}
		size := binary.LittleEndian.Uint32(n[:])
		if size > kolosync.MaxMessageSize {
			d.errorf("a request of %d bytes", size)
			return
		}
		b := make([]byte, size)
		if k, err := io.ReadFull(st, b); err != nil {
			// A partial request is still scanned: key bytes must not reach the pin in any frame.
			d.scan(b[:k])
			d.errorf("a partial request: %d of %d bytes", k, size)
			return
		}
		d.scan(b)
		answer, head := d.hooks()
		var out []byte
		switch {
		case bytes.Equal(b, d.headReq):
			h, err := d.rep.Head()
			if err != nil {
				d.t.Error(err)
				return
			}
			out = headFrame(h.Encode(), d.rep.Header().Encode())
			if head != nil {
				out = head(out)
			}
		default:
			start, size, ok := d.fetchReq(b)
			if !ok {
				d.errorf("the pin got a request off the list: %d bytes", len(b))
				return
			}
			if out = d.fetch(start, start, size); out == nil {
				return
			}
			if answer != nil {
				out = answer(start, out)
			}
		}
		st.Write(out)
		if len(out) < 4 || int(binary.LittleEndian.Uint32(out))+4 != len(out) {
			return // a truncated or oversize frame ends the stream
		}
	}
}

// fetchReq returns the entry and tree size of b if b is exactly a FETCH_REQ of one entry in a tree of at most the
// double's size.
func (d *pinDouble) fetchReq(b []byte) (uint64, uint64, bool) {
	dec := kolocodec.NewDecoder(b)
	v, _ := dec.Uvarint()
	typ, _ := dec.Uvarint()
	start, _ := dec.Uvarint()
	end, _ := dec.Uvarint()
	size, err := dec.Uvarint()
	if err != nil || v != 1 || typ != 10 || start != end || size > d.rep.Len() || start >= size {
		return 0, 0, false
	}
	var e kolocodec.Encoder
	for _, x := range []uint64{1, 10, start, end, size} {
		e.PutUvarint(x)
	}
	return start, size, bytes.Equal(e.Bytes(), b)
}

// fetch encodes the FETCH answer for entries start to end of the tree of size entries.
func (d *pinDouble) fetch(start, end, size uint64) []byte {
	var e kolocodec.Encoder
	e.PutUvarint(1)
	e.PutUvarint(11)
	e.PutUvarint(end - start + 1)
	for i := start; i <= end; i++ {
		en, err := d.rep.Entry(i)
		if err != nil {
			d.t.Error(err)
			return nil
		}
		if len(en.Key) != 0 {
			d.errorf("the pin holds the key of entry %d", i)
		}
		p, err := d.rep.InclusionProof(i, size)
		if err != nil {
			d.t.Error(err)
			return nil
		}
		e.PutBytes(en.Record)
		e.PutBytes(en.Ciphertext)
		e.PutBytes(en.Marker)
		e.PutBytes(nil)
		e.PutBytes(p.Encode())
	}
	return frame(e.Bytes())
}

func headFrame(head, header []byte) []byte {
	var e kolocodec.Encoder
	e.PutUvarint(1)
	e.PutUvarint(5)
	e.PutBytes(head)
	e.PutBytes(header)
	return frame(e.Bytes())
}

func frame(payload []byte) []byte {
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(payload))), payload...)
}

// forkedHead returns the HEAD answer of a pin whose log has the size of head and another root, signed by alice:
// she signed two histories.
func (f *fixture) forkedHead(t *testing.T, head kololog.VerifiedHead) []byte {
	t.Helper()
	h := head.Head()
	h.Root[0] ^= 1
	var e kolocodec.Encoder
	e.PutString(kololog.HeadContext)
	e.PutFixed(h.LogKey)
	e.PutFixed(h.LogID[:])
	e.PutUvarint(h.Size)
	e.PutFixed(h.Root[:])
	var err error
	if h.Signature, err = f.e.id.Sign(kololog.HeadContext, e.Bytes()); err != nil {
		t.Fatal(err)
	}
	return headFrame(h.Encode(), f.e.content.Header().Encode())
}

// pinAttack is one way a pin double misbehaves; at counts FETCH answers and keys asked from 0.
type pinAttack struct {
	at     int
	answer func(start uint64, honest []byte) []byte
	head   func(honest []byte) []byte
	noKey  bool
	all    bool // after the range read, read every file whole and compare
	stall  bool // the answer never comes, at the HEAD answer if at is -1; the read is cancelled when it is reached
	mode   readMode
	cancel bool // cancel the context given to OpenContext once the file is open, then read
}

// readMode is how pinRead gets the big file: a plain reader, a reader that keeps the file local, or Prefetch.
type readMode int

const (
	readPlain readMode = iota
	readKeep
	readPrefetch
)

// checkKept fails the test for a kept object that is not the object its key names.
func checkKept(t testing.TB, v *Volume, local kolostore.Backend) {
	t.Helper()
	start := v.localPrefix(localObject)
	err := local.Scan(start, upper(start), false, 0, func(k, val []byte) bool {
		var h koloblobs.Hash
		copy(h[:], k[len(k)-len(h):])
		if !objectOK(h, val) {
			t.Errorf("a kept object does not match its hash")
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pinRead reads a few bytes of the big file through d changed by h, with the volume's metadata log replicated
// whole, and returns the bytes read and the error. It checks that every key copy handed to kolo-sync was cleared.
func (f *fixture) pinRead(t *testing.T, d *pinDouble, addr string, pid ed25519.PublicKey, head kololog.VerifiedHead, keys []kololog.EntryKey, h pinAttack) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	var fetches atomic.Int64
	d.mu.Lock()
	d.head = h.head
	if h.stall && h.at < 0 {
		d.head = func(honest []byte) []byte {
			cancel()
			<-release
			return honest
		}
	}
	d.answer = func(start uint64, honest []byte) []byte {
		if int(fetches.Add(1)-1) != h.at {
			return honest
		}
		if h.stall {
			cancel()
			<-release
			return honest
		}
		if h.answer != nil {
			return h.answer(start, honest)
		}
		return honest
	}
	d.mu.Unlock()
	conn, err := kolosecure.Dial(ctx, addr, newIdentity(t), pid, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	held := newHeld(keys)
	if h.noKey {
		held.noneAt = h.at
	}
	defer func() {
		if !held.zeroed() {
			t.Error("a key copy handed to kolo-sync was not cleared after use")
		}
		if h.at >= 0 && int(fetches.Load()) <= h.at {
			t.Errorf("the read stopped after %d answers, before the attack on answer %d", fetches.Load(), h.at)
		}
	}()
	meta := newReplica(t, newStore(t), f.e.meta)
	grow(t, meta, f.e.meta, true)
	var n atomic.Int64
	var local kolostore.Backend
	if h.mode != readPlain {
		local = kolostore.NewMemory()
		defer local.Close()
	}
	rv, err := OpenFetchReader(meta, fetchWithKeys(conn, head, held.key, &n), local)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	if h.mode != readPlain {
		if err := rv.SetMark("documents/big-file.bin", MarkKeepLocal); err != nil {
			t.Fatal(err)
		}
		defer checkKept(t, rv, local)
	}
	if h.mode == readPrefetch {
		return nil, rv.Prefetch(ctx)
	}
	file, err := rv.OpenContext(ctx, "documents/big-file.bin")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, 10)
	if h.cancel {
		cancel()
		opened := fetches.Load()
		k, err := file.(io.ReaderAt).ReadAt(buf, bigOffset)
		if fetches.Load() != opened {
			t.Error("a read after the context was cancelled asked the pin for entries")
		}
		return buf[:k], err
	}
	k, err := file.(io.ReaderAt).ReadAt(buf, bigOffset)
	if h.all && err == nil {
		for name, want := range f.files {
			if got, err := fs.ReadFile(rv, name); err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s read back as %d bytes, %v", name, len(got), err)
			}
		}
	}
	return buf[:k], err
}

const bigOffset = 5<<20 + 100

// headAndKeys returns the verified head of the content log and the entry keys, as Bob holds them after Alice gave
// them to him; a destroyed entry has none.
func (f *fixture) headAndKeys(t *testing.T) (kololog.VerifiedHead, []kololog.EntryKey) {
	t.Helper()
	l := f.e.content
	h, err := l.Head()
	if err != nil {
		t.Fatal(err)
	}
	head, err := kololog.VerifyHead(f.e.id.PublicKey(), l.ID(), h)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]kololog.EntryKey, l.Len())
	for i := range keys {
		if k, err := l.Key(uint64(i)); err == nil {
			keys[i] = k
		}
	}
	return head, keys
}

// A pin double that sees every byte Bob sends never receives a key, and an honest one serves every file.
func TestPinDoubleNeverSeesAKey(t *testing.T) {
	checkGoroutines(t)
	f := newFixture(t)
	d := f.newPinDouble(t, f.secrets(t))
	pid := newIdentity(t)
	addr := d.serve(pid)
	head, keys := f.headAndKeys(t)
	big := f.files["documents/big-file.bin"]
	got, err := f.pinRead(t, d, addr, pid.PublicKey(), head, keys, pinAttack{at: -1, all: true})
	if err != nil || !bytes.Equal(got, big[bigOffset:bigOffset+10]) {
		t.Fatalf("honest pin: %v", err)
	}
}

// Cancelling the context of OpenContext ends a read of remote content with context.Canceled, without a request.
func TestOpenContextCancelEndsRemoteRead(t *testing.T) {
	checkGoroutines(t)
	f := newFixture(t)
	d := f.newPinDouble(t, f.secrets(t))
	pid := newIdentity(t)
	addr := d.serve(pid)
	head, keys := f.headAndKeys(t)
	got, err := f.pinRead(t, d, addr, pid.PublicKey(), head, keys, pinAttack{at: -1, cancel: true})
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("read after cancel = %q, %v", got, err)
	}
}

// A hostile pin can stop a read but never make it return wrong bytes, and each attack ends in the error that names
// it.
func TestHostilePin(t *testing.T) {
	checkGoroutines(t)
	f := newFixture(t)
	d := f.newPinDouble(t, f.secrets(t))
	pid := newIdentity(t)
	addr := d.serve(pid)
	head, keys := f.headAndKeys(t)
	// edit decodes an honest FETCH answer, lets fn change its entries and encodes the result.
	edit := func(fn func(entries [][5][]byte) [][5][]byte) func(uint64, []byte) []byte {
		return func(_ uint64, honest []byte) []byte {
			dec := kolocodec.NewDecoder(honest[4:])
			dec.Uvarint()
			dec.Uvarint()
			n, _ := dec.Uvarint()
			var entries [][5][]byte
			for range n {
				var e [5][]byte
				for j := range e {
					e[j], _ = dec.Bytes(kolosync.MaxMessageSize)
				}
				entries = append(entries, e)
			}
			entries = fn(entries)
			var e kolocodec.Encoder
			e.PutUvarint(1)
			e.PutUvarint(11)
			e.PutUvarint(uint64(len(entries)))
			for _, en := range entries {
				for _, b := range en {
					e.PutBytes(b)
				}
			}
			return frame(e.Bytes())
		}
	}
	set := func(field int, b []byte) func(uint64, []byte) []byte {
		return edit(func(es [][5][]byte) [][5][]byte { es[0][field] = b; return es })
	}
	flip := func(field int) func(uint64, []byte) []byte {
		return edit(func(es [][5][]byte) [][5][]byte {
			es[0][field] = bytes.Clone(es[0][field])
			es[0][field][len(es[0][field])/2] ^= 1
			return es
		})
	}
	size := f.e.content.Len()
	answers := map[string]struct {
		answer func(uint64, []byte) []byte
		want   error
		or     error // another documented error for the same bytes
	}{
		"wrong ciphertext":     {flip(1), kolosync.ErrInvalid, nil},
		"wrong proof":          {flip(4), kolosync.ErrInvalid, nil},
		"wrong record":         {flip(0), kolosync.ErrInvalid, nil},
		"forged delete marker": {set(2, bytes.Repeat([]byte{1}, 160)), kolosync.ErrInvalid, nil},
		"a key in the answer":  {set(3, bytes.Repeat([]byte{7}, 32)), kolosync.ErrInvalid, nil},
		"missing entries":      {edit(func([][5][]byte) [][5][]byte { return nil }), kolosync.ErrProtocol, nil},
		// Well formed, so only the entry count rejects it.
		"an extra entry": {edit(func(es [][5][]byte) [][5][]byte {
			return append(es, [5][]byte{es[0][0], nil, nil, nil, es[0][4]})
		}), kolosync.ErrProtocol, nil},
		// The content is random, so two entries are sometimes over MaxMessageSize: then the frame is refused as too
		// large, before its entries are read. Either way the answer is rejected.
		"extra entries": {func(start uint64, _ []byte) []byte {
			return d.fetch(start, min(start+1, size-1), size)
		}, kolosync.ErrProtocol, kolosync.ErrTooLarge},
		"another entry": {func(start uint64, _ []byte) []byte {
			return d.fetch((start+1)%size, (start+1)%size, size)
		}, kolosync.ErrInvalid, nil},
		"not found":          {func(uint64, []byte) []byte { return frame([]byte{1, 9, 1}) }, kolosync.ErrNotFound, nil},
		"truncated response": {func(_ uint64, honest []byte) []byte { return honest[:len(honest)/2] }, kolosync.ErrProtocol, nil},
		"oversize frame": {func(uint64, []byte) []byte {
			return binary.LittleEndian.AppendUint32(nil, kolosync.MaxMessageSize+1)
		}, kolosync.ErrTooLarge, nil},
	}
	type tc struct {
		h        pinAttack
		want, or error
	}
	cases := map[string]tc{
		"head of another log": {pinAttack{at: -1, head: func([]byte) []byte {
			other := newEnv(t)
			if _, err := other.content.Append([]byte("another log")); err != nil {
				t.Fatal(err)
			}
			oh, err := other.content.Head()
			if err != nil {
				t.Fatal(err)
			}
			return headFrame(oh.Encode(), other.content.Header().Encode())
		}}, kolosync.ErrInvalid, nil},
		"garbage head":            {pinAttack{at: -1, head: func([]byte) []byte { return frame([]byte{1, 5, 3, 1, 2, 3}) }}, kolosync.ErrProtocol, nil},
		"same size, other root":   {pinAttack{at: -1, head: func([]byte) []byte { return f.forkedHead(t, head) }}, kolosync.ErrDiverged, nil},
		"stalled pin at the head": {pinAttack{at: -1, stall: true}, context.Canceled, nil},
	}
	for _, at := range []int{0, 1, 3} {
		where := fmt.Sprintf("answer %d", at)
		for name, a := range answers {
			cases[name+" at "+where] = tc{pinAttack{at: at, answer: a.answer}, a.want, a.or}
		}
		cases["no key held at "+where] = tc{pinAttack{at: at, noKey: true}, kololog.ErrKeyUnavailable, nil}
		cases["stalled pin at "+where] = tc{pinAttack{at: at, stall: true}, context.Canceled, nil}
	}
	want := f.files["documents/big-file.bin"][bigOffset : bigOffset+10]
	for name, x := range cases {
		for _, mode := range []readMode{readPlain, readKeep, readPrefetch} {
			t.Run(fmt.Sprintf("%s, mode %d", name, mode), func(t *testing.T) {
				x.h.mode = mode
				got, err := f.pinRead(t, d, addr, pid.PublicKey(), head, copies(keys), x.h)
				if !errors.Is(err, x.want) && (x.or == nil || !errors.Is(err, x.or)) {
					t.Fatalf("read = %q, %v; want %v or %v", got, err, x.want, x.or)
				}
				if len(got) != 0 && !bytes.Equal(got, want[:len(got)]) {
					t.Fatalf("wrong bytes returned: %q", got)
				}
			})
		}
	}
}

func copies(keys []kololog.EntryKey) []kololog.EntryKey {
	out := make([]kololog.EntryKey, len(keys))
	for i, k := range keys {
		out[i] = bytes.Clone(k)
	}
	return out
}

// The double itself reports a request off the list, a key in a request and a key it holds.
func TestPinDoubleChecks(t *testing.T) {
	checkGoroutines(t)
	f := newFixture(t)
	var key kololog.EntryKey
	for i := range f.e.content.Len() {
		if k, err := f.e.content.Key(i); err == nil {
			key = k
			break
		}
	}
	d := f.newPinDouble(t, [][]byte{key})
	var mu sync.Mutex
	var seen []string
	d.errorf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf(format, args...))
	}
	pid := newIdentity(t)
	addr := d.serve(pid)
	conn, err := kolosecure.Dial(testCtx(t), addr, newIdentity(t), pid.PublicKey(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(payload []byte) {
		st, err := conn.OpenStream(testCtx(t))
		if err != nil {
			t.Fatal(err)
		}
		st.Write(frame(payload))
		io.ReadFull(st, make([]byte, 4))
		st.Close()
	}
	var e kolocodec.Encoder
	e.PutUvarint(1)
	e.PutUvarint(1)
	e.PutFixed(f.e.id.PublicKey())
	e.PutFixed(key) // a key where the log ID goes
	send(e.Bytes())
	send(append(bytes.Clone(d.headReq), 0)) // one byte past a HEAD_REQ
	var g kolocodec.Encoder
	for _, x := range []uint64{1, 10, 0, 0, f.e.content.Len(), 5} { // a FETCH_REQ with a field more
		g.PutUvarint(x)
	}
	send(g.Bytes())
	st, err := conn.OpenStream(testCtx(t)) // a partial frame that holds a key, then the end of the stream
	if err != nil {
		t.Fatal(err)
	}
	st.Write(binary.LittleEndian.AppendUint32(nil, 100))
	st.Write(key)
	st.Close()
	io.ReadFull(st, make([]byte, 1))
	if err := d.rep.AddKey(0, key); err != nil {
		t.Fatal(err)
	}
	d.fetch(0, 0, f.e.content.Len())
	mu.Lock()
	defer mu.Unlock()
	count := map[string]int{}
	for _, s := range seen {
		for _, class := range []string{"received secret", "off the list", "partial request", "holds the key"} {
			if strings.Contains(s, class) {
				count[class]++
			}
		}
	}
	want := map[string]int{"received secret": 2, "off the list": 3, "partial request": 1, "holds the key": 1}
	if !maps.Equal(count, want) {
		t.Fatalf("the double reported %v, want %v; reports: %q", count, want, seen)
	}
}
