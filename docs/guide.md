# Guide

Walkthrough of the API, path rules, reading from a pinning peer, errors, the design and what a storage holder can see. Numbers and bounds are in [limits.md](limits.md), the formats in [formats.md](formats.md). A complete program is in the README.

## API

- `Create(meta, content, opts...) (*Volume, error)` starts a volume in two
  empty kolo-store logs of one owner (value encoding `kololog.Bytes`) and
  takes them over. `Open(meta, content, opts...)` rebuilds it from the logs
  and finishes a `Delete` that a crash interrupted. `Close` closes the logs.
  The volume's ID, `ID()`, is the metadata log's ID.
- `OpenReader(meta, content, local, opts...) (*Volume, error)` builds a
  read-only volume from a replica of the metadata log. `content` is a
  kolo-blobs `Backend`, such as `koloblobs.NewLogBackend` on a replica of
  the content log. `OpenFetchReader(meta, fetch, local, opts...)` reads the
  content from another peer instead, where `fetch(ctx, index)` gets one
  content log entry. `local` keeps marks and kept objects, and may be nil;
  `Close` leaves it and `content` open, and a constructor that fails leaves
  the replica open. A replica whose value encoding is not `kololog.Bytes` is
  `ErrInvalidArgument`, as for `Create`. The reader replays the longest prefix it can
  replay exactly: it stops at an entry held without its key and before a
  destroyed entry that no complete PURGE sequence lists, and `Len()` says
  which version that is. `Refresh()` brings it up to the replica's latest
  verified head, incrementally when it can, and `Len()` may go down. An
  incremental refresh checks every entry below `Len()` for a destroy marker,
  so it costs time in `Len()`. `ContentLog(meta)` returns the content log's ID from the first entry.
- `WriteFile(ctx, name, src)` creates a file (its parent must exist) or
  stores new content for an existing one, reading `src` to its end. Only the
  chunks that changed since earlier versions are stored (kolo-blobs
  deduplicates within one key), and a killed write is resumed by calling it
  again with the same content. `ctx` is checked between chunks. `Mkdir(name)` needs its parent and fails with
  `fs.ErrExist` on an existing name. `Move(from, to)` renames or moves a file
  or a whole directory; the destination's parent must exist and the
  destination must not (`fs.ErrExist`); a move into the node's own subtree or
  of the root is `fs.ErrInvalid`; a move to the same path does nothing.
  `Delete(name)` removes a file or a directory with everything below it, and
  destroys what it held (see [Design](#design)). The root cannot be deleted.
- `Open`, `OpenContext(ctx, name)`, `ReadDir` and `Stat`, as `fs.FS`,
  `fs.ReadDirFS` and `fs.StatFS`. A file is also an `io.ReaderAt` and
  `io.Seeker`: a range read fetches one node per tree level and the chunks
  that cover it. `OpenContext` gives the file a context that ends its reads.
  Listings are sorted by name in byte order. `Stat` reports files as 0444 and
  directories as `fs.ModeDir | 0555`; the modification time is the writer's
  clock at the op, for display only. `FileInfo.Sys()` is a `NodeInfo`: the
  node's ID, the version in which it last changed and the blob ID.
- `Len()` is the current version: the number of metadata entries the tree is
  built from. `Snapshot(version)`, 1 to `Len()`, returns the tree after the
  first `version` entries as an immutable `Snapshot` with the same `Open`,
  `OpenContext`, `ReadDir` and `Stat`, and `Version()`. Version 1 is the
  empty root. `Diff(a, b)`, `1 <= a <= b <= Len()`, returns one `Change` per
  node added, moved, modified or deleted in between, sorted by path, then node
  ID; a moved directory is one change. Deletion wins over history: a deleted
  node is in no snapshot, as if it had never been there, and appears in a
  diff only as a `Change` with `Deleted` set and no paths.
- `SetMark(name, mark)`, `Mark(name)` and `Prefetch(ctx)`: selective sync on a
  reader opened with a local backend. `MarkKeepLocal` keeps every object of the
  file's current version on the device, so it reads offline; `MarkFetchOnDemand`
  fetches only the objects a read needs and keeps nothing; `MarkInherit` clears a
  mark. A node without a mark takes its nearest marked ancestor's, and the
  root's default is `MarkFetchOnDemand`, so disk use never grows on its own.
  Marks are stored by node ID, so they follow moves, and they are dropped
  with the node. `Prefetch` reads every `MarkKeepLocal` file's current version
  through the local backend, removes its kept objects of older versions, goes
  on after a file fails and returns every error. It holds the volume's change
  lock, so `Refresh`, `SetMark` and `Close` wait until it ends: stop a long one
  with its context. A kept object is checked
  against its hash before it is kept and before it is served; one that fails
  is dropped. A move that puts a file under a node with another mark, seen by
  `Refresh`, removes the objects kept for it. Marks on a writer, or on a reader without a local backend, are
  `ErrInvalidArgument`.
- `Stats()` returns totals since open, read with atomics: `Ops`, `Replayed`,
  `BytesWritten`, `BytesRead`, `ObjectsFetched` and `ObjectsLocal`.
- Options: `WithMaxDepth(n)`, at least 1, and `WithMaxNameLength(n)`, 1 to
  the largest op (kolo-blobs `MaxObjectSize`, 4,128,764 bytes), since a name
  must fit in one op.
- A `Volume` and a `Snapshot` are safe for concurrent use. Writes are
  serialized from the start of a blob write to the op's append; reads never
  wait for a blob write. A failed append leaves the tree as it was.

## Paths

Paths follow `io/fs.ValidPath`: slash-separated, relative to the volume root,
`.` is the root. A path that breaks a rule is `fs.ErrInvalid` inside an
`*fs.PathError`:

| Rule | Decision |
|---|---|
| Encoding | valid UTF-8 |
| Normalization | names are stored in NFC, and every path given to the API is normalized first, so `e` plus a combining acute accent and the precomposed `é` are one name; the original bytes of a non-NFC name are not kept. Two names that differ only in normalization (possible on a Linux disk) are one name in a volume: `Mkdir` and `Move` report `fs.ErrExist` and `WriteFile` replaces the file, so code that copies a disk folder into a volume should `Stat` each normalized path first to detect such a pair |
| Unassigned code points | refused when a name is created (in the Unicode version of the Go release); a reader accepts them, since a newer writer's tables may know more |
| Control characters | refused (general category Cc) |
| `.`, `..`, empty, `/` in a name | refused |
| Case | names compare byte for byte: `README` and `readme` may both exist, and a case-only rename is an ordinary move; names that differ only in case collide on a case-insensitive disk (the macOS and Windows defaults); the [recipe](#writing-a-volume-to-disk) below reports that as `fs.ErrExist` |
| Windows-only rules (`<>:"\|?*`, `CON` and the other device names with or without an extension, a trailing dot or space, over 255 UTF-16 units) | allowed in a volume, since other systems create such names; code that writes a volume to disk converts each path with `LocalPath`, which on Windows refuses them with `ErrUnsafeName` (`filepath.Localize` alone accepts `NUL.txt` and `<>\|?*"`; `os.CopyFS` does not use these rules: it renames a trailing dot or space and writes device names with an extension) |
| Name length | at most `MaxNameLength` bytes after NFC, default 765 (`DefaultMaxNameLength`) |
| Depth | at most `MaxDepth` names below the root, default 2048 (`DefaultMaxDepth`) |

## Writing a volume to disk

`LocalPath` checks names only and never looks at the disk. A disk can give two
volume names one entry, for example names that differ only in case on a
case-insensitive disk, or a Windows 8.3 short name such as `LONGFI~1.TXT` for
`longfilename.txt` or `PROGRA~1` for a folder. A link already in the
destination can also lead outside it. So create each folder and file under an
`os.Root`, with `Mkdir` and `O_EXCL`: an alias then fails with `fs.ErrExist` instead of
overwriting another entry, and `os.Root` refuses a path that leaves the
directory. This is the function of the `LocalPath` example, compiled and run by
`go test`:

```go
// checkout writes the tree of v into dir, which should be empty. Each folder and file is created under an
// os.Root, with Mkdir and O_EXCL, so a name that aliases one already written (a case-only pair on a
// case-insensitive disk, a Windows 8.3 short name) fails with fs.ErrExist instead of overwriting it, and no link
// leads outside dir.
func checkout(v fs.FS, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(v, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		local, err := kolofiles.LocalPath(name)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return root.Mkdir(local, 0o777)
		}
		src, err := v.Open(name)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			dst.Close()
			return err
		}
		return dst.Close()
	})
}
```

`os.CopyFS` is not a substitute: it follows links already in the destination,
merges folders whose names differ only in case, and on Windows it renames a
trailing dot or space and writes device names with an extension such as
`NUL.txt`, all without an error.

## Reading from a pinning peer

The owner's two logs are all a reader needs besides the owner's public key
and the keys. A reader device keeps a replica of the metadata log (with its
entry keys, from the owner or any peer with kolo-sync `Keys` access) and reads
the content from a peer that holds only ciphertext, using entry keys it holds
itself, so the pinning peer never receives a key (see the [kolo-blobs guide](https://github.com/ciranda-dev/kolo-blobs/blob/main/docs/guide.md#reading-from-a-pinning-peer)
for the keys and the head):

```go
// Once, owner online: the content log's head and every entry key.
head, err := kolosync.FetchHead(ctx, ownerConn, owner, contentLogID)
if err != nil {
	return err
}
var keys []kololog.EntryKey
if head.Size() > 0 { // an empty content log has no entries to key
	if keys, err = kolosync.FetchKeys(ctx, ownerConn, head, 0, head.Size()-1); err != nil {
		return err
	}
}

// Later, owner offline: one entry per call from the pinning peer, opened with the key held here.
fetch := func(ctx context.Context, i uint64) ([]byte, error) {
	f, err := kolosync.FetchWithKeys(ctx, pinConn, head, i, i, func(j uint64) (kololog.EntryKey, error) {
		if j >= uint64(len(keys)) || keys[j] == nil {
			return nil, nil // none held
		}
		return bytes.Clone(keys[j]), nil // kolo-sync clears its copy after use
	})
	if err != nil {
		return nil, err
	}
	return f[0].Data, f[0].Err
}
v, err := kolofiles.OpenFetchReader(meta, fetch, local)
```

`meta` is a kolo-store replica of the owner's metadata log (`kolosync.Pull`,
from the pinning peer for the ciphertext and from a peer with `Keys` access
for the keys). `local` is a kolo-store `Backend` for the device's marks and
kept objects; it may be nil. `kolofiles.ContentLog(meta)` gives the content
log's ID. When the owner changes the volume, pull the metadata log again and
call `Refresh`; a version written after the saved head needs a new head and
its keys. `TestPinnedVolumeReadWhileOwnerOffline` runs this flow on 127.0.0.1
with a real kolo-pin peer, then dumps the peer's store and finds no key, name
or plaintext; `TestPinDoubleNeverSeesAKey` and `TestHostilePin` use a pin
double that fails on any key byte and on any request off the kolo-sync list.

## Errors

For `errors.Is`; none carries a key, file content or entry bytes.

- Path errors are `*fs.PathError` with `Op` starting `kolofiles:`, so
  `fs.ErrNotExist`, `fs.ErrExist` and `fs.ErrInvalid` match. Names appear only
  in `PathError.Path`, as the caller gave them.
- `ErrNotDir`: a path element is a file. `ErrIsDir`: a file operation on a
  directory.
- `ErrLimit`: a path or a log over `MaxDepth` or `MaxNameLength`, or an op
  to append over the largest entry kolo-sync carries.
- `ErrReadOnly`: a write on a reader. `ErrClosed`: a call on a closed volume,
  or a read of a `Snapshot` or file of one.
- `ErrCorrupt`: the log is not a kolo-files log, or an op or an object table
  breaks a rule of the format (see [formats](formats.md)).
- `ErrInvalidArgument`: a nil argument, an option out of range, a version out
  of range, a mark where marks do not apply, logs of different owners or with
  a value encoding other than `kololog.Bytes`, a content log other than the
  one the volume names, or a non-empty log given to `Create`.
- Errors from kolo-blobs, kolo-store and kolo-log pass through wrapped. A file
  whose entries were destroyed reads as `kololog.ErrDeleted` (and
  `fs.ErrNotExist`).

## Design

- Two logs. A kolo-log replica grows only as a verified prefix, and kolo-sync
  `Pull` copies a whole log. With one log a reader could not hold every name
  without every byte of every file. The metadata log is small and always fully
  replicated; the content log is replicated whole, fetched in ranges, or both.
- Nodes. Every file and folder has a random 16-byte `NodeID`, chosen when it is
  created and never changed by a move or rename. Ops refer to nodes, never to
  paths, so a rename is one op, a moved folder moves its subtree, and history
  follows a file across renames.
- Versions. A version is a metadata log length. `Snapshot(v)` replays the
  first `v` entries; `Snapshot(Len())` copies the current tree. `Diff(a, b)`
  reads the entries between `a` and `b`, then replays the first `b`.
- Keys. Each file gets a kolo-blobs key when it is created. It is in the
  create op, which kolo-log encrypts under its entry key, so every version of
  the file shares its unchanged chunks and one key opens them all. Keys are
  not kept in memory and are never returned, printed or logged; a copy read
  from the log is cleared after use. A key is for one file and its versions.
- Object tables. Each version also writes a table, a second blob under the
  file key, listing every object of the content blob and the content log entry
  that holds it. The op carries the table blob's own locations, so a reader
  without the content replica finds each entry with no scan of the log, and
  `Delete` can list the entries of every version.
- Delete. `Delete(name)` collects the subtree's nodes, every metadata entry
  about them (found in an in-memory index the writer keeps, so a delete does not
  read the rest of the log) and every content entry of their versions and tables; appends a
  REMOVE op (the node ID only) and PURGE ops listing the entries, each under
  the largest entry kolo-sync carries; then destroys the content entries and
  the metadata entries with kolo-log `Delete`. What is destroyed: every name the
  node ever had, its file key (so every version becomes unreadable at once on
  conforming peers) and every object. What stays: the REMOVE and PURGE ops
  (a node ID and entry indexes), kolo-log's signed records and markers, and the
  fact that a delete happened. It is a best-effort tombstone: it holds on every
  peer that follows the protocol and applies the delete markers, and a peer
  that already received a key can keep the data. A crash at any step is
  finished by the next `Open`, which first checks that the entries an
  unfinished sequence lists belong to the removed nodes, and that the content
  entries a finished one still lists are objects of them (`ErrCorrupt`
  otherwise). Overwritten content is kept (every earlier
  version of a live file stays readable); only `Delete` removes history. A
  reader that sees destroyed entries before the PURGE op that lists them steps
  back to just before them until the op arrives, and drops the marks and kept
  objects of removed nodes on its next open or refresh.
- A write killed before its op was appended leaves unreferenced objects. For a
  new file nobody can read them, since its key was never stored; for an
  existing file they are under that file's key and are destroyed when the file
  is deleted.
- One writer. A volume has one writer, its owner; a log with dependencies on
  another writer is `ErrCorrupt`.

## What is visible to storage

- A pinning peer that holds both logs sees entry counts and padded sizes, when
  entries are added and which entries a delete destroyed. It sees no name, no
  file key, no entry key, no blob ID and no plaintext: names, keys and sizes
  are inside kolo-log ciphertext. `TestPinnedVolumeReadWhileOwnerOffline`
  dumps its store.
- A peer that serves entries to a reader with `Fetch` or `FetchWithKeys` sees
  which content entries are fetched and when, so from the order (a table, the
  root, one node per level, then chunks) it learns which file is opened and
  which ranges are read, and the size of the head the reader checks against.
- Under one file key, equal chunks are equal ciphertext, so two versions of a
  file show which chunks they share. Different files never share an object.
- A peer with `Keys` access to the metadata log reads every name and file key
  of the volume; give `Keys` access only to readers who may read all of it.
