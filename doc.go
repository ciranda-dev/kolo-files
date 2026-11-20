// Package kolofiles keeps a tree of files and folders with its whole history in two Kolo logs: names,
// folders and versions on top of kolo-blobs.
//
// A Volume is a metadata log of ops (one entry per change: mkdir, create, write, move, remove, purge) and a
// content log of kolo-blobs objects, both kolo-store logs of one owner. The tree is rebuilt by replaying the
// metadata log; every op is a version, so any earlier tree can be checked out with Snapshot and compared
// with Diff. Each file has its own kolo-blobs key, kept inside its create op, so a pinning peer that holds
// both logs sees neither names, keys nor content. Delete destroys every name, key and object of a file or
// folder with kolo-log's deletion. Volume and Snapshot implement io/fs. A reader opens a replica of the
// metadata log and reads content from a content replica or, entry by entry, from another peer; marks
// choose per path which files a device keeps.
//
// Experimental: the API may change before v1.0.0.
package kolofiles
