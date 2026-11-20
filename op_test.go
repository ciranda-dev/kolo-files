package kolofiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kolocodec "github.com/ciranda-dev/kolo-codec"
	kololog "github.com/ciranda-dev/kolo-log"
)

// file returns a CREATE op of a small valid-looking version, for hostile logs.
func fileOp(node, parent NodeID, name string) *op {
	return &op{kind: kindCreate, node: node, parent: parent, name: name, ver: smallVersion()}
}

func smallVersion() version {
	return version{blob: koloblobs.ID{9}, size: 3, table: koloblobs.ID{8}, tableSize: 60, locs: []location{{koloblobs.Hash{8}, 4}}}
}

func TestOpRoundTrip(t *testing.T) {
	ops := []*op{
		{kind: kindVolume, volume: kololog.LogID{1}, contentLog: kololog.LogID{2}},
		{kind: kindMkdir, node: NodeID{1}, parent: NodeID{2}, name: "d", time: -5},
		{kind: kindCreate, node: NodeID{1}, parent: NodeID{2}, name: "f", time: 1 << 60, key: koloblobs.Key{7}, ver: smallVersion()},
		{kind: kindWrite, node: NodeID{1}, time: 3, ver: smallVersion()},
		{kind: kindMove, node: NodeID{1}, parent: NodeID{3}, name: "g"},
		{kind: kindRemove, node: NodeID{1}},
		{kind: kindPurge, node: NodeID{1}, meta: []uint64{1, 2, 9}, objs: []uint64{0, math.MaxUint64}, done: true},
		{kind: kindPurge, node: NodeID{1}},
	}
	for _, o := range ops {
		b := encodeOp(o)
		got, err := decodeOp(b, DefaultMaxNameLength, true)
		if err != nil {
			t.Fatalf("kind %d: %v", o.kind, err)
		}
		if !bytes.Equal(encodeOp(got), b) {
			t.Fatalf("kind %d does not round trip", o.kind)
		}
	}
	// Without its key, a CREATE decodes with a zero key.
	got, err := decodeOp(encodeOp(ops[2]), DefaultMaxNameLength, false)
	if err != nil || got.key != (koloblobs.Key{}) {
		t.Fatalf("key kept: %v", err)
	}
}

// raw builds an entry field by field.
func raw(f func(e *kolocodec.Encoder)) []byte {
	var e kolocodec.Encoder
	f(&e)
	return e.Bytes()
}

func head(e *kolocodec.Encoder, kind uint64) {
	e.PutString(opTag)
	e.PutUvarint(kind)
	e.PutUvarint(0)
}

func TestDecodeRefuses(t *testing.T) {
	node := make([]byte, 16)
	h32 := make([]byte, 32)
	mkdirWith := func(name string) []byte {
		return raw(func(e *kolocodec.Encoder) {
			head(e, kindMkdir)
			e.PutFixed(node)
			e.PutFixed(node)
			e.PutString(name)
			e.PutVarint(0)
		})
	}
	versionWith := func(f func(e *kolocodec.Encoder)) []byte {
		return raw(func(e *kolocodec.Encoder) {
			head(e, kindWrite)
			e.PutFixed(node)
			e.PutVarint(0)
			f(e)
		})
	}
	good := encodeOp(&op{kind: kindMkdir, name: "a"})
	nonMinimal := bytes.Clone(good)
	nonMinimal = append(nonMinimal[:len(nonMinimal)-1], 0x80, 0x00) // the time 0 as two bytes
	for _, tc := range []struct {
		name string
		b    []byte
		want error
	}{
		{"other tag", raw(func(e *kolocodec.Encoder) { e.PutString("kolo-kv/op/v0") }), ErrCorrupt},
		{"unknown kind", raw(func(e *kolocodec.Encoder) { head(e, kindPurge+1) }), ErrCorrupt},
		{"deps", raw(func(e *kolocodec.Encoder) {
			e.PutString(opTag)
			e.PutUvarint(kindRemove)
			e.PutUvarint(1)
			e.PutFixed(h32)
			e.PutUvarint(3)
			e.PutFixed(node)
		}), ErrCorrupt},
		{"trailing bytes", append(bytes.Clone(good), 0), ErrCorrupt},
		{"short", good[:len(good)-1], ErrCorrupt},
		{"non-minimal varint", nonMinimal, ErrCorrupt},
		{"too large", append(bytes.Clone(good), make([]byte, maxEntry)...), ErrCorrupt},
		{"empty name", mkdirWith(""), ErrCorrupt},
		{"dot", mkdirWith("."), ErrCorrupt},
		{"dot dot", mkdirWith(".."), ErrCorrupt},
		{"slash", mkdirWith("a/b"), ErrCorrupt},
		{"control", mkdirWith("a\x00"), ErrCorrupt},
		{"C1 control", mkdirWith("a\u0080"), ErrCorrupt},
		{"not NFC", mkdirWith("café"), ErrCorrupt},
		{"invalid UTF-8", mkdirWith("a\xff"), ErrCorrupt},
		{"long name", mkdirWith(strings.Repeat("a", 2000)), ErrLimit},
		{"list longer than the entry", raw(func(e *kolocodec.Encoder) {
			head(e, kindPurge)
			e.PutFixed(node)
			e.PutUvarint(1000)
			e.PutUvarint(1)
		}), ErrCorrupt},
		{"list overflows", raw(func(e *kolocodec.Encoder) {
			head(e, kindPurge)
			e.PutFixed(node)
			e.PutUvarint(2)
			e.PutUvarint(math.MaxUint64 - 1)
			e.PutUvarint(1)
			e.PutUvarint(0)
			e.PutBool(true)
		}), ErrCorrupt},
		{"bad bool", raw(func(e *kolocodec.Encoder) {
			head(e, kindPurge)
			e.PutFixed(node)
			e.PutUvarint(0)
			e.PutUvarint(0)
			e.PutUvarint(2)
		}), ErrCorrupt},
		{"size over MaxSize", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(math.MaxUint64)
		}), ErrCorrupt},
		{"table over its bound", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(10)
			e.PutFixed(h32)
			e.PutUvarint(maxTableSize(maxObjects(10)) + 1)
		}), ErrCorrupt},
		{"table locations over their bound", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(10)
			e.PutFixed(h32)
			e.PutUvarint(60)
			e.PutUvarint(maxObjects(60) + 1)
		}), ErrCorrupt},
		{"table locations longer than the entry", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(1 << 40)
			e.PutFixed(h32)
			e.PutUvarint(1 << 30)
			e.PutUvarint(1000)
			e.PutFixed(h32)
			e.PutUvarint(0)
		}), ErrCorrupt},
		{"table locations not ascending", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(10)
			e.PutFixed(h32)
			e.PutUvarint(60)
			e.PutUvarint(2)
			e.PutFixed(h32)
			e.PutUvarint(0)
			e.PutFixed(h32)
			e.PutUvarint(1)
		}), ErrCorrupt},
		{"table blob not located", versionWith(func(e *kolocodec.Encoder) {
			e.PutFixed(h32)
			e.PutUvarint(10)
			e.PutFixed(h32)
			e.PutUvarint(60)
			e.PutUvarint(1)
			e.PutFixed(append([]byte{1}, h32[1:]...))
			e.PutUvarint(0)
		}), ErrCorrupt},
	} {
		if _, err := decodeOp(tc.b, DefaultMaxNameLength, true); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

func decodeTableBytes(b []byte, size uint64, blob koloblobs.ID) ([]location, error) {
	return decodeTable(bytes.NewReader(b), size, blob)
}

func TestTableBounds(t *testing.T) {
	for _, tc := range []struct{ size, want uint64 }{
		{0, 1}, {1, 2}, {koloblobs.MinChunkSize, 2}, {koloblobs.MinChunkSize + 1, 3},
		{256 * koloblobs.MinChunkSize, 257}, {256*koloblobs.MinChunkSize + 1, 260},
	} {
		if got := maxObjects(tc.size); got != tc.want {
			t.Errorf("maxObjects(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
	if n := maxObjects(koloblobs.MaxSize); n < 1<<45 || n > 1<<46 {
		t.Fatalf("maxObjects(MaxSize) = %d", n)
	}
	// A real table fits its bound, and decodeTable refuses a table that does not list the blob.
	locs := []location{{koloblobs.Hash{1}, 5}, {koloblobs.Hash{2}, 1 << 40}}
	b := encodeTable(locs)
	if uint64(len(b)) > maxTableSize(2) {
		t.Fatal("table over its bound")
	}
	if _, err := decodeTableBytes(b, 1, koloblobs.ID{2}); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTableBytes(b, 1, koloblobs.ID{3}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("table without the root: %v", err)
	}
	if _, err := decodeTableBytes(b, 0, koloblobs.ID{2}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("table over the objects of its size: %v", err)
	}
	if _, err := decodeTableBytes(append(b, 0), 1, koloblobs.ID{2}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("table with trailing bytes: %v", err)
	}
	if _, err := decodeTableBytes(encodeOp(&op{kind: kindRemove}), 1, koloblobs.ID{2}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("not a table: %v", err)
	}
}

func TestReplayRules(t *testing.T) {
	a, b, c := NodeID{1}, NodeID{2}, NodeID{3}
	for _, tc := range []struct {
		name string
		ops  []*op
		want error
	}{
		{"volume after the first", []*op{{kind: kindVolume}}, ErrCorrupt},
		{"create twice", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: a, name: "b"}}, ErrCorrupt},
		{"create the root", []*op{{kind: kindMkdir, node: rootID, name: "a"}}, ErrCorrupt},
		{"create a parent of earlier nodes", []*op{{kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindMkdir, node: a, parent: b, name: "a"}}, ErrCorrupt},
		{"create on a taken name", []*op{{kind: kindMkdir, node: a, name: "a"}, fileOp(b, rootID, "a")}, ErrCorrupt},
		{"create in a file", []*op{fileOp(a, rootID, "a"), {kind: kindMkdir, node: b, parent: a, name: "b"}}, ErrCorrupt},
		{"write a missing node", []*op{{kind: kindWrite, node: a, ver: smallVersion()}}, ErrCorrupt},
		{"write the root", []*op{{kind: kindWrite, node: rootID, ver: smallVersion()}}, ErrCorrupt},
		{"write a directory", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindWrite, node: a, ver: smallVersion()}}, ErrCorrupt},
		{"move a missing node", []*op{{kind: kindMove, node: a, name: "a"}}, ErrCorrupt},
		{"move the root", []*op{{kind: kindMove, node: rootID, parent: a, name: "a"}}, ErrCorrupt},
		{"move into itself", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMove, node: a, parent: a, name: "x"}}, ErrCorrupt},
		{"move into its subtree", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindMkdir, node: c, parent: b, name: "c"}, {kind: kindMove, node: a, parent: c, name: "x"}}, ErrCorrupt},
		{"move onto a taken name", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, name: "b"}, {kind: kindMove, node: a, name: "b"}}, ErrCorrupt},
		{"move into a file", []*op{fileOp(a, rootID, "a"), {kind: kindMkdir, node: b, name: "b"}, {kind: kindMove, node: b, parent: a, name: "b"}}, ErrCorrupt},
		{"remove the root", []*op{{kind: kindRemove, node: rootID}}, ErrCorrupt},
		{"remove a missing node", []*op{{kind: kindRemove, node: a}}, ErrCorrupt},
		{"remove twice", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindRemove, node: a}}, ErrCorrupt},
		{"purge without remove", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindPurge, node: a, meta: []uint64{1}, done: true}}, ErrCorrupt},
		{"purge after done", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, done: true}, {kind: kindPurge, node: a, done: true}}, ErrCorrupt},
		{"purge lists entry 0", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{0}, done: true}}, ErrCorrupt},
		{"purge lists itself", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{3}, done: true}}, ErrCorrupt},
		{"purge lists a later entry", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{4}, done: true}}, ErrCorrupt},
		{"purge lists a remove", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{2}, done: true}}, ErrCorrupt},
		{"purge lists a purge", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a}, {kind: kindPurge, node: a, meta: []uint64{3}, done: true}}, ErrCorrupt},
		{"purge lists an entry twice", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1}}, {kind: kindPurge, node: a, meta: []uint64{1}, done: true}}, ErrCorrupt},
		{"create a removed node", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindMkdir, node: a, name: "x"}}, ErrCorrupt},
		{"create a node dropped with its parent", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindRemove, node: a}, {kind: kindMkdir, node: b, name: "b"}}, ErrCorrupt},
		{"create a purged node", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1}, done: true}, {kind: kindMkdir, node: a, name: "a"}}, ErrCorrupt},
		{"create a purged child", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1, 2}, done: true}, {kind: kindMkdir, node: b, name: "b"}}, ErrCorrupt},
		{"purge lists an op about another node", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, name: "b"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{2}, done: true}}, ErrCorrupt},
		{"purge lists an op about a node moved out", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindMove, node: b, name: "b"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{2}, done: true}}, ErrCorrupt},
		{"purge lists a write of a node with a broken chain", []*op{{kind: kindMkdir, node: a, name: "a"}, fileOp(c, b, "c"), {kind: kindWrite, node: c, ver: smallVersion()}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{3}, done: true}}, ErrCorrupt},
		// Valid logs.
		{"purge lists the subtree", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1, 2}, done: true}}, nil},
		{"purge lists a node moved in", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMkdir, node: b, name: "b"}, {kind: kindMove, node: b, parent: a, name: "b"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1, 2, 3}, done: true}}, nil},
		{"orphan create and move out", []*op{{kind: kindMkdir, node: b, parent: a, name: "b"}, {kind: kindMove, node: b, name: "b"}}, nil},
		{"remove then purge", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindPurge, node: a, meta: []uint64{1}}, {kind: kindPurge, node: a, done: true}}, nil},
		{"same name after remove", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindRemove, node: a}, {kind: kindMkdir, node: b, name: "a"}}, nil},
		{"move to the same place", []*op{{kind: kindMkdir, node: a, name: "a"}, {kind: kindMove, node: a, name: "a"}}, nil},
		{"unassigned code point", []*op{{kind: kindMkdir, node: a, name: "͸"}}, nil},
	} {
		if err := hostile(t, tc.ops...); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		} else if err != nil {
			wantPrefix(t, err)
		}
	}
	// The first entry must be this volume.
	e := newEnv(t)
	appendRaw(t, e.meta, encodeOp(&op{kind: kindMkdir, node: a, name: "a"}))
	if _, err := Open(e.meta, e.content); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("no volume entry: %v", err)
	}
	e = newEnv(t)
	appendRaw(t, e.meta, encodeOp(&op{kind: kindVolume, volume: kololog.LogID{1}, contentLog: e.content.ID()}))
	if _, err := Open(e.meta, e.content); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("another volume's entry: %v", err)
	}
	// On the writer, a destroyed entry that no purge lists is corrupt.
	e = newEnv(t)
	appendRaw(t, e.meta, encodeOp(&op{kind: kindVolume, volume: e.meta.ID(), contentLog: e.content.ID()}), encodeOp(&op{kind: kindMkdir, node: a, name: "a"}))
	if err := e.meta.Delete(1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(e.meta, e.content); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unlisted destroyed entry: %v", err)
	}
}

// A table that claims far more locations than it holds costs memory for the bytes read, not for the claim.
func TestDecodeTableHostile(t *testing.T) {
	root := koloblobs.ID{1}
	claim := func(n uint64) []byte {
		return raw(func(e *kolocodec.Encoder) {
			e.PutString(tableTag)
			e.PutUvarint(n)
			for i := range 3 {
				e.PutFixed(append(make([]byte, 31), byte(i+1)))
				e.PutUvarint(uint64(i))
			}
		})
	}
	var err error
	if got := allocated(func() {
		_, err = decodeTable(bytes.NewReader(claim(1<<40)), koloblobs.MaxSize, root)
	}); !errors.Is(err, ErrCorrupt) || got > 1<<20 {
		t.Fatalf("claim of 2^40 locations over 3: %v, allocated %d bytes", err, got)
	}
	// A count over what a blob of that size has is refused at once.
	if _, err := decodeTable(bytes.NewReader(claim(3)), 1, root); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("count over the objects of the size: %v", err)
	}
	// A failing source is an error of its own, not ErrCorrupt.
	boom := errors.New("boom")
	if _, err := decodeTable(io.MultiReader(bytes.NewReader(claim(5)[:30]), errReader{boom}), koloblobs.MaxSize, root); !errors.Is(err, boom) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("failing source: %v", err)
	}
	// Entries that are not ascending are refused as they are read.
	desc := raw(func(e *kolocodec.Encoder) {
		e.PutString(tableTag)
		e.PutUvarint(2)
		e.PutFixed(append(make([]byte, 31), 2))
		e.PutUvarint(0)
		e.PutFixed(append(make([]byte, 31), 1))
		e.PutUvarint(0)
	})
	if _, err := decodeTableBytes(desc, 10, root); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("descending table: %v", err)
	}
}

// A version whose claimed table size is not the table blob's is refused before any table is read into memory.
func TestHostileTableSize(t *testing.T) {
	for name, claim := range map[string]func(*version){
		"one byte more": func(v *version) { v.tableSize++ },
		"huge":          func(v *version) { v.size, v.tableSize = koloblobs.MaxSize, 1<<49 },
	} {
		v, e := newVolume(t)
		write(t, v, "f", []byte("content"))
		id := nodeOf(t, v, "f").ID
		e.reopenLogs(t, v)
		b, err := e.meta.Get(1)
		if err != nil {
			t.Fatal(err)
		}
		c, err := decodeOp(b, DefaultMaxNameLength, false)
		if err != nil {
			t.Fatal(err)
		}
		claim(&c.ver)
		appendRaw(t, e.meta, encodeOp(&op{kind: kindWrite, node: id, ver: c.ver}))
		w, err := Open(e.meta, e.content)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		// A reader that fetches entries reads the table too; one with a content backend never does.
		rst := newStore(t)
		meta, content := newReplica(t, rst, e.meta), newReplica(t, rst, e.content)
		grow(t, meta, e.meta, true)
		grow(t, content, e.content, true)
		fv, err := OpenFetchReader(meta, func(_ context.Context, i uint64) ([]byte, error) { return content.Get(i) }, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer fv.Close()
		for who, v := range map[string]*Volume{"writer": w, "fetch reader": fv} {
			var err error
			if got := allocated(func() { _, err = v.Open("f") }); !errors.Is(err, ErrCorrupt) || got > 1<<20 {
				t.Errorf("%s, %s: Open = %v, allocated %d bytes", name, who, err, got)
			}
			wantPrefix(t, err)
		}
	}
}

// purgeItems is the most that fit one entry at the longest varint per index, and not one fewer than that.
func TestPurgeItemsFit(t *testing.T) {
	fixed := len(encodeOp(&op{kind: kindPurge})) + 2*binary.MaxVarintLen64
	if purgeItems*binary.MaxVarintLen64+fixed > maxEntry || (purgeItems+1)*binary.MaxVarintLen64+fixed <= maxEntry {
		t.Fatalf("purgeItems = %d for a fixed part of %d bytes under %d", purgeItems, fixed, maxEntry)
	}
	meta := make([]uint64, purgeItems)
	for i := range meta {
		meta[i] = uint64(i) << 56 // eight bytes or more per index when put as a first value or a gap
	}
	if n := len(encodeOp(&op{kind: kindPurge, node: NodeID{1}, meta: meta})); n > maxEntry {
		t.Fatalf("a PURGE op of %d items takes %d bytes, over %d", purgeItems, n, maxEntry)
	}
}
