# Formats

The metadata log, object tables and the keys a reader stores, and the references.

## Metadata log and object tables

A metadata log entry is one op in
[kolo-codec](https://github.com/ciranda-dev/kolo-codec) form, with the log
value encoding `kololog.Bytes`:

    op      = string("kolo-files/op/v0") ‖ uvarint(kind) ‖ deps ‖ body
    deps    = uvarint(0)    (only 0 is valid: no other writer)
    node    = fixed[16]
    time    = varint(Unix time in nanoseconds, from the writer's clock)
    list    = uvarint(n) ‖ (n > 0: uvarint(first) ‖ (n-1) × uvarint(gap - 1))
    version = fixed[32] blob ID ‖ uvarint(size) ‖ fixed[32] table ID ‖ uvarint(table size) ‖ locations
    locations = uvarint(n) ‖ n × (fixed[32] object hash ‖ uvarint(content log index)), ascending by hash

    0 VOLUME  fixed[32] volume ID ‖ fixed[32] content log ID
    1 MKDIR   node ‖ node(parent) ‖ string(name) ‖ time
    2 CREATE  node ‖ node(parent) ‖ string(name) ‖ time ‖ fixed[32] file key ‖ version
    3 WRITE   node ‖ time ‖ version
    4 MOVE    node ‖ node(new parent) ‖ string(new name)
    5 REMOVE  node
    6 PURGE   node(removed) ‖ list(metadata indexes) ‖ list(content indexes) ‖ bool(done)

Entry 0 is a VOLUME op: the volume ID is the metadata log's ID, and the
content log ID names the second log. The root is the all-zero node ID. An
object table is a blob under the file key whose plaintext is

    string("kolo-files/table/v0") ‖ locations

listing every distinct object of the content blob. A reader refuses, with
`ErrCorrupt` (or `ErrLimit` for a name over its bound): another tag or kind,
non-minimal varints, trailing bytes, a name that is empty, `.`, `..`, has a
`/` or a control character, or is not NFC or valid UTF-8, a list or location
count larger than the bytes left or than a blob of that size can hold, a list
that is not strictly ascending or overflows, locations not ascending, an op over
the largest entry kolo-sync carries, non-empty `deps`, a VOLUME anywhere but
entry 0 or entry 0 that is not a VOLUME, a VOLUME whose volume ID is not the
log's ID, a CREATE or MKDIR of an existing node or of one a REMOVE dropped, a WRITE, MOVE or REMOVE of a
node that does not exist, a WRITE of a directory, a parent that is a file, a
create or move onto a taken name, a move of the root or into the node's own
subtree, a REMOVE of the root, a PURGE of a node that was not removed or is
already purged, an index list naming entry 0, an entry at or after its PURGE,
a REMOVE or PURGE op it can read, or an op it can read about a node that is not
the removed node or below it, a table that does not list the blob's
root, and a blob or table whose size is not the one its op gives. A parent that
does not exist is allowed in CREATE, MKDIR and MOVE only because of purges: a
node moved out of a directory before that directory was deleted keeps, in its
earlier ops, a parent whose ops are destroyed; it is in no path until a later
move puts it under a live parent.

The local backend of a reader holds a mark as
`"kolo-files/v0/" ‖ volume ID ‖ 'm' ‖ node ID ‖ uint64(create entry)` with the
mark as one byte, and a kept object as
`"kolo-files/v0/" ‖ volume ID ‖ 'o' ‖ node ID ‖ uint64(create entry) ‖ object hash`.
There is no JSON form.

## References

- Go `io/fs`: path syntax (`ValidPath`), the `FS`, `ReadDirFS`, `StatFS`,
  `File` and `ReadDirFile` interfaces, `PathError` and its sentinels.
  `path/filepath.Localize` and `os.Root` (`Mkdir`, `O_CREATE|O_EXCL`) for writing a volume to disk, after the Windows file name rules of `LocalPath`.
- [Unicode Standard Annex #15, Unicode Normalization Forms](https://www.unicode.org/reports/tr15/tr15-58.html) (revision 58, Unicode 18.0.0, 12 August 2026).
  Takes: NFC for names, and its stability policy for assigned characters.
  Not implemented: NFKC and NFKD.
- POSIX filename rules (no NUL, no `/`), Linux `PATH_MAX` for the default depth
  and Windows and ext4 component lengths for the default name length.
- kolo-blobs, which builds on RFC 5869 (HKDF), RFC 2104 (HMAC), NIST SP 800-38D
  (AES-GCM), SHA-256 and FastCDC: file keys, objects and the tree under every
  file. Its references are in its own formats page.
- [RFC 9562, Universally Unique IDentifiers (UUIDs)](https://www.rfc-editor.org/info/rfc9562) (Proposed Standard, May 2024).
  Not implemented: node IDs are 16 random bytes, deliberately not UUIDs.
