package kolofiles_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	kolofiles "github.com/ciranda-dev/kolo-files"
	koloid "github.com/ciranda-dev/kolo-id"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// volumeWith is a volume in memory holding names: a name ending in a slash is a folder, any other a file whose
// content is its name.
func volumeWith(t *testing.T, names ...string) *kolofiles.Volume {
	t.Helper()
	st := kolostore.New(kolostore.NewMemory())
	t.Cleanup(func() { st.Close() })
	me, err := koloid.Generate()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Create(me)
	if err != nil {
		t.Fatal(err)
	}
	content, err := st.Create(me)
	if err != nil {
		t.Fatal(err)
	}
	v, err := kolofiles.Create(meta, content)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	for _, name := range names {
		if dir, ok := strings.CutSuffix(name, "/"); ok {
			err = v.Mkdir(dir)
		} else {
			err = v.WriteFile(context.Background(), name, strings.NewReader(name))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// caseInsensitive reports whether the disk under dir folds case, as the macOS and Windows defaults do.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(dir, "probe"))
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	return err == nil
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		names = append(names, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCheckoutCaseAlias writes a file pair and a folder pair that differ only in case: a case-insensitive disk must
// report fs.ErrExist and keep the first file, a case-sensitive disk must hold both.
func TestCheckoutCaseAlias(t *testing.T) {
	for _, c := range []struct {
		names []string
		keep  string
	}{
		{[]string{"README", "readme"}, "README"},
		{[]string{"Docs/", "Docs/a", "docs/", "docs/b"}, "Docs/a"},
	} {
		dir := t.TempDir()
		err := checkout(volumeWith(t, c.names...), dir)
		if caseInsensitive(t, dir) {
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("checkout of %q on a case-insensitive disk: %v, want fs.ErrExist", c.names, err)
			}
			if got := readFile(t, filepath.Join(dir, filepath.FromSlash(c.keep))); got != c.keep {
				t.Fatalf("%s holds %q after the alias", c.keep, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("checkout of %q: %v", c.names, err)
		}
		if got, want := listing(t, dir), trimSlash(c.names); !slices.Equal(got, want) {
			t.Fatalf("disk holds %q, want %q", got, want)
		}
	}
}

func trimSlash(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.TrimSuffix(n, "/")
	}
	return out
}

// TestCheckoutLinkOutside puts a link to another directory in the destination, under the name of a folder of the
// volume, and a dangling link under the name of a file: checkout must refuse both and write nothing through them.
func TestCheckoutLinkOutside(t *testing.T) {
	for _, names := range [][]string{{"notes/", "notes/todo.txt"}, {"todo.txt"}} {
		dir, target := t.TempDir(), filepath.Join(t.TempDir(), "target")
		if err := os.Symlink(target, filepath.Join(dir, strings.TrimSuffix(names[0], "/"))); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		folder := strings.HasSuffix(names[0], "/")
		if folder {
			if err := os.Mkdir(target, 0o777); err != nil {
				t.Fatal(err)
			}
		}
		if err := checkout(volumeWith(t, names...), dir); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("checkout of %q over a link: %v, want fs.ErrExist", names, err)
		}
		if folder {
			if got := listing(t, target); len(got) != 0 {
				t.Fatalf("checkout wrote %q through the link", got)
			}
		} else if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("checkout created the link's target: %v", err)
		}
	}
}

// TestCopyFSIsNotACheckout shows why the docs do not offer os.CopyFS for writing a volume to disk: on a
// case-insensitive disk it merges two folders into one, and it follows a link already in the destination.
func TestCopyFSIsNotACheckout(t *testing.T) {
	dir := t.TempDir()
	if caseInsensitive(t, dir) {
		if err := os.CopyFS(dir, volumeWith(t, "Docs/", "Docs/a", "docs/", "docs/b")); err != nil {
			t.Fatalf("os.CopyFS: %v", err)
		}
		if got := listing(t, dir); !slices.Equal(got, []string{"Docs", "Docs/a", "Docs/b"}) {
			t.Fatalf("os.CopyFS made %q, want one merged folder", got)
		}
	}

	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "notes")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.CopyFS(dir, volumeWith(t, "notes/", "notes/todo.txt")); err != nil {
		t.Fatalf("os.CopyFS: %v", err)
	}
	if got := listing(t, outside); !slices.Equal(got, []string{"todo.txt"}) {
		t.Fatalf("os.CopyFS wrote %q through the link, want todo.txt", got)
	}
}

// TestGuideShowsCheckout keeps the recipe in the guide equal to the compiled one in example_test.go.
func TestGuideShowsCheckout(t *testing.T) {
	src, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "// checkout writes")
	end := strings.Index(string(src[max(start, 0):]), "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("checkout not found in example_test.go")
	}
	if fn := string(src[start : start+end+3]); !strings.Contains(string(guide), fn) {
		t.Fatalf("docs/guide.md does not show checkout as example_test.go defines it:\n%s", fn)
	}
}
