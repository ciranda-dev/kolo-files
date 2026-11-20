package kolofiles

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kolocodec "github.com/ciranda-dev/kolo-codec"
	kololog "github.com/ciranda-dev/kolo-log"
)

// opTag starts every metadata entry; it versions the op format and marks the log as a kolo-files log.
const opTag = "kolo-files/op/v0"

// tableTag starts the plaintext of every object table blob.
const tableTag = "kolo-files/table/v0"

const (
	kindVolume = iota
	kindMkdir
	kindCreate
	kindWrite
	kindMove
	kindRemove
	kindPurge
)

// maxEntry is the largest op: the largest entry value kolo-sync carries, so every op syncs.
const maxEntry = koloblobs.MaxObjectSize

// locationSize is the fewest bytes one encoded location takes: a hash and a one-byte index.
const locationSize = koloblobs.HashSize + 1

// purgeItems is how many indexes one PURGE op lists at most, at binary.MaxVarintLen64 bytes per index after its
// header and two list counts, so it stays under maxEntry. A var so tests can lower it.
var purgeItems = (maxEntry - len(encodeOp(&op{kind: kindPurge})) - 2*binary.MaxVarintLen64) / binary.MaxVarintLen64

var errBadOp = fmt.Errorf("%w: malformed op", ErrCorrupt)

// location is where one object of a blob is: its hash and the index of the content log entry holding it.
type location struct {
	h koloblobs.Hash
	i uint64
}

// version is the content of a CREATE or WRITE: the blob, its size, its object table blob and that blob's objects.
type version struct {
	blob      koloblobs.ID
	size      uint64
	table     koloblobs.ID
	tableSize uint64
	locs      []location // the table blob's objects, ascending by hash
}

// op is one metadata entry.
type op struct {
	kind         uint64
	node, parent NodeID
	name         string
	time         int64
	key          koloblobs.Key // CREATE, only when decoded with its key; clear it after use
	ver          version
	volume       kololog.LogID // VOLUME
	contentLog   kololog.LogID // VOLUME
	meta, objs   []uint64      // PURGE: metadata and content log entries, ascending
	done         bool          // PURGE: the last of its node's PURGE ops
}

// encodeOp returns the entry of o. For a CREATE the buffer holds the file key and is allocated once, so no copy of
// the key is left in a grown buffer; the caller clears it after use.
func encodeOp(o *op) []byte {
	var e kolocodec.Encoder
	e.PutString(opTag)
	e.PutUvarint(o.kind)
	e.PutUvarint(0) // deps: none, single writer
	switch o.kind {
	case kindVolume:
		e.PutFixed(o.volume[:])
		e.PutFixed(o.contentLog[:])
	case kindMkdir:
		e.PutFixed(o.node[:])
		e.PutFixed(o.parent[:])
		e.PutString(o.name)
		e.PutVarint(o.time)
	case kindCreate:
		e.PutFixed(o.node[:])
		e.PutFixed(o.parent[:])
		e.PutString(o.name)
		e.PutVarint(o.time)
		var rest kolocodec.Encoder
		putVersion(&rest, &o.ver)
		head, tail := e.Bytes(), rest.Bytes()
		b := make([]byte, 0, len(head)+koloblobs.KeySize+len(tail))
		b = append(append(append(b, head...), o.key[:]...), tail...)
		return b
	case kindWrite:
		e.PutFixed(o.node[:])
		e.PutVarint(o.time)
		putVersion(&e, &o.ver)
	case kindMove:
		e.PutFixed(o.node[:])
		e.PutFixed(o.parent[:])
		e.PutString(o.name)
	case kindRemove:
		e.PutFixed(o.node[:])
	case kindPurge:
		e.PutFixed(o.node[:])
		putList(&e, o.meta)
		putList(&e, o.objs)
		e.PutBool(o.done)
	}
	return e.Bytes()
}

func putVersion(e *kolocodec.Encoder, v *version) {
	e.PutFixed(v.blob[:])
	e.PutUvarint(v.size)
	e.PutFixed(v.table[:])
	e.PutUvarint(v.tableSize)
	putLocations(e, v.locs)
}

func putLocations(e *kolocodec.Encoder, locs []location) {
	e.PutUvarint(uint64(len(locs)))
	for _, l := range locs {
		e.PutFixed(l.h[:])
		e.PutUvarint(l.i)
	}
}

// putList writes an ascending list as its length, its first index and each gap minus one.
func putList(e *kolocodec.Encoder, list []uint64) {
	e.PutUvarint(uint64(len(list)))
	for i, v := range list {
		if i > 0 {
			v -= list[i-1] + 1
		}
		e.PutUvarint(v)
	}
}

// decodeOp decodes and checks one entry; names longer than maxName are ErrLimit. Without withKey the file key of a
// CREATE is cleared as soon as it is read.
func decodeOp(b []byte, maxName int, withKey bool) (*op, error) {
	if len(b) > maxEntry {
		return nil, fmt.Errorf("%w: op of %d bytes", ErrCorrupt, len(b))
	}
	d := kolocodec.NewDecoder(b)
	tag, err := d.String(len(opTag))
	if err != nil || tag != opTag {
		return nil, fmt.Errorf("%w: not a kolo-files op", ErrCorrupt)
	}
	o := &op{}
	if o.kind, err = d.UvarintMax(kindPurge); err != nil {
		return nil, errBadOp
	}
	if deps, err := d.Uvarint(); err != nil || deps != 0 {
		return nil, fmt.Errorf("%w: op with dependencies on other writers", ErrCorrupt)
	}
	ok := true
	fixed := func(dst []byte) {
		b, err := d.Fixed(len(dst))
		ok = ok && err == nil
		copy(dst, b)
		clear(b)
	}
	name := func() {
		s, err := d.String(d.Len())
		ok = ok && err == nil
		o.name = s
	}
	tm := func() {
		t, err := d.Varint()
		ok = ok && err == nil
		o.time = t
	}
	switch o.kind {
	case kindVolume:
		fixed(o.volume[:])
		fixed(o.contentLog[:])
	case kindMkdir:
		fixed(o.node[:])
		fixed(o.parent[:])
		name()
		tm()
	case kindCreate:
		fixed(o.node[:])
		fixed(o.parent[:])
		name()
		tm()
		fixed(o.key[:])
		if !withKey {
			clear(o.key[:])
		}
		ok = ok && getVersion(d, &o.ver) == nil
	case kindWrite:
		fixed(o.node[:])
		tm()
		ok = ok && getVersion(d, &o.ver) == nil
	case kindMove:
		fixed(o.node[:])
		fixed(o.parent[:])
		name()
	case kindRemove:
		fixed(o.node[:])
	case kindPurge:
		fixed(o.node[:])
		var err1, err2, err3 error
		o.meta, err1 = getList(d)
		o.objs, err2 = getList(d)
		o.done, err3 = d.Bool()
		ok = ok && err1 == nil && err2 == nil && err3 == nil
	}
	if !ok || d.Done() != nil {
		clear(o.key[:])
		return nil, errBadOp
	}
	if o.kind == kindMkdir || o.kind == kindCreate || o.kind == kindMove {
		if err := checkName(o.name, maxName); err != nil {
			clear(o.key[:])
			return nil, err
		}
	}
	return o, nil
}

// getVersion reads the content fields of a CREATE or WRITE and bounds each count by what the sizes allow.
func getVersion(d *kolocodec.Decoder, v *version) error {
	b, err := d.Fixed(koloblobs.HashSize)
	if err != nil {
		return err
	}
	copy(v.blob[:], b)
	if v.size, err = d.UvarintMax(koloblobs.MaxSize); err != nil {
		return err
	}
	if b, err = d.Fixed(koloblobs.HashSize); err != nil {
		return err
	}
	copy(v.table[:], b)
	if v.tableSize, err = d.UvarintMax(maxTableSize(maxObjects(v.size))); err != nil {
		return err
	}
	if v.locs, err = getLocations(d, maxObjects(v.tableSize)); err != nil {
		return err
	}
	if _, ok := findLocation(v.locs, koloblobs.Hash(v.table)); !ok {
		return errBadOp
	}
	return nil
}

// getLocations reads a list of at most max locations, strictly ascending by hash.
func getLocations(d *kolocodec.Decoder, max uint64) ([]location, error) {
	n, err := d.UvarintMax(max)
	if err != nil {
		return nil, err
	}
	if n > uint64(d.Len()/locationSize) {
		return nil, errBadOp
	}
	locs := make([]location, n)
	for k := range locs {
		b, err := d.Fixed(koloblobs.HashSize)
		if err != nil {
			return nil, err
		}
		copy(locs[k].h[:], b)
		if locs[k].i, err = d.Uvarint(); err != nil {
			return nil, err
		}
		if k > 0 && bytes.Compare(locs[k-1].h[:], locs[k].h[:]) >= 0 {
			return nil, errBadOp
		}
	}
	return locs, nil
}

// getList reads an ascending list of indexes; its count is checked against the bytes left before allocating.
func getList(d *kolocodec.Decoder) ([]uint64, error) {
	n, err := d.Uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(d.Len()) {
		return nil, errBadOp
	}
	list := make([]uint64, n)
	for k := range list {
		v, err := d.Uvarint()
		if err != nil {
			return nil, err
		}
		if k > 0 {
			prev := list[k-1]
			if v >= math.MaxUint64-prev {
				return nil, errBadOp
			}
			v += prev + 1
		}
		list[k] = v
	}
	return list, nil
}

// maxObjects is the most distinct objects a blob of size bytes holds: every chunk but the last is at least
// MinChunkSize, and above them are tree nodes of at most Fanout children, MaxHeight levels at most.
func maxObjects(size uint64) uint64 {
	var chunks uint64
	if size > 0 {
		chunks = (size-1)/koloblobs.MinChunkSize + 1
	}
	total, n := chunks, chunks
	for range koloblobs.MaxHeight {
		n = max((n+koloblobs.Fanout-1)/koloblobs.Fanout, 1)
		total += n
		if n == 1 {
			break
		}
	}
	return total
}

// maxTableSize is the largest table plaintext listing n objects: the tag, the count, and n hashes with indexes of
// up to 10 bytes.
func maxTableSize(n uint64) uint64 {
	return 1 + uint64(len(tableTag)) + 10 + n*(koloblobs.HashSize+10)
}

// encodeTable returns the plaintext of an object table: every distinct object of a blob and its entry.
func encodeTable(locs []location) []byte {
	var e kolocodec.Encoder
	e.PutString(tableTag)
	putLocations(&e, locs)
	return e.Bytes()
}

// decodeTable reads the object table of a blob of size bytes whose ID is blob from r, one location at a time: the
// count is checked against the objects a blob of that size holds, and memory grows only with the bytes r delivers.
func decodeTable(r io.Reader, size uint64, blob koloblobs.ID) ([]location, error) {
	br := bufio.NewReader(r)
	bad := fmt.Errorf("%w: malformed object table", ErrCorrupt)
	// next decodes from the bytes ahead with f, then consumes what f used.
	next := func(max int, f func(*kolocodec.Decoder) error) error {
		b, err := br.Peek(max)
		if err != nil && err != io.EOF {
			return fmt.Errorf("kolofiles: read object table: %w", err)
		}
		d := kolocodec.NewDecoder(b)
		if f(d) != nil {
			return bad
		}
		br.Discard(len(b) - d.Len())
		return nil
	}
	var n uint64
	err := next(1+len(tableTag)+binary.MaxVarintLen64, func(d *kolocodec.Decoder) error {
		tag, err := d.String(len(tableTag))
		if err != nil || tag != tableTag {
			return bad
		}
		n, err = d.UvarintMax(maxObjects(size))
		return err
	})
	if err != nil {
		return nil, err
	}
	var locs []location
	for range n {
		var l location
		err := next(koloblobs.HashSize+binary.MaxVarintLen64, func(d *kolocodec.Decoder) error {
			b, err := d.Fixed(koloblobs.HashSize)
			if err != nil {
				return err
			}
			copy(l.h[:], b)
			l.i, err = d.Uvarint()
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(locs) > 0 && bytes.Compare(locs[len(locs)-1].h[:], l.h[:]) >= 0 {
			return nil, bad
		}
		locs = append(locs, l)
	}
	if _, err := br.Peek(1); err == nil {
		return nil, bad
	} else if err != io.EOF {
		return nil, fmt.Errorf("kolofiles: read object table: %w", err)
	}
	if _, ok := findLocation(locs, koloblobs.Hash(blob)); !ok {
		return nil, fmt.Errorf("%w: the object table does not list the blob's root", ErrCorrupt)
	}
	return locs, nil
}

// findLocation returns the entry of h in locs, which is ascending by hash.
func findLocation(locs []location, h koloblobs.Hash) (uint64, bool) {
	k, ok := slices.BinarySearchFunc(locs, h, func(l location, h koloblobs.Hash) int { return bytes.Compare(l.h[:], h[:]) })
	if !ok {
		return 0, false
	}
	return locs[k].i, true
}
