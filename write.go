package kolofiles

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"time"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
)

// writable reports ErrClosed or ErrReadOnly; call it holding writeMu.
func (v *Volume) writable() error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrClosed
	}
	if !v.writer() {
		return ErrReadOnly
	}
	return nil
}

// appendOp appends o to the metadata log, refusing an op larger than kolo-sync carries.
func (v *Volume) appendOp(o *op) (uint64, error) {
	b := encodeOp(o)
	defer clear(b)
	if len(b) > maxEntry {
		return 0, fmt.Errorf("%w: op of %d bytes, over %d", ErrLimit, len(b), maxEntry)
	}
	i, err := v.meta.Append(b)
	if err != nil {
		return 0, fmt.Errorf("kolofiles: append: %w", err)
	}
	v.ops.Add(1)
	return i, nil
}

// commit appends o and applies it to the tree; the caller has checked it against the tree.
func (v *Volume) commit(o *op) (uint64, error) {
	i, err := v.appendOp(o)
	if err != nil {
		return 0, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n = i + 1
	if o.kind >= kindMkdir && o.kind <= kindMove {
		v.byNode[o.node] = append(v.byNode[o.node], i)
	}
	_, err = v.st.apply(i, o, v.opts.maxDepth, v.isPurged)
	return i, err
}

// isPurged reports whether id has a complete PURGE sequence; call it holding mu.
func (v *Volume) isPurged(id NodeID) bool {
	_, ok := v.purged[id]
	return ok
}

// newNodeID returns a random ID that the tree does not hold; call it holding mu.
func (v *Volume) newNodeID() NodeID {
	for {
		var id NodeID
		rand.Read(id[:])
		if id != rootID && v.st.nodes[id] == nil && v.st.kids[id] == nil {
			return id
		}
	}
}

// WriteFile creates the file name, whose parent must exist, or stores new content for an existing file, reading src
// to its end. Only changed chunks are stored; a write killed part way is resumed by calling it again.
func (v *Volume) WriteFile(ctx context.Context, name string, src io.Reader) error {
	pe := func(err error) error { return &fs.PathError{Op: "kolofiles: write", Path: name, Err: err} }
	if ctx == nil || src == nil {
		return pe(ErrInvalidArgument)
	}
	names, err := v.opts.parsePath(name, true)
	if err != nil {
		return pe(err)
	}
	if names == nil {
		return pe(ErrIsDir)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if err := v.writable(); err != nil {
		return pe(err)
	}
	o := &op{time: time.Now().UnixNano(), name: names[len(names)-1]}
	var created uint64
	v.mu.RLock()
	pid, p, err := v.st.lookup(names[:len(names)-1])
	if err == nil && !p.dir {
		err = ErrNotDir
	}
	if err == nil {
		if id, ok := v.st.kids[pid][o.name]; ok {
			n := v.st.nodes[id]
			if n.dir {
				err = ErrIsDir
			}
			o.kind, o.node, created = kindWrite, id, n.created
		} else {
			o.kind, o.node, o.parent = kindCreate, v.newNodeID(), pid
		}
	}
	v.mu.RUnlock()
	if err != nil {
		return pe(err)
	}
	var key koloblobs.Key
	defer clear(key[:])
	if o.kind == kindWrite {
		c, err := v.readCreate(created, o.node)
		if err != nil {
			return pe(err)
		}
		key = c.key
		clear(c.key[:])
	} else {
		key = koloblobs.NewKey()
	}
	if o.ver, err = v.store(ctx, key, src); err != nil {
		return pe(err)
	}
	if o.kind == kindCreate {
		o.key = key
		defer clear(o.key[:])
	}
	if _, err := v.commit(o); err != nil {
		return pe(err)
	}
	return nil
}

type countReader struct {
	r io.Reader
	n uint64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += uint64(n)
	return n, err
}

// store writes src as a blob under key, then its object table as a second blob under the same key, and returns the
// fields of the version's op.
func (v *Volume) store(ctx context.Context, key koloblobs.Key, src io.Reader) (version, error) {
	s := koloblobs.New(v.lb)
	cr := &countReader{r: src}
	id, err := s.Write(ctx, key, cr)
	v.bytesWritten.Add(cr.n)
	if err != nil {
		return version{}, fmt.Errorf("kolofiles: write content: %w", err)
	}
	locs, err := v.locations(ctx, s, id, key)
	if err != nil {
		return version{}, err
	}
	table := encodeTable(locs)
	tid, err := s.Write(ctx, key, bytes.NewReader(table))
	if err != nil {
		return version{}, fmt.Errorf("kolofiles: write object table: %w", err)
	}
	tlocs, err := v.locations(ctx, s, tid, key)
	if err != nil {
		return version{}, err
	}
	return version{blob: id, size: cr.n, table: tid, tableSize: uint64(len(table)), locs: tlocs}, nil
}

// locations lists every distinct object of the blob id with the content log entry holding it, ascending by hash.
func (v *Volume) locations(ctx context.Context, s *koloblobs.Store, id koloblobs.ID, key koloblobs.Key) ([]location, error) {
	seen := map[koloblobs.Hash]uint64{}
	for h, err := range s.Objects(ctx, id, key) {
		if err != nil {
			return nil, fmt.Errorf("kolofiles: list objects: %w", err)
		}
		if _, ok := seen[h]; ok {
			continue
		}
		i, err := v.lb.Index(ctx, h)
		if err != nil {
			return nil, fmt.Errorf("kolofiles: locate object: %w", err)
		}
		seen[h] = i
	}
	locs := make([]location, 0, len(seen))
	for h, i := range seen {
		locs = append(locs, location{h, i})
	}
	slices.SortFunc(locs, func(a, b location) int { return bytes.Compare(a.h[:], b.h[:]) })
	return locs, nil
}

// Mkdir creates the directory name; its parent must exist, and an existing name is fs.ErrExist.
func (v *Volume) Mkdir(name string) error {
	pe := func(err error) error { return &fs.PathError{Op: "kolofiles: mkdir", Path: name, Err: err} }
	names, err := v.opts.parsePath(name, true)
	if err != nil {
		return pe(err)
	}
	if names == nil {
		return pe(fs.ErrExist)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if err := v.writable(); err != nil {
		return pe(err)
	}
	o := &op{kind: kindMkdir, name: names[len(names)-1], time: time.Now().UnixNano()}
	v.mu.RLock()
	pid, p, err := v.st.lookup(names[:len(names)-1])
	if err == nil && !p.dir {
		err = ErrNotDir
	}
	if _, taken := v.st.kids[pid][o.name]; err == nil && taken {
		err = fs.ErrExist
	}
	if err == nil {
		o.node, o.parent = v.newNodeID(), pid
	}
	v.mu.RUnlock()
	if err != nil {
		return pe(err)
	}
	if _, err := v.commit(o); err != nil {
		return pe(err)
	}
	return nil
}

// Move renames or moves from to the path to, whose parent must exist; a directory moves with everything below it.
// An existing to is fs.ErrExist; the root, or a move into from's own subtree, is fs.ErrInvalid.
func (v *Volume) Move(from, to string) error {
	pe := func(path string, err error) error { return &fs.PathError{Op: "kolofiles: move", Path: path, Err: err} }
	fn, err := v.opts.parsePath(from, false)
	if err != nil {
		return pe(from, err)
	}
	tn, err := v.opts.parsePath(to, true)
	if err != nil {
		return pe(to, err)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if err := v.writable(); err != nil {
		return pe(from, err)
	}
	v.mu.RLock()
	o, path, err := v.planMove(fn, tn)
	v.mu.RUnlock()
	if err != nil {
		p := to
		if path == 0 {
			p = from
		}
		return pe(p, err)
	}
	if o == nil {
		return nil
	}
	if _, err := v.commit(o); err != nil {
		return pe(to, err)
	}
	return nil
}

// planMove checks a move and returns its op, nil for a move to the same path; path tells which path an error is
// about, 0 for from. Call it holding mu.
func (v *Volume) planMove(fn, tn []string) (o *op, path int, err error) {
	id, n, err := v.st.lookup(fn)
	if err != nil {
		return nil, 0, err
	}
	if fn == nil {
		return nil, 0, fs.ErrInvalid
	}
	if tn == nil {
		return nil, 1, fs.ErrExist
	}
	pid, p, err := v.st.lookup(tn[:len(tn)-1])
	if err == nil && !p.dir {
		err = ErrNotDir
	}
	if err != nil {
		return nil, 1, err
	}
	name := tn[len(tn)-1]
	if pid == n.parent && name == n.name {
		return nil, 0, nil
	}
	if _, taken := v.st.kids[pid][name]; taken {
		return nil, 1, fs.ErrExist
	}
	d, _, met, err := v.st.depth(pid, id, v.opts.maxDepth)
	if met {
		return nil, 1, fs.ErrInvalid
	}
	if err != nil {
		return nil, 1, err
	}
	if d+1+v.st.height(id) > v.opts.maxDepth {
		return nil, 1, fmt.Errorf("%w: the moved tree would be deeper than %d", ErrLimit, v.opts.maxDepth)
	}
	return &op{kind: kindMove, node: id, parent: pid, name: name}, 0, nil
}
