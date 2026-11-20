package kolofiles

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// maxWindowsName is the longest file name Windows creates, in UTF-16 units.
const maxWindowsName = 255

// LocalPath converts a path of a volume into a path for the host operating system. On Windows it refuses with
// ErrUnsafeName a name Windows reserves for a device (some versions open the device instead) or cannot create: CON,
// NUL.txt, COM1 and the other device names, one of < > : " | ? * \, a trailing dot or space, a control character,
// or more than 255 UTF-16 units. A path that is not an io/fs path, or holds a NUL, is fs.ErrInvalid. Errors are
// *fs.PathError.
//
// LocalPath checks names only and never looks at the disk. To write a volume to disk, create each folder and file
// under an os.Root with Mkdir and O_CREATE|O_EXCL, so a name aliasing another (case, a Windows 8.3 short name) fails
// with fs.ErrExist instead of overwriting, and no link leads outside the directory, as the example does.
func LocalPath(name string) (string, error) {
	fail := func(err error) (string, error) { return "", &fs.PathError{Op: "localpath", Path: name, Err: err} }
	if !utf8.ValidString(name) || !fs.ValidPath(name) || strings.IndexByte(name, 0) >= 0 {
		return fail(fs.ErrInvalid)
	}
	if runtime.GOOS == "windows" {
		if err := windowsPath(name); err != nil {
			return fail(err)
		}
	}
	p, err := filepath.Localize(name)
	if err != nil {
		return fail(fs.ErrInvalid)
	}
	return p, nil
}

// windowsPath checks every name of a valid io/fs path against windowsName.
func windowsPath(name string) error {
	if name == "." {
		return nil
	}
	for n := range strings.SplitSeq(name, "/") {
		if err := windowsName(n); err != nil {
			return err
		}
	}
	return nil
}

// windowsName reports why Windows cannot create a file or folder called name, or reserves it for a device, wrapping
// ErrUnsafeName. It does not use the host, so it gives the same answer everywhere.
func windowsName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrUnsafeName)
	}
	units := 0
	for _, r := range name {
		if r < ' ' || strings.ContainsRune(`<>:"/\|?*`, r) {
			return fmt.Errorf("%w: character not allowed on Windows", ErrUnsafeName)
		}
		units++
		if r >= 0x10000 {
			units++
		}
	}
	if units > maxWindowsName {
		return fmt.Errorf("%w: longer than %d UTF-16 units", ErrUnsafeName, maxWindowsName)
	}
	if last := name[len(name)-1]; last == '.' || last == ' ' {
		return fmt.Errorf("%w: trailing dot or space", ErrUnsafeName)
	}
	if reservedWindowsName(name) {
		return fmt.Errorf("%w: reserved device name", ErrUnsafeName)
	}
	return nil
}

// reservedWindowsName reports whether name, up to its first dot and without trailing spaces, is a device name in
// any ASCII case: CON, PRN, AUX, NUL, CONIN$, CONOUT$, COM or LPT followed by 1 to 9 or a superscript 1, 2 or 3.
// Windows before 11 also treats such a name with an extension as the device.
func reservedWindowsName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = asciiUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if n, ok := strings.CutPrefix(stem, "COM"); ok {
		return portNumber(n)
	}
	if n, ok := strings.CutPrefix(stem, "LPT"); ok {
		return portNumber(n)
	}
	return false
}

// asciiUpper upper-cases ASCII letters only, as Windows does when it compares device names: strings.ToUpper would
// also turn a dotless i into I.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func portNumber(s string) bool {
	switch s {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}
