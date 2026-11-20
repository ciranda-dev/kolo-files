package kolofiles

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// windowsRefused are names Windows cannot create or treats as a device; windowsAllowed are close names it accepts.
var (
	windowsRefused = []string{
		"CON", "con", "Prn", "AUX", "NUL", "NUL.txt", "nul.tar.gz", "COM1", "com9", "LPT1", "lpt9.log", "COM¹", "COM²",
		"LPT³.txt", "CON .txt", "NUL ", "CONIN$", "CONOUT$",
		"a:b", "a:stream", "CON:stream", "::$DATA", "a<b", "a>b", `a"b`, "a|b", "a?b", "a*b", `a\b`, `<>|?*"`,
		"a\x01", "\x1f", "tab\there",
		"trailing.", "trailing ", "...", "a. ", "a ..",
		strings.Repeat("a", 256), strings.Repeat("😀", 128), strings.Repeat("é", 256),
	}
	windowsAllowed = []string{
		"café", "README", "a", "a b", " leading", "a.b", ".hidden", "..x", "x..y", "CONS", "NULL", "COM0", "COM10", "COM", "LPT",
		"LPT0", "COMx", "CON1", "AUX1", "PRNT", "xCON", "CON-1", "😀", "日本語", "x́", "$", "CONIN", "CONOUT", "conın$",
		strings.Repeat("a", 255), strings.Repeat("😀", 127) + "a", strings.Repeat("é", 255),
	}
)

func TestWindowsName(t *testing.T) {
	for _, name := range append(slices.Clone(windowsRefused), "") {
		if err := windowsName(name); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("windowsName(%q) = %v, want ErrUnsafeName", name, err)
		}
	}
	for _, name := range windowsAllowed {
		if err := windowsName(name); err != nil {
			t.Errorf("windowsName(%q) = %v", name, err)
		}
	}
}

func TestWindowsPath(t *testing.T) {
	for _, name := range []string{"CON", "dir/CON", "CON/x", "a/b/NUL.txt", "a/b:c/d", "a/b./c", "a/b /c", "x/" + strings.Repeat("a", 256)} {
		if err := windowsPath(name); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("windowsPath(%q) = %v, want ErrUnsafeName", name, err)
		}
	}
	for _, name := range []string{".", "a", "a/b/c", "CONS/NULL/COM0", "x/" + strings.Repeat("a", 255)} {
		if err := windowsPath(name); err != nil {
			t.Errorf("windowsPath(%q) = %v", name, err)
		}
	}
}

func TestLocalPath(t *testing.T) {
	windows := runtime.GOOS == "windows"
	for _, name := range append(slices.Clone(windowsRefused), "a/CON", "CON/a", "x/NUL.txt") {
		if strings.ContainsAny(name, "\x00") {
			continue
		}
		got, err := LocalPath(name)
		var pe *fs.PathError
		switch {
		case windows:
			if !errors.Is(err, ErrUnsafeName) || !errors.As(err, &pe) || pe.Path != name || got != "" {
				t.Errorf("LocalPath(%q) = %q, %v on Windows, want ErrUnsafeName in a PathError", name, got, err)
			}
		case err != nil:
			t.Errorf("LocalPath(%q): %v on %s, which accepts the name", name, err, runtime.GOOS)
		case got != filepath.FromSlash(name):
			t.Errorf("LocalPath(%q) = %q", name, got)
		}
	}
	for _, name := range append(slices.Clone(windowsAllowed), ".", "a/b/c", "dir/café") {
		got, err := LocalPath(name)
		if err != nil || got != filepath.FromSlash(name) {
			t.Errorf("LocalPath(%q) = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"", "/a", "a/", "a//b", "..", "a/../b", "./a", "a\x00b", "\xff"} {
		_, err := LocalPath(name)
		var pe *fs.PathError
		if !errors.Is(err, fs.ErrInvalid) || !errors.As(err, &pe) {
			t.Errorf("LocalPath(%q) = %v, want fs.ErrInvalid in a PathError", name, err)
		}
	}
}

// checkWindowsRefuses shows that Windows itself does not create a file called name in dir: it refuses a name with
// one of <>"|?*, and a name with a colon or a trailing dot or space makes a file of a shorter name, which the
// listing shows.
func checkWindowsRefuses(t *testing.T, dir, name string) {
	t.Helper()
	switch {
	case strings.ContainsAny(name, `<>"|?*`):
		if f, err := os.Create(filepath.Join(dir, name)); err == nil {
			f.Close()
			t.Errorf("Windows created a file called %q, which LocalPath refuses", name)
		}
	case strings.Contains(name, ":"), strings.HasSuffix(name, "."), strings.HasSuffix(name, " "):
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return
		}
		f.Close()
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if e.Name() == name {
				t.Errorf("Windows lists a file called %q, which LocalPath refuses", name)
			}
		}
	}
}

// TestLocalPathDisk writes the names of a volume to a temporary directory through LocalPath, the way code that
// checks a volume out does: a name LocalPath refuses is skipped, every other one must be created and listed back.
// On Windows it also checks that the system itself refuses the names with a character it cannot hold, so the rule
// is not just a copy of what we believe.
func TestLocalPathDisk(t *testing.T) {
	v, _ := newVolume(t)
	names := slices.Concat(windowsAllowed[:12], []string{"CON", "NUL.txt", "a:b", `<>|?*"`, "trailing.", "trailing "})
	if err := v.Mkdir("d"); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		write(t, v, "d/"+name, []byte(name))
	}
	dir := t.TempDir()
	var made []string
	for _, name := range names {
		local, err := LocalPath("d/" + name)
		if err != nil {
			if runtime.GOOS != "windows" || windowsName(name) == nil {
				t.Errorf("LocalPath(%q): %v", name, err)
			}
			if runtime.GOOS == "windows" {
				checkWindowsRefuses(t, dir, name)
			}
			continue
		}
		path := filepath.Join(dir, local)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o666); err != nil {
			t.Errorf("creating %q: %v", name, err)
			continue
		}
		made = append(made, name)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "d"))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range ents {
		listed = append(listed, e.Name())
	}
	slices.Sort(listed)
	slices.Sort(made)
	if !slices.Equal(listed, made) {
		t.Fatalf("disk holds %q, created %q", listed, made)
	}
	if runtime.GOOS == "windows" && len(made) != 12 {
		t.Fatalf("%d names created on Windows, want the 12 allowed ones", len(made))
	}
}
