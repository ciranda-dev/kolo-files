package kolofiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// readCreate reads the CREATE op of node id at entry i with its file key; the caller clears o.key.
func (v *Volume) readCreate(i uint64, id NodeID) (*op, error) {
	b, err := v.get(i)
	if errors.Is(err, kololog.ErrDeleted) {
		return nil, fmt.Errorf("kolofiles: the file was deleted: %w", err)
	}
	if err != nil {
		return nil, readErr(err)
	}
	o, err := decodeOp(b, v.opts.maxName, true)
	clear(b)
	if err != nil {
		return nil, entryErr(i, err)
	}
	if o.kind != kindCreate || o.node != id {
		clear(o.key[:])
		return nil, fmt.Errorf("%w: entry %d is not the file's create op", ErrCorrupt, i)
	}
	return o, nil
}

// readVersion reads the content fields of node id at entry i, a CREATE or a WRITE op.
func (v *Volume) readVersion(i uint64, id NodeID) (version, error) {
	b, err := v.get(i)
	if errors.Is(err, kololog.ErrDeleted) {
		return version{}, fmt.Errorf("kolofiles: the file was deleted: %w", err)
	}
	if err != nil {
		return version{}, readErr(err)
	}
	o, err := decodeOp(b, v.opts.maxName, false)
	clear(b)
	if err != nil {
		return version{}, entryErr(i, err)
	}
	if (o.kind != kindCreate && o.kind != kindWrite) || o.node != id {
		return version{}, fmt.Errorf("%w: entry %d is not a version of the file", ErrCorrupt, i)
	}
	return o.ver, nil
}

// fetchFunc returns how this volume gets content log entries one at a time, or nil for a reader on a content backend.
func (v *Volume) fetchFunc() koloblobs.FetchFunc {
	if v.writer() {
		return func(ctx context.Context, i uint64) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return v.content.Get(i)
		}
	}
	return v.fetch
}

func locateIn(locs []location) koloblobs.LocateFunc {
	return func(h koloblobs.Hash) (uint64, bool) { return findLocation(locs, h) }
}

// counted counts the objects a backend returns.
type counted struct {
	b koloblobs.Backend
	n *atomic.Uint64
}

func (c counted) Get(ctx context.Context, h koloblobs.Hash) ([]byte, error) {
	obj, err := c.b.Get(ctx, h)
	if err == nil {
		c.n.Add(1)
	}
	return obj, err
}

func (c counted) Put(context.Context, koloblobs.Hash, []byte) error { return koloblobs.ErrReadOnly }

// touched records the objects of a MarkKeepLocal file that reads went through.
type touched struct {
	mu   sync.Mutex
	objs map[koloblobs.Hash]struct{}
}

// kept serves a MarkKeepLocal file's objects from the local backend and stores those it fetches there.
type kept struct {
	v       *Volume
	id      NodeID
	created uint64
	prefix  []byte
	base    koloblobs.Backend
	t       *touched
}

// objectOK reports whether obj is an object of the right size whose SHA-256 is h.
func objectOK(h koloblobs.Hash, obj []byte) bool {
	return len(obj) >= koloblobs.ObjectOverhead && len(obj) <= koloblobs.MaxObjectSize && sha256.Sum256(obj) == h
}

// Get checks every object before it is kept or served, and drops a kept object that fails the check.
func (k *kept) Get(ctx context.Context, h koloblobs.Hash) ([]byte, error) {
	key := append(bytes.Clone(k.prefix), h[:]...)
	obj, err := k.v.local.Get(key)
	if err == nil && !objectOK(h, obj) {
		if err = k.v.local.Delete(key); err != nil {
			return nil, fmt.Errorf("kolofiles: remove local object: %w", err)
		}
		err = kolostore.ErrNotFound
	}
	switch {
	case err == nil:
		k.v.localHits.Add(1)
	case errors.Is(err, kolostore.ErrNotFound):
		if obj, err = k.base.Get(ctx, h); err != nil {
			return nil, err
		}
		if !objectOK(h, obj) {
			return nil, fmt.Errorf("kolofiles: object does not match its hash: %w", koloblobs.ErrCorrupt)
		}
		if err := k.keep(key, obj); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("kolofiles: local object: %w", err)
	}
	k.t.mu.Lock()
	k.t.objs[h] = struct{}{}
	k.t.mu.Unlock()
	return obj, nil
}

// keep stores obj unless the volume is closed or the node is gone, replaced or no longer MarkKeepLocal; it holds mu so that no clean-up runs
// between the check and the Put and misses the object.
func (k *kept) keep(key, obj []byte) error {
	k.v.mu.RLock()
	defer k.v.mu.RUnlock()
	st := k.v.st
	if st == nil {
		return nil
	}
	if n := st.nodes[k.id]; n == nil || n.created != k.created || k.v.effectiveMark(st, k.id) != MarkKeepLocal {
		return nil
	}
	if err := k.v.local.Put(key, obj); err != nil {
		return fmt.Errorf("kolofiles: keep object: %w", err)
	}
	return nil
}

func (k *kept) Put(context.Context, koloblobs.Hash, []byte) error { return koloblobs.ErrReadOnly }

// openContent opens the current content of file id, node n. With keep, objects are read through the local
// backend and stored there, and the returned set records them.
func (v *Volume) openContent(ctx context.Context, id NodeID, n *node, keep bool) (*koloblobs.Reader, *touched, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	c, err := v.readCreate(n.created, id)
	if err != nil {
		return nil, nil, err
	}
	key := c.key
	clear(c.key[:])
	defer clear(key[:])
	ver := c.ver
	if n.version != n.created {
		if ver, err = v.readVersion(n.version, id); err != nil {
			return nil, nil, err
		}
	}
	if ver.blob != n.blob || ver.size != n.size {
		return nil, nil, fmt.Errorf("%w: entry %d does not match the tree", ErrCorrupt, n.version)
	}
	var t *touched
	if keep {
		t = &touched{objs: map[koloblobs.Hash]struct{}{}}
	}
	layer := func(b koloblobs.Backend) koloblobs.Backend {
		b = counted{b, &v.fetched}
		if !keep {
			return b
		}
		return &kept{v: v, id: id, created: n.created, prefix: v.objectPrefix(id, n.created), base: b, t: t}
	}
	var b koloblobs.Backend
	if fetch := v.fetchFunc(); fetch != nil {
		locs, err := v.readTable(ctx, ver, key, layer(koloblobs.NewRemoteBackend(fetch, locateIn(ver.locs))))
		if err != nil {
			return nil, nil, err
		}
		b = layer(koloblobs.NewRemoteBackend(fetch, locateIn(locs)))
	} else {
		b = layer(v.backend)
	}
	r, err := koloblobs.New(b).Open(ctx, ver.blob, key)
	if err != nil {
		return nil, nil, fmt.Errorf("kolofiles: open content: %w", err)
	}
	if uint64(r.Size()) != ver.size {
		r.Close()
		return nil, nil, fmt.Errorf("%w: the blob's size is not the file's", ErrCorrupt)
	}
	return r, t, nil
}

// readTable reads and checks the object table of a version through b; its size is the one the op gives, and
// memory follows the bytes the table blob holds, not the size it claims.
func (v *Volume) readTable(ctx context.Context, ver version, key koloblobs.Key, b koloblobs.Backend) ([]location, error) {
	r, err := koloblobs.New(b).Open(ctx, ver.table, key)
	if err != nil {
		return nil, fmt.Errorf("kolofiles: open object table: %w", err)
	}
	defer r.Close()
	if uint64(r.Size()) != ver.tableSize {
		return nil, fmt.Errorf("%w: the object table's size is not the one its op gives", ErrCorrupt)
	}
	return decodeTable(r, ver.size, ver.blob)
}

// localPrefix starts every key of this volume in the local backend.
func (v *Volume) localPrefix(kind byte) []byte {
	p := make([]byte, 0, len(localTag)+len(v.id)+1+len(NodeID{})+8+koloblobs.HashSize)
	return append(append(append(p, localTag...), v.id[:]...), kind)
}

// nodePrefix ends with the entry of the node's create op: a node that is not in the tree, with a create entry
// inside the replayed prefix, was removed, while one with a later create entry is only not replayed yet.
func (v *Volume) nodePrefix(kind byte, id NodeID, created uint64) []byte {
	return binary.BigEndian.AppendUint64(append(v.localPrefix(kind), id[:]...), created)
}

func (v *Volume) objectPrefix(id NodeID, created uint64) []byte {
	return v.nodePrefix(localObject, id, created)
}

func (v *Volume) markKey(id NodeID, created uint64) []byte {
	return v.nodePrefix(localMark, id, created)
}

// localTag and the kinds below lay out the local backend: tag ‖ volume ID ‖ 'm' ‖ node ID ‖ create entry holds a
// mark, and tag ‖ volume ID ‖ 'o' ‖ node ID ‖ create entry ‖ object hash holds an object of a MarkKeepLocal file.
const (
	localTag    = "kolo-files/v0/"
	localMark   = 'm'
	localObject = 'o'
)
