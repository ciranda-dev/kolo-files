# kolo-files

Folders, names and versions for Kolo: an encrypted tree of files with its whole history.

[![status: experimental](https://img.shields.io/badge/status-experimental-orange)](#status)
[![license: MIT OR Apache-2.0](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue)](#license)
[![go: 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8)](go.mod)

## What it is

kolo-files keeps an encrypted tree of folders, names and versions on top of [kolo-blobs](https://github.com/ciranda-dev/kolo-blobs), with names in Unicode Normalization Form C ([UAX #15](https://www.unicode.org/reports/tr15/tr15-58.html)). A `Volume` is a tree of files and folders kept in two [kolo-store](https://github.com/ciranda-dev/kolo-store) logs of one owner: a metadata log with one op per change (create a folder, write a file, move, delete) and a content log of kolo-blobs objects. The tree is rebuilt by replaying the metadata log, and every op is a version: `Snapshot` checks out any earlier tree and `Diff` compares two.

Each file has its own key, kept inside its create op, so a [kolo-pin](https://github.com/ciranda-dev/kolo-pin) pinning peer that holds both logs sees neither names, keys nor content. `Volume` and `Snapshot` implement `io/fs`, so `fs.WalkDir`, `fs.ReadFile` and `fs.Glob` work on them; to write one to disk, follow the [guide](docs/guide.md#writing-a-volume-to-disk), not `os.CopyFS`. A reader keeps a replica of the metadata log and reads content from a content replica or, entry by entry, from another peer; marks choose which files a device keeps.

It is pure Go: no cgo and nothing to install.

## Install

```sh
go get github.com/ciranda-dev/kolo-files
```

## Quick start

This program keeps a small tree in a volume on disk, in the directory given as argument, such as `go run . "$(mktemp -d)/store"`. It needs a module that requires kolo-files, kolo-id and kolo-store.

```go
// Command basic keeps a small tree in a volume on disk, in the directory given as argument, then lists it, checks
// out an older version, compares two versions and deletes a file.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	kolofiles "github.com/ciranda-dev/kolo-files"
	koloid "github.com/ciranda-dev/kolo-id"
	kolostore "github.com/ciranda-dev/kolo-store"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: basic DIR")
	}
	ctx := context.Background()
	db, err := kolostore.OpenPebble(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	st := kolostore.New(db)
	defer st.Close()
	me, err := koloid.Generate()
	if err != nil {
		log.Fatal(err)
	}
	meta, err := st.Create(me)
	if err != nil {
		log.Fatal(err)
	}
	content, err := st.Create(me)
	if err != nil {
		log.Fatal(err)
	}
	v, err := kolofiles.Create(meta, content)
	if err != nil {
		log.Fatal(err)
	}
	defer v.Close()

	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	must(v.Mkdir("notes"))
	must(v.WriteFile(ctx, "notes/todo.txt", strings.NewReader("buy bread")))
	first := v.Len()
	must(v.WriteFile(ctx, "notes/todo.txt", strings.NewReader("buy bread and milk")))
	must(v.Move("notes/todo.txt", "notes/shopping.txt"))
	must(v.WriteFile(ctx, "draft.txt", strings.NewReader("not for keeping")))

	list := func(name string, fsys fs.FS) {
		fmt.Println(name)
		must(fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(fsys, p)
			fmt.Printf("  %s: %s\n", p, b)
			return err
		}))
	}
	list("now:", v)
	old, err := v.Snapshot(first)
	must(err)
	list(fmt.Sprintf("at version %d:", first), old)

	changes, err := v.Diff(first, v.Len())
	must(err)
	for _, c := range changes {
		fmt.Printf("changed: %q -> %q, content modified: %v\n", c.Old, c.New, c.Modified)
	}

	// Delete destroys the name, the key and every object of the file; no version shows it again.
	must(v.Delete("draft.txt"))
	_, err = v.Stat("draft.txt")
	fmt.Println("draft.txt after delete:", err)
}
```

It prints the tree now, the tree at version 3, the two changes between version 3 and now (a file added, and a file renamed and edited) and that `draft.txt` is gone after `Delete`.

A reader device needs the owner's two logs, the owner's public key and the keys. It keeps a replica of the metadata log and reads the content from a peer that holds only ciphertext, with entry keys it holds itself, so the pinning peer never receives a key. The code is in the [guide](docs/guide.md#reading-from-a-pinning-peer).

## API

| Symbol | What it does |
|---|---|
| `Create(meta, content)` | Start a volume in two empty kolo-store logs of one owner |
| `Open(meta, content)` | Rebuild a volume from its logs and finish a `Delete` a crash interrupted |
| `OpenReader`, `OpenFetchReader` | A read-only volume from a replica of the metadata log, with content from a replica or fetched from a peer |
| `WriteFile`, `Mkdir`, `Move`, `Delete` | Change the tree; a write stores only the chunks that changed, and `Delete` destroys what the node held |
| `Open`, `ReadDir`, `Stat` | `io/fs` access to the current tree; files are also `io.ReaderAt` and `io.Seeker` |
| `LocalPath` | Convert a volume path into a host path; on Windows it refuses names Windows reserves for a device or cannot create. It checks names only: the [guide](docs/guide.md#writing-a-volume-to-disk) shows how to write a volume to disk |
| `Len`, `Snapshot`, `Diff` | The current version, the tree at an earlier version, and the changes between two |
| `SetMark`, `Mark`, `Prefetch` | Choose which files a reader keeps on the device or fetches on demand |
| `Refresh` | Bring a reader up to the replica's latest verified head |
| `Stats` | Totals of ops, replays, bytes and objects |

Paths follow `io/fs.ValidPath`. Names are stored in NFC, compare byte for byte and have a default maximum of 765 bytes and 2048 levels, both settable with `WithMaxNameLength` and `WithMaxDepth`. A `Volume` and a `Snapshot` are safe for concurrent use. The full rules are in the [guide](docs/guide.md#paths).

Errors are sentinels that work with `errors.Is`: `ErrNotDir`, `ErrIsDir`, `ErrLimit`, `ErrReadOnly`, `ErrClosed`, `ErrCorrupt`, `ErrUnsafeName` and `ErrInvalidArgument`. Path errors are `*fs.PathError`, so `fs.ErrNotExist` and `fs.ErrExist` match.

## How it works

- Two logs: a small metadata log that a reader always replicates whole, and a content log that is replicated whole, fetched in ranges, or both.
- Every file and folder has a random node ID that a move never changes, so a rename is one op and history follows a file across renames.
- `Delete` destroys every name the node ever had, its file key and every object, so no version shows it again. It is a best-effort tombstone: a peer that already received a key can keep the data.
- A pinning peer sees entry counts, padded sizes and timing, never a name, key or content. A peer with `Keys` access to the metadata log reads every name and file key. What each party can see is in the [guide](docs/guide.md#what-is-visible-to-storage).

## More

[Guide](docs/guide.md), [formats](docs/formats.md), [limits](docs/limits.md), [security policy](SECURITY.md).

## License

Licensed under either of the [Apache License, Version 2.0](LICENSE) or the [MIT license](LICENSE-MIT), at your option.

## Status

Experimental. The API may change before v1.0.0.
