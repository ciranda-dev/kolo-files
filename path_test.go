package kolofiles

import (
	"errors"
	"io/fs"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

func TestNameRulesWriter(t *testing.T) {
	v, _ := newVolume(t)
	long := strings.Repeat("é", DefaultMaxNameLength/2) // 764 bytes, one under the bound
	good := []string{
		"café", "README", "readme", "😀", "שלום", "مرحبا", "CON", "NUL.txt", "a:b", `a\b`, "trailing.", "trailing ",
		`<>|?*"`, long, "", "日本語", "x" + strings.Repeat("́", 3),
	}
	for _, name := range good {
		if err := v.Mkdir(name); err != nil {
			t.Errorf("Mkdir(%q): %v", name, err)
		}
	}
	for _, name := range good {
		fi, err := v.Stat(name)
		if err != nil {
			t.Errorf("Stat(%q): %v", name, err)
			continue
		}
		if want := norm.NFC.String(name); fi.Name() != want {
			t.Errorf("stored %q, want %q", fi.Name(), want)
		}
	}
	bad := []struct {
		name string
		want error
	}{
		{"", fs.ErrInvalid}, {"/a", fs.ErrInvalid}, {"a/", fs.ErrInvalid}, {"café//b", fs.ErrInvalid},
		{"..", fs.ErrInvalid}, {"café/..", fs.ErrInvalid}, {"./café", fs.ErrInvalid}, {"a\x00b", fs.ErrInvalid},
		{"a\x01", fs.ErrInvalid}, {"\x1f", fs.ErrInvalid}, {"\x7f", fs.ErrInvalid}, {"a\u0085", fs.ErrInvalid},
		{"\u009f", fs.ErrInvalid}, {"\xff", fs.ErrInvalid}, {"a\xc3", fs.ErrInvalid}, {"͸", fs.ErrInvalid},
		{"﷐", fs.ErrInvalid}, {"\U0010fffe", fs.ErrInvalid}, {long + "ab", ErrLimit},
	}
	for _, tc := range bad {
		err := v.Mkdir(tc.name)
		var pe *fs.PathError
		if !errors.Is(err, tc.want) || !errors.As(err, &pe) {
			t.Errorf("Mkdir(%q) = %v, want %v", tc.name, err, tc.want)
		}
		if err := v.WriteFile(testCtx(t), tc.name, strings.NewReader("x")); !errors.Is(err, tc.want) && !errors.Is(err, ErrIsDir) {
			t.Errorf("WriteFile(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	// NFD input finds the NFC name; case is kept and compared byte for byte.
	if fi, err := v.Stat("café"); err != nil || fi.Name() != "café" {
		t.Fatalf("NFD lookup: %v", err)
	}
	if err := v.Mkdir("café"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("NFD duplicate: %v", err)
	}
	move(t, v, "README", "ReadMe")
	if _, err := v.Stat("README"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("case-only rename kept the old name")
	}
	if _, err := v.Stat("readme"); err != nil {
		t.Fatal("case-only rename touched another name")
	}
	ents, _ := v.ReadDir(".")
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
	}
	if !slices.IsSorted(names) {
		t.Fatalf("listing not in byte order: %q", names)
	}
	// Windows-only names are stored; LocalPath refuses them for a Windows disk and only there (see TestLocalPath).
	for _, name := range []string{"CON", "NUL.txt", "a:b", `a\b`, `<>|?*"`, "trailing.", "trailing "} {
		_, err := LocalPath(name)
		if refused := errors.Is(err, ErrUnsafeName); refused != (runtime.GOOS == "windows") || err != nil && !refused {
			t.Errorf("LocalPath(%q) on %s = %v", name, runtime.GOOS, err)
		}
	}
	// Lookups do not check assignment, so a newer writer's names stay reachable.
	if _, err := v.Stat("͸"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lookup of an unassigned code point: %v", err)
	}
}

func TestNormalizationCollisions(t *testing.T) {
	const nfc = "café"
	nfd := norm.NFD.String(nfc)
	if nfd == nfc {
		t.Fatal("test strings are equal")
	}
	names := func(v *Volume) []string {
		ents, err := v.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}

	v, _ := newVolume(t)
	write(t, v, nfc, []byte("one"))
	id := nodeOf(t, v, nfc).ID
	first := v.Len()

	// WriteFile through the NFD path replaces the same node.
	write(t, v, nfd, []byte("two"))
	if got := nodeOf(t, v, nfc).ID; got != id {
		t.Fatalf("NFD write made node %x, want %x", got, id)
	}
	if got := read(t, v, nfc); string(got) != "two" {
		t.Fatalf("content %q", got)
	}
	if got := names(v); !slices.Equal(got, []string{nfc}) {
		t.Fatalf("listing %q", got)
	}

	// Diff reports the NFC name.
	ch, err := v.Diff(first, v.Len())
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 1 || ch[0].Old != nfc || ch[0].New != nfc || !ch[0].Modified {
		t.Fatalf("diff %+v", ch)
	}

	// Mkdir and Move onto the NFD form of an existing name are refused.
	if err := v.Mkdir(nfd); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Mkdir NFD: %v", err)
	}
	write(t, v, "other", []byte("x"))
	err = v.Move("other", nfd)
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrExist) || !errors.As(err, &pe) {
		t.Fatalf("Move onto NFD: %v", err)
	}
	if got := read(t, v, "other"); string(got) != "x" {
		t.Fatalf("refused move changed the source: %q", got)
	}

	// A move to the node's own NFD form does nothing.
	n := v.Len()
	move(t, v, nfc, nfd)
	if v.Len() != n {
		t.Fatal("a move to the NFD form of the same name appended an op")
	}
	if got := names(v); !slices.Equal(got, []string{nfc, "other"}) {
		t.Fatalf("listing %q", got)
	}

	// Delete through the NFD path removes the NFC entry.
	del(t, v, nfd)
	if _, err := v.Stat(nfc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after delete: %v", err)
	}
	if got := names(v); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("listing %q", got)
	}
}

func TestUnicodeTables(t *testing.T) {
	if unicode.Version != norm.Version {
		t.Fatalf("unicode %s and norm %s tables differ", unicode.Version, norm.Version)
	}
}

func TestBounds(t *testing.T) {
	v, _ := newVolume(t, WithMaxDepth(3), WithMaxNameLength(10))
	mkdir(t, v, "aaaaaaaaaa")
	if err := v.Mkdir("aaaaaaaaaab"); !errors.Is(err, ErrLimit) {
		t.Fatalf("11-byte name: %v", err)
	}
	if err := v.Mkdir("é" + strings.Repeat("a", 9)); !errors.Is(err, ErrLimit) {
		t.Fatalf("11-byte name after NFC: %v", err)
	}
	mkdir(t, v, "aaaaaaaaaa/b")
	mkdir(t, v, "aaaaaaaaaa/b/c")
	if err := v.Mkdir("aaaaaaaaaa/b/c/d"); !errors.Is(err, ErrLimit) {
		t.Fatalf("depth 4: %v", err)
	}
	if err := v.WriteFile(testCtx(t), "aaaaaaaaaa/b/c/d", strings.NewReader("x")); !errors.Is(err, ErrLimit) {
		t.Fatalf("file at depth 4: %v", err)
	}
	if _, err := v.Stat("aaaaaaaaaa/b/c/d"); !errors.Is(err, ErrLimit) {
		t.Fatalf("Stat at depth 4: %v", err)
	}
	// A move whose subtree would end below the bound is refused; one that fits is not.
	mkdir(t, v, "x")
	mkdir(t, v, "x/y")
	if err := v.Move("aaaaaaaaaa/b", "x/y/b"); !errors.Is(err, ErrLimit) {
		t.Fatalf("move past the depth: %v", err)
	}
	move(t, v, "aaaaaaaaaa/b/c", "x/y/c")
	// A reader with smaller bounds refuses the log with ErrLimit; one with the same bounds opens it.
	if err := hostileOpts(t, []Option{WithMaxNameLength(9)}, &op{kind: kindMkdir, node: NodeID{1}, name: "aaaaaaaaaa"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("name over the reader's bound: %v", err)
	}
	if err := hostileOpts(t, []Option{WithMaxDepth(1)}, &op{kind: kindMkdir, node: NodeID{1}, name: "a"}, &op{kind: kindMkdir, node: NodeID{2}, parent: NodeID{1}, name: "b"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("depth over the reader's bound: %v", err)
	}
	if err := hostileOpts(t, []Option{WithMaxDepth(2)}, &op{kind: kindMkdir, node: NodeID{1}, name: "a"}, &op{kind: kindMkdir, node: NodeID{2}, parent: NodeID{1}, name: "b"}); err != nil {
		t.Fatalf("depth at the reader's bound: %v", err)
	}
	if err := hostile(t, &op{kind: kindMkdir, node: NodeID{1}, name: strings.Repeat("a", DefaultMaxNameLength+1)}); !errors.Is(err, ErrLimit) {
		t.Fatalf("name over the default bound: %v", err)
	}
	if err := hostile(t, &op{kind: kindMkdir, node: NodeID{1}, name: strings.Repeat("a", DefaultMaxNameLength)}); err != nil {
		t.Fatalf("name at the default bound: %v", err)
	}
}

func TestParsePath(t *testing.T) {
	o, _ := newOptions(nil)
	for _, tc := range []struct {
		in   string
		want []string
	}{{".", nil}, {"a", []string{"a"}}, {"a/b", []string{"a", "b"}}, {"café/x", []string{"café", "x"}}, {"Å", []string{"Å"}}} {
		got, err := o.parsePath(tc.in, true)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("parsePath(%q) = %q, %v", tc.in, got, err)
		}
	}
	deep := strings.TrimSuffix(strings.Repeat("a/", DefaultMaxDepth), "/")
	if _, err := o.parsePath(deep, false); err != nil {
		t.Fatalf("path at the depth: %v", err)
	}
	if _, err := o.parsePath(deep+"/a", false); !errors.Is(err, ErrLimit) {
		t.Fatalf("path over the depth: %v", err)
	}
}

// An open directory's errors carry the name as the caller gave it, not the stored one.
func TestDirFileErrorPaths(t *testing.T) {
	v, _ := newVolume(t)
	stored := "café"
	given := norm.NFD.String(stored)
	mkdir(t, v, stored)
	d, err := v.Open(given)
	if err != nil {
		t.Fatal(err)
	}
	var pe *fs.PathError
	if _, err := d.Read(nil); !errors.As(err, &pe) || pe.Path != given {
		t.Fatalf("Read of a directory: %v, want path %q", err, given)
	}
	d.Close()
	if _, err := d.(fs.ReadDirFile).ReadDir(1); !errors.As(err, &pe) || pe.Path != given {
		t.Fatalf("ReadDir after Close: %v, want path %q", err, given)
	}
}
