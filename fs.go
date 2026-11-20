package kolofiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
)

var (
	_ fs.FS        = (*Volume)(nil)
	_ fs.ReadDirFS = (*Volume)(nil)
	_ fs.StatFS    = (*Volume)(nil)
	_ fs.FS        = (*Snapshot)(nil)
	_ fs.ReadDirFS = (*Snapshot)(nil)
	_ fs.StatFS    = (*Snapshot)(nil)
	_ io.ReaderAt  = (*file)(nil)
	_ io.Seeker    = (*file)(nil)
)

// view is a tree to read: the volume's current one when st is nil, or a snapshot's.
type view struct {
	v  *Volume
	st *state
}

// found is a node copied out of a tree, with a directory's entries and whether this reader keeps a file local.
type found struct {
	id   NodeID
	n    node
	kids []fs.DirEntry // a directory's entries, when asked for
	keep bool          // a file this reader keeps local
}

// find looks up name, and lists it if it is a directory and list is set.
func (w view) find(name string, list bool) (found, error) {
	names, err := w.v.opts.parsePath(name, false)
	if err != nil {
		return found{}, err
	}
	w.v.mu.RLock()
	defer w.v.mu.RUnlock()
	if w.v.closed {
		return found{}, ErrClosed
	}
	st := w.st
	if st == nil {
		st = w.v.st
	}
	id, n, err := st.lookup(names)
	if err != nil {
		return found{}, err
	}
	f := found{id: id, n: *n}
	if n.dir && list {
		for _, c := range st.children(id) {
			cid := st.kids[id][c]
			f.kids = append(f.kids, fs.FileInfoToDirEntry(fileInfo{id: cid, n: *st.nodes[cid]}))
		}
	}
	if !n.dir && w.st == nil && w.v.marks != nil {
		f.keep = w.v.effectiveMark(st, id) == MarkKeepLocal
	}
	return f, nil
}

func (w view) open(ctx context.Context, name string) (fs.File, error) {
	f, err := w.find(name, true)
	if err != nil {
		return nil, &fs.PathError{Op: "kolofiles: open", Path: name, Err: err}
	}
	info := fileInfo{id: f.id, n: f.n}
	if f.n.dir {
		return &dirFile{info: info, name: name, entries: f.kids}, nil
	}
	r, _, err := w.v.openContent(ctx, f.id, &f.n, f.keep)
	if err != nil {
		return nil, &fs.PathError{Op: "kolofiles: open", Path: name, Err: notExist(err)}
	}
	return &file{info: info, r: r, v: w.v, name: name}, nil
}

func (w view) readDir(name string) ([]fs.DirEntry, error) {
	f, err := w.find(name, true)
	if err == nil && !f.n.dir {
		err = ErrNotDir
	}
	if err != nil {
		return nil, &fs.PathError{Op: "kolofiles: readdir", Path: name, Err: err}
	}
	return f.kids, nil
}

func (w view) stat(name string) (fs.FileInfo, error) {
	f, err := w.find(name, false)
	if err != nil {
		return nil, &fs.PathError{Op: "kolofiles: stat", Path: name, Err: err}
	}
	return fileInfo{id: f.id, n: f.n}, nil
}

// notExist adds fs.ErrNotExist to the error of a file whose entries were destroyed.
func notExist(err error) error {
	if errors.Is(err, kololog.ErrDeleted) && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return err
}

// Open opens a file or directory for reading, as fs.FS: a file is also an io.ReaderAt and io.Seeker, a directory
// an fs.ReadDirFile. Reads of remote content cannot be cancelled; use OpenContext for that.
func (v *Volume) Open(name string) (fs.File, error) {
	return view{v: v}.open(context.Background(), name)
}

// OpenContext is Open with a context that bounds every content read of the file it returns.
func (v *Volume) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return view{v: v}.open(ctx, name)
}

// ReadDir lists a directory sorted by name in byte order, as fs.ReadDirFS.
func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) { return view{v: v}.readDir(name) }

// Stat describes a file or directory, as fs.StatFS; its Sys is a NodeInfo.
func (v *Volume) Stat(name string) (fs.FileInfo, error) { return view{v: v}.stat(name) }

// Snapshot is the tree at one version of a volume; it is immutable, safe for concurrent use and needs no closing.
// Its reads go through the volume, so they fail with ErrClosed once the volume is closed.
type Snapshot struct {
	w   view
	ver uint64
}

// Open opens a file or directory of the snapshot for reading, as Volume.Open.
func (s *Snapshot) Open(name string) (fs.File, error) { return s.w.open(context.Background(), name) }

// OpenContext is Open with a context that bounds every content read of the file it returns.
func (s *Snapshot) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return s.w.open(ctx, name)
}

// ReadDir lists a directory of the snapshot, as Volume.ReadDir.
func (s *Snapshot) ReadDir(name string) ([]fs.DirEntry, error) { return s.w.readDir(name) }

// Stat describes a file or directory of the snapshot, as Volume.Stat.
func (s *Snapshot) Stat(name string) (fs.FileInfo, error) { return s.w.stat(name) }

// Version returns the snapshot's version: the length of the metadata log it was replayed from.
func (s *Snapshot) Version() uint64 { return s.ver }

// fileInfo describes a node; it is an fs.FileInfo.
type fileInfo struct {
	id NodeID
	n  node
}

func (f fileInfo) Name() string {
	if f.id == rootID {
		return "."
	}
	return f.n.name
}

func (f fileInfo) Size() int64 {
	if f.n.dir {
		return 0
	}
	return int64(f.n.size)
}

func (f fileInfo) Mode() fs.FileMode {
	if f.n.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (f fileInfo) ModTime() time.Time {
	if f.id == rootID {
		return time.Time{}
	}
	return time.Unix(0, f.n.mtime)
}

func (f fileInfo) IsDir() bool { return f.n.dir }

func (f fileInfo) Sys() any { return NodeInfo{ID: f.id, Version: f.n.changed + 1, Blob: f.n.blob} }

// file is an open file: the kolo-blobs Reader of its content.
type file struct {
	info fileInfo
	r    *koloblobs.Reader
	v    *Volume
	name string // as the caller gave it
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }

// check returns a PathError wrapping ErrClosed once the volume is closed.
func (f *file) check(op string) error {
	f.v.mu.RLock()
	defer f.v.mu.RUnlock()
	if f.v.closed {
		return &fs.PathError{Op: "kolofiles: " + op, Path: f.name, Err: ErrClosed}
	}
	return nil
}

func (f *file) Read(p []byte) (int, error) {
	if err := f.check("read"); err != nil {
		return 0, err
	}
	n, err := f.r.Read(p)
	f.v.bytesRead.Add(uint64(n))
	return n, err
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if err := f.check("readat"); err != nil {
		return 0, err
	}
	n, err := f.r.ReadAt(p, off)
	f.v.bytesRead.Add(uint64(n))
	return n, err
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if err := f.check("seek"); err != nil {
		return 0, err
	}
	return f.r.Seek(offset, whence)
}

func (f *file) Close() error { return f.r.Close() }

// dirFile is an open directory, listing the entries it had when it was opened.
type dirFile struct {
	info    fileInfo
	name    string // as the caller gave it
	entries []fs.DirEntry
	closed  bool
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "kolofiles: read", Path: d.name, Err: ErrIsDir}
}

func (d *dirFile) Close() error {
	d.closed = true
	return nil
}

// ReadDir returns the next n entries, or all that are left when n <= 0, as fs.ReadDirFile.
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, &fs.PathError{Op: "kolofiles: readdir", Path: d.name, Err: fs.ErrClosed}
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n:n]
	d.entries = d.entries[n:]
	return out, nil
}
