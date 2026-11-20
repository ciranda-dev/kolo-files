package kolofiles

import (
	"bytes"
	"errors"
	"io/fs"
	"math"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	koloblobs "github.com/ciranda-dev/kolo-blobs"
	"golang.org/x/text/unicode/norm"
)

func seedOps() []*op {
	return []*op{
		{kind: kindVolume, volume: [32]byte{1}, contentLog: [32]byte{2}},
		{kind: kindMkdir, node: NodeID{1}, parent: NodeID{2}, name: "d", time: -5},
		{kind: kindCreate, node: NodeID{1}, parent: NodeID{2}, name: "f", time: 1 << 60, key: [32]byte{7}, ver: smallVersion()},
		{kind: kindWrite, node: NodeID{1}, time: 3, ver: smallVersion()},
		{kind: kindMove, node: NodeID{1}, parent: NodeID{3}, name: "g"},
		{kind: kindRemove, node: NodeID{1}},
		{kind: kindPurge, node: NodeID{1}, meta: []uint64{1, 2, 9}, objs: []uint64{0, math.MaxUint64}, done: true},
	}
}

// An op that decodes encodes back to the same bytes: one op, one encoding. A refused entry is ErrCorrupt or
// ErrLimit, and an accepted name obeys every rule.
func FuzzDecodeOp(f *testing.F) {
	for _, o := range seedOps() {
		f.Add(encodeOp(o), true)
	}
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, b []byte, withKey bool) {
		o, err := decodeOp(b, DefaultMaxNameLength, withKey)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrLimit) {
				t.Fatalf("error %v is neither ErrCorrupt nor ErrLimit", err)
			}
			return
		}
		if withKey && !bytes.Equal(encodeOp(o), b) {
			t.Fatal("decoded op encodes to other bytes")
		}
		if !withKey && o.key != ([32]byte{}) {
			t.Fatal("a key was kept")
		}
		if o.kind >= kindMkdir && o.kind != kindWrite && o.kind != kindRemove && o.kind != kindPurge {
			if err := checkName(o.name, DefaultMaxNameLength); err != nil {
				t.Fatalf("accepted name %q: %v", o.name, err)
			}
		}
		for _, list := range [][]uint64{o.meta, o.objs} {
			for i := 1; i < len(list); i++ {
				if list[i] <= list[i-1] {
					t.Fatal("list not ascending")
				}
			}
		}
	})
}

// An object table that decodes encodes back to the same bytes.
func FuzzDecodeTable(f *testing.F) {
	h := smallVersion().locs[0].h
	f.Add(encodeTable([]location{{h, 3}}), uint64(10))
	f.Add([]byte{}, uint64(0))
	f.Fuzz(func(t *testing.T, b []byte, size uint64) {
		locs, err := decodeTableBytes(b, size, koloblobs.ID(h))
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error %v does not wrap ErrCorrupt", err)
			}
			return
		}
		if !bytes.Equal(encodeTable(locs), b) {
			t.Fatal("decoded table encodes to other bytes")
		}
		if uint64(len(locs)) > maxObjects(size) {
			t.Fatalf("%d locations for a size of %d", len(locs), size)
		}
	})
}

// A path that parses gives names that obey every rule and parse again to themselves; a refused path is
// fs.ErrInvalid or ErrLimit.
func FuzzParsePath(f *testing.F) {
	for _, s := range []string{".", "a", "a/b", "/a", "a/", "a//b", "a/./b", "a/../b", "é", "é", "a\x00", "a\xff", "͸", strings.Repeat("a/", 3000)} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, name string, create bool) {
		o := &options{maxDepth: DefaultMaxDepth, maxName: DefaultMaxNameLength}
		names, err := o.parsePath(name, create)
		if err != nil {
			if !errors.Is(err, fs.ErrInvalid) && !errors.Is(err, ErrLimit) {
				t.Fatalf("error %v is neither fs.ErrInvalid nor ErrLimit", err)
			}
			return
		}
		if len(names) > o.maxDepth {
			t.Fatalf("%d names", len(names))
		}
		for _, n := range names {
			if !utf8.ValidString(n) || checkName(n, o.maxName) != nil || !norm.NFC.IsNormalString(n) {
				t.Fatalf("accepted name %q", n)
			}
		}
		if names == nil {
			return
		}
		again, err := o.parsePath(strings.Join(names, "/"), create)
		if err != nil || strings.Join(again, "/") != strings.Join(names, "/") {
			t.Fatalf("%q parses to %q, then %q, %v", name, names, again, err)
		}
	})
}

// checkState reports a way st is not a tree of its nodes.
func checkState(st *state, maxDepth int) error {
	root := st.nodes[rootID]
	if root == nil || !root.dir {
		return errors.New("no root directory")
	}
	for p, m := range st.kids {
		if len(m) == 0 {
			return errors.New("an empty child map")
		}
		if n := st.nodes[p]; n != nil && !n.dir {
			return errors.New("a file with children")
		}
		for name, id := range m {
			if n := st.nodes[id]; n == nil || n.parent != p || n.name != name {
				return errors.New("a child entry that does not match its node")
			}
		}
	}
	for id, n := range st.nodes {
		if id == rootID {
			continue
		}
		if st.kids[n.parent][n.name] != id {
			return errors.New("a node missing from its parent")
		}
		if _, _, err := st.path(id, maxDepth); err != nil && !errors.Is(err, ErrLimit) {
			return err
		}
		if _, _, _, err := st.depth(id, NodeID{}, maxDepth); err != nil && !errors.Is(err, ErrLimit) {
			return err
		}
	}
	return nil
}

// Random sequences of ops over a few nodes and names either apply, leaving a consistent tree within the depth
// bound, or stop at ErrCorrupt or ErrLimit; none panics.
func FuzzApply(f *testing.F) {
	f.Add([]byte{1, 1, 0, 0, 1, 2, 1, 1, 4, 1, 0, 0, 3, 1, 2, 1, 5, 1, 0, 0})
	f.Add([]byte{1, 1, 0, 0, 1, 2, 1, 0, 3, 1, 2, 0, 3, 2, 1, 0, 5, 2, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		const maxDepth = 4
		st := newState()
		for i := uint64(0); len(data) >= 4 && i < 64; i, data = i+1, data[4:] {
			o := &op{kind: uint64(data[0]%5) + kindMkdir, node: NodeID{data[1] % 6}, parent: NodeID{data[2] % 6}, name: string(rune('a' + data[3]%3)), ver: smallVersion()}
			purged := func(NodeID) bool { return data[3]%2 == 0 }
			if _, err := st.apply(i, o, maxDepth, purged); err != nil {
				if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrLimit) {
					t.Fatalf("error %v is neither ErrCorrupt nor ErrLimit", err)
				}
				return
			}
			if err := checkState(st, maxDepth); err != nil {
				t.Fatalf("after op %d (kind %d): %v", i, o.kind, err)
			}
		}
	})
}

// refReserved states the device name rule a second way, for FuzzLocalPath: none of these letters has a non-ASCII
// case partner in Go's folding, so (?i) folds ASCII only, as Windows does.
var refReserved = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[1-9¹²³]|LPT[1-9¹²³]) *(\..*)?$`)

// refWindowsName is the Windows name rule written independently of windowsName.
func refWindowsName(n string) bool {
	if n == "" || strings.ContainsAny(n, `<>:"/\|?*`) || strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") {
		return false
	}
	for _, r := range n {
		if r < 0x20 {
			return false
		}
	}
	return len(utf16.Encode([]rune(n))) <= maxWindowsName && !refReserved.MatchString(n)
}

func FuzzLocalPath(f *testing.F) {
	for _, s := range append(append([]string{"", ".", "a/b", "x/NUL.txt", "conın$", "\xff", "a\x00"}, windowsRefused...), windowsAllowed...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got, err := LocalPath(name)
		var pe *fs.PathError
		if err != nil && (!errors.As(err, &pe) || pe.Path != name || got != "" ||
			!errors.Is(err, fs.ErrInvalid) && !errors.Is(err, ErrUnsafeName)) {
			t.Fatalf("LocalPath(%q) = %q, %v", name, got, err)
		}
		if !fs.ValidPath(name) || strings.Contains(name, "\x00") || !utf8.ValidString(name) {
			if !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("LocalPath(%q) = %v, want fs.ErrInvalid", name, err)
			}
			return
		}
		ref := true
		if name != "." {
			for c := range strings.SplitSeq(name, "/") {
				ref = ref && refWindowsName(c)
			}
		}
		if werr := windowsPath(name); (werr == nil) != ref {
			t.Fatalf("windowsPath(%q) = %v, the reference rule says allowed=%v", name, werr, ref)
		}
		if runtime.GOOS == "windows" {
			if (err == nil) != ref {
				t.Fatalf("LocalPath(%q) = %v on Windows, the reference rule says allowed=%v", name, err, ref)
			}
		} else if err != nil || got != filepath.FromSlash(name) {
			t.Fatalf("LocalPath(%q) = %q, %v on %s", name, got, err, runtime.GOOS)
		}
	})
}

// A log of random folder, move, remove and purge ops, with purge lists over earlier entries, opens as a writer and as
// a reader with the same verdict, and neither panics.
func FuzzReplay(f *testing.F) {
	f.Add([]byte{1, 1, 0, 0, 0, 5, 1, 0, 0, 0, 6, 1, 0, 0, 1})
	f.Add([]byte{1, 1, 0, 0, 0, 1, 2, 1, 1, 0, 5, 1, 0, 0, 0, 6, 1, 0, 0, 6, 6, 1, 0, 3, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		var ops []*op
		for ; len(data) >= 5 && len(ops) < 24; data = data[5:] {
			o := &op{kind: uint64(data[0]%6) + kindMkdir, node: NodeID{data[1]%5 + 1}, parent: NodeID{data[2] % 5}, name: string(rune('a' + data[3]%3))}
			switch o.kind {
			case kindCreate, kindWrite:
				o.kind = kindMkdir
			case kindPurge:
				for j := uint64(1); j <= uint64(len(ops)); j++ {
					if data[3]>>(j%8)&1 == 1 {
						o.meta = append(o.meta, j)
					}
				}
				o.done = data[4]%2 == 0
			}
			ops = append(ops, o)
		}
		hostileOpts(t, nil, ops...)
	})
}
