# Limits

This page is reference material: the README does not repeat it.

## Name length and depth

The name length and depth rules are in the path table of the [guide](guide.md#paths).

The two bounds are safety settings, not product limits. 765 bytes holds
every name that ext4 (255 bytes) and Windows allow: Windows names are at most
255 UTF-16 units, and a UTF-16 unit takes at most 3 bytes of UTF-8 (a code
point outside the BMP is 2 units and 4 bytes). 2048 is the deepest tree one
Linux path argument can name (`PATH_MAX` 4096 with the NUL, one-byte names). A
path over a bound is `ErrLimit`. A writer enforces the bounds on every create
and move, so a reader with the same settings opens everything the writer
wrote. A reader checks a replayed `MKDIR`, `CREATE` or `MOVE` only for the
depth of the node it creates or moves, not for the nodes below a moved
directory, so a hostile log can put nodes deeper than `MaxDepth`; such a node
has no path (a lookup is `ErrLimit`, `Diff` reports it with empty paths).
Walks up the tree (moves, paths, marks) take at most `MaxDepth` steps, except
the reachability check of `Diff`, which stops at the number of nodes and walks
each chain once; walks down it (a delete, a subtree move check) take at most the number of nodes.
An object table is read as a stream: its count is checked against the
objects a blob of that size can hold and against the table's size, and memory
grows with the bytes the table blob really holds, not with the size a writer
claims. Everything else is bounded by what kolo-store has accepted: there is
no limit on files, folders, versions or file size.

## Memory

Memory: the current tree is kept in memory, about 211 bytes per live node
with a 10-byte name (`BenchmarkStateNode`, 64-bit, Apple M5, Go 1.27.3), the
ID of every node a delete removed, 8 bytes per destroyed entry, a writer's
index of the entries about each live node (about 71 bytes per node with one
op and 8 bytes per further op, `BenchmarkIndexNode`), and kolo-store's own
per-entry cost. A replay of a whole log holds every decoded
op until it has applied them, about 344 bytes per entry with a 10-byte name
(`BenchmarkReplayEntry`, same machine) besides the tree, so open a very large
volume with `GOMEMLIMIT` set. Snapshots and diffs hold their own tree while
they run and nothing after. Every walk is iterative.

## Time

Costs that grow with the size of a volume, measured on a loaded 64-bit machine
with an in-memory store:

- `Create` and `Open` replay every metadata entry once, and `Open` also reads
  both logs back through kolo-log, which verifies every record and ciphertext.
  Reopening a volume of 9,000 small files (9,000 metadata and 36,000 content
  entries) took 1.7 s.
- `Delete` reads only the entries about the removed nodes, found in the
  writer's index. Deleting one file from a volume of 9,000 took 27 ms when it
  still scanned the metadata log, and about 0.5 ms with the index.
- `Refresh` on a reader copies the tree (13 ms for 100,000 nodes) and checks
  every entry below `Len` for a destroy marker, so call it when the replica has
  grown, not on every entry. `Snapshot` of the current version copies the tree
  the same way; an older one replays the log up to it.
- A create or move walks up to `MaxDepth` parents, so a hostile log of nodes at
  the bound costs the reader about 130 microseconds per op (50,000 creates under
  a chain of 2,040 folders took 6.4 s). Lower `WithMaxDepth` for a volume whose
  owner you do not trust.
- `WriteFile` holds the volume's change lock from the first chunk to the op's
  append, and `Prefetch` holds it for every file it reads, so `Mkdir`, `Move`,
  `Delete`, `Refresh`, `SetMark` and `Close` wait for either. End a long one with
  its context.
- `SetMark`, and a `Refresh` that replays in full or sees a move, scan every key
  this volume holds in the local backend, so their cost grows with the objects
  kept on the device.
