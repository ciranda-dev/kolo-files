package kolofiles_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	kolofiles "github.com/ciranda-dev/kolo-files"
)

// shortNames reports whether the disk under dir gives a long name an 8.3 alias.
func shortNames(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "longprobename.txt")
	if err := os.WriteFile(probe, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "LONGPR~1.TXT"))
	return err == nil
}

// TestCheckoutShortName writes a long name and then the 8.3 alias Windows gave it, for a file and for a folder:
// checkout must report fs.ErrExist and keep the first one.
func TestCheckoutShortName(t *testing.T) {
	if !shortNames(t, t.TempDir()) {
		t.Skip("8.3 names are off on this disk")
	}
	for _, c := range []struct {
		names []string
		keep  string
	}{
		{[]string{"longfilename.txt", "longfi~1.txt"}, "longfilename.txt"},
		{[]string{"program files test/", "program files test/a", "progra~1/", "progra~1/b"}, "program files test/a"},
	} {
		dir := t.TempDir()
		if err := checkout(volumeWith(t, c.names...), dir); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("checkout of %q: %v, want fs.ErrExist", c.names, err)
		}
		if got := readFile(t, filepath.Join(dir, filepath.FromSlash(c.keep))); got != c.keep {
			t.Fatalf("%s holds %q after its alias", c.keep, got)
		}
	}
}

// TestCopyFSOnWindows shows what os.CopyFS does with names LocalPath refuses: it reports no error, drops the
// trailing dot and writes a device name with an extension, so the disk does not hold the volume's names. checkout
// refuses them.
func TestCopyFSOnWindows(t *testing.T) {
	v := volumeWith(t, "d/", "d/NUL.txt", "d/trailing.")
	dir := t.TempDir()
	if err := os.CopyFS(dir, v); err == nil {
		if got := listing(t, filepath.Join(dir, "d")); slices.Equal(got, []string{"NUL.txt", "trailing."}) {
			t.Fatalf("os.CopyFS kept the volume's names %q", got)
		}
	}
	if err := checkout(v, t.TempDir()); !errors.Is(err, kolofiles.ErrUnsafeName) {
		t.Fatalf("checkout: %v, want ErrUnsafeName", err)
	}
}
