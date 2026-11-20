package kolofiles

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
)

const (
	// DefaultMaxDepth is the default of WithMaxDepth: the deepest tree one Linux path argument can name.
	DefaultMaxDepth = 2048
	// DefaultMaxNameLength is the default of WithMaxNameLength, in bytes: 255 UTF-16 units of at most 3 bytes each.
	DefaultMaxNameLength = 765
)

var (
	// ErrNotDir means an element of a path that should be a directory is a file.
	ErrNotDir = errors.New("kolofiles: not a directory")
	// ErrIsDir means a file operation on a directory.
	ErrIsDir = errors.New("kolofiles: is a directory")
	// ErrLimit means a path or a log over MaxDepth or MaxNameLength, or an op to append over the largest entry kolo-sync carries.
	ErrLimit = errors.New("kolofiles: over a bound")
	// ErrReadOnly means a write on a reader.
	ErrReadOnly = errors.New("kolofiles: read-only volume")
	// ErrClosed means a call on a closed Volume, or on a Snapshot or file of one.
	ErrClosed = errors.New("kolofiles: volume closed")
	// ErrCorrupt means the log is not a kolo-files log, or an op or an object table breaks a rule of the format.
	ErrCorrupt = errors.New("kolofiles: corrupt volume")
	// ErrUnsafeName means LocalPath refused a name the host reserves for a device (some versions open the device
	// instead) or cannot create.
	ErrUnsafeName = errors.New("kolofiles: name not allowed on this system")
	// ErrInvalidArgument means a nil, out of range or mismatched argument, or a mark where marks do not apply.
	ErrInvalidArgument = errors.New("kolofiles: invalid argument")
)

var errEncoding = fmt.Errorf("%w: value encoding is not bytes", ErrInvalidArgument)

// NodeID identifies a file or directory for its whole life; moves and renames keep it. The root is the zero ID.
type NodeID [16]byte

// String returns the ID as lowercase hex.
func (id NodeID) String() string { return hex.EncodeToString(id[:]) }

var rootID NodeID

// NodeInfo is what fs.FileInfo.Sys returns for a file or directory of a Volume or Snapshot.
type NodeInfo struct {
	ID      NodeID
	Version uint64       // the version in which the node last changed: the length of the log after its last op
	Blob    koloblobs.ID // the blob of a file's content; zero for a directory
}

// Change is one node that differs between two versions, as Diff reports it.
type Change struct {
	Node     NodeID
	Old, New string // the path at the older and at the newer version; empty where the node is not in the tree
	Modified bool   // the file's content differs
	Deleted  bool   // the node was deleted between the versions; its paths are destroyed with it
}

// Mark says what a reader device keeps of a file; a directory's mark applies to everything below it.
type Mark uint8

const (
	// MarkInherit clears a mark: the node takes its nearest marked ancestor's mark.
	MarkInherit Mark = iota
	// MarkFetchOnDemand fetches only the objects a read needs, when it needs them, and keeps none. The root's default.
	MarkFetchOnDemand
	// MarkKeepLocal keeps every object of the file's current version on the device, so it reads offline.
	MarkKeepLocal
)

// String returns the mark's name.
func (m Mark) String() string {
	switch m {
	case MarkInherit:
		return "MarkInherit"
	case MarkFetchOnDemand:
		return "MarkFetchOnDemand"
	case MarkKeepLocal:
		return "MarkKeepLocal"
	}
	return fmt.Sprintf("Mark(%d)", uint8(m))
}

// Stats are a Volume's totals since it was opened.
type Stats struct {
	Ops            uint64 // ops appended to the metadata log
	Replayed       uint64 // entries replayed by opens, refreshes, snapshots and diffs
	BytesWritten   uint64 // file content read by WriteFile
	BytesRead      uint64 // file content returned by files
	ObjectsFetched uint64 // objects from the content backend
	ObjectsLocal   uint64 // objects served by the local backend of a reader
}

// Option configures Create, Open and the readers.
type Option func(*options)

type options struct {
	maxDepth int
	maxName  int
	err      error
}

// WithMaxDepth sets how many names below the root a path may have, at least 1; the default is DefaultMaxDepth.
func WithMaxDepth(n int) Option {
	return func(o *options) {
		if n < 1 {
			o.err = fmt.Errorf("%w: max depth %d", ErrInvalidArgument, n)
		}
		o.maxDepth = n
	}
}

// WithMaxNameLength sets the longest name in bytes after NFC, 1 to the largest op; the default is DefaultMaxNameLength.
func WithMaxNameLength(n int) Option {
	return func(o *options) {
		if n < 1 || n > maxEntry {
			o.err = fmt.Errorf("%w: max name length %d", ErrInvalidArgument, n)
		}
		o.maxName = n
	}
}

func newOptions(opts []Option) (options, error) {
	o := options{maxDepth: DefaultMaxDepth, maxName: DefaultMaxNameLength}
	for _, f := range opts {
		if f == nil {
			return o, fmt.Errorf("%w: nil option", ErrInvalidArgument)
		}
		f(&o)
	}
	return o, o.err
}

// Volume is a tree of files and folders kept in a metadata log and a content log. It is safe for concurrent use.
// A Volume from Create or Open writes; one from a reader constructor reads a replica.
type Volume struct {
	opts      options
	id        kololog.LogID
	contentID kololog.LogID

	meta    *kolostore.Log        // writer
	content *kolostore.Log        // writer
	lb      *koloblobs.LogBackend // writer: objects are written through it
	rep     *kolostore.Replica    // reader
	backend koloblobs.Backend     // reader: the content backend, nil with a fetch function
	fetch   koloblobs.FetchFunc   // reader: fetches content log entries, nil with a backend
	local   kolostore.Backend     // reader: marks and kept objects, may be nil

	writeMu   sync.Mutex   // serializes changes, from a blob write to its op's append
	destroyMu sync.RWMutex // held for writing while entries are destroyed, for reading while snapshots replay

	mu     sync.RWMutex
	st     *state
	n      uint64              // entries the state is built from
	skip   []uint64            // entries listed by complete PURGE sequences, ascending
	purged map[NodeID]struct{} // nodes with a complete PURGE sequence
	byNode map[NodeID][]uint64 // writer: the entries of the MKDIR, CREATE, WRITE and MOVE ops about each live node, ascending
	marks  map[NodeID]Mark     // reader with a local backend
	closed bool

	ops, replayed, bytesWritten, bytesRead, fetched, localHits atomic.Uint64
}

// Create starts a new volume in meta and content, two empty logs of one owner with the kololog.Bytes encoding, and
// takes them over: Close closes them. If Create fails, the logs stay open.
func Create(meta, content *kolostore.Log, opts ...Option) (*Volume, error) {
	v, err := newWriter(meta, content, opts)
	if err != nil {
		return nil, err
	}
	if meta.Len() != 0 || content.Len() != 0 {
		return nil, fmt.Errorf("%w: Create needs empty logs", ErrInvalidArgument)
	}
	o := &op{kind: kindVolume, volume: meta.ID(), contentLog: content.ID()}
	if _, err := v.meta.Append(encodeOp(o)); err != nil {
		return nil, fmt.Errorf("kolofiles: append: %w", err)
	}
	v.ops.Add(1)
	v.st, v.n = newState(), 1
	return v, nil
}

// Open rebuilds the volume in meta, your own metadata log, with content, the log its first entry names, and takes
// them over. It finishes a Delete that a crash interrupted. If Open fails, the logs stay open.
func Open(meta, content *kolostore.Log, opts ...Option) (*Volume, error) {
	v, err := newWriter(meta, content, opts)
	if err != nil {
		return nil, err
	}
	h, err := meta.Head()
	if err != nil {
		return nil, fmt.Errorf("kolofiles: %w", err)
	}
	hdr := meta.Header()
	vh, err := kololog.VerifyHead(hdr.LogKey, hdr.LogID, h)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if vh.Size() != meta.Len() {
		return nil, fmt.Errorf("%w: head does not cover the log", ErrCorrupt)
	}
	if meta.Len() == 0 {
		return nil, fmt.Errorf("%w: no volume entry", ErrCorrupt)
	}
	r, err := v.replay(meta.Len(), false)
	if err != nil {
		return nil, err
	}
	if r.contentLog != content.ID() {
		return nil, fmt.Errorf("%w: the volume names another content log", ErrInvalidArgument)
	}
	v.install(r)
	if err := v.resume(r); err != nil {
		return nil, err
	}
	return v, nil
}

func newWriter(meta, content *kolostore.Log, opts []Option) (*Volume, error) {
	o, err := newOptions(opts)
	if err != nil {
		return nil, err
	}
	if meta == nil || content == nil {
		return nil, fmt.Errorf("%w: nil log", ErrInvalidArgument)
	}
	mh, ch := meta.Header(), content.Header()
	if !bytes.Equal(mh.LogKey, ch.LogKey) || meta.ID() == content.ID() {
		return nil, fmt.Errorf("%w: the logs must be two logs of one owner", ErrInvalidArgument)
	}
	if mh.Encoding != kololog.Bytes || ch.Encoding != kololog.Bytes {
		return nil, errEncoding
	}
	return &Volume{
		opts: o, id: meta.ID(), contentID: content.ID(),
		meta: meta, content: content, lb: koloblobs.NewLogBackend(content),
		purged: map[NodeID]struct{}{}, byNode: map[NodeID][]uint64{},
	}, nil
}

// ContentLog returns the ID of the content log that the volume in meta names in its first entry.
func ContentLog(meta *kolostore.Replica) (kololog.LogID, error) {
	if meta == nil {
		return kololog.LogID{}, fmt.Errorf("%w: nil replica", ErrInvalidArgument)
	}
	h, err := meta.Head()
	if err != nil {
		return kololog.LogID{}, fmt.Errorf("kolofiles: %w", err)
	}
	if h.Size == 0 {
		return kololog.LogID{}, fmt.Errorf("kolofiles: empty replica: %w", kololog.ErrNoHead)
	}
	b, err := meta.Get(0)
	if err != nil {
		return kololog.LogID{}, readErr(err)
	}
	o, err := decodeOp(b, DefaultMaxNameLength, false)
	if err != nil {
		return kololog.LogID{}, err
	}
	if o.kind != kindVolume || o.volume != meta.ID() {
		return kololog.LogID{}, fmt.Errorf("%w: entry 0 is not this volume", ErrCorrupt)
	}
	return o.contentLog, nil
}

// OpenReader rebuilds a read-only volume from meta, a replica of the owner's metadata log, and takes it over.
// content holds the objects, local (may be nil) the marks and kept objects. If it fails, the replica stays open.
func OpenReader(meta *kolostore.Replica, content koloblobs.Backend, local kolostore.Backend, opts ...Option) (*Volume, error) {
	if content == nil {
		return nil, fmt.Errorf("%w: nil content backend", ErrInvalidArgument)
	}
	return openReader(meta, content, nil, local, opts)
}

// OpenFetchReader is OpenReader with content read from another peer, one content log entry per fetch call.
func OpenFetchReader(meta *kolostore.Replica, fetch koloblobs.FetchFunc, local kolostore.Backend, opts ...Option) (*Volume, error) {
	if fetch == nil {
		return nil, fmt.Errorf("%w: nil fetch", ErrInvalidArgument)
	}
	return openReader(meta, nil, fetch, local, opts)
}

func openReader(meta *kolostore.Replica, content koloblobs.Backend, fetch koloblobs.FetchFunc, local kolostore.Backend, opts []Option) (*Volume, error) {
	o, err := newOptions(opts)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: nil replica", ErrInvalidArgument)
	}
	if meta.Header().Encoding != kololog.Bytes {
		return nil, errEncoding
	}
	v := &Volume{opts: o, id: meta.ID(), rep: meta, backend: content, fetch: fetch, local: local, purged: map[NodeID]struct{}{}}
	if err := v.loadMarks(); err != nil {
		return nil, err
	}
	if err := v.rebuild(); err != nil {
		return nil, err
	}
	return v, nil
}

// ID returns the volume's ID, the ID of its metadata log.
func (v *Volume) ID() kololog.LogID { return v.id }

// Len returns the number of metadata log entries the tree is built from, which is the current version. On a reader
// it is at most the size of the replica's last verified head, and less while an entry key or a purge op is missing.
func (v *Volume) Len() uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.n
}

// Stats returns the volume's totals; it is safe to call at any time.
func (v *Volume) Stats() Stats {
	return Stats{
		Ops:            v.ops.Load(),
		Replayed:       v.replayed.Load(),
		BytesWritten:   v.bytesWritten.Load(),
		BytesRead:      v.bytesRead.Load(),
		ObjectsFetched: v.fetched.Load(),
		ObjectsLocal:   v.localHits.Load(),
	}
}

// Close closes the volume and the logs or the replica it took over, not the backends. It waits for a running
// WriteFile, Prefetch or Refresh, for example on a stalled fetch, until that call's context ends. A second Close
// returns nil.
func (v *Volume) Close() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.destroyMu.Lock()
	defer v.destroyMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	v.st, v.skip, v.purged, v.marks = nil, nil, nil, nil
	if v.rep != nil {
		return v.rep.Close()
	}
	return errors.Join(v.meta.Close(), v.content.Close())
}

// entryErr adds the entry index to err, which already starts with "kolofiles:".
func entryErr(i uint64, err error) error { return fmt.Errorf("%w: entry %d", err, i) }

func (v *Volume) writer() bool { return v.meta != nil }

// get reads entry i of the metadata log.
func (v *Volume) get(i uint64) ([]byte, error) {
	if v.meta != nil {
		return v.meta.Get(i)
	}
	return v.rep.Get(i)
}

// readErr keeps storage errors as they are and reports an entry that does not open as ErrCorrupt.
func readErr(err error) error {
	for _, bad := range []error{kololog.ErrMalformedEntry, kololog.ErrMalformed, kololog.ErrBadEntry, kololog.ErrStorageMismatch, kolostore.ErrCorrupt} {
		if errors.Is(err, bad) {
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
	}
	return fmt.Errorf("kolofiles: %w", err)
}
