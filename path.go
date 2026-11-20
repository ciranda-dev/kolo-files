package kolofiles

import (
	"fmt"
	"io/fs"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// parsePath checks a path given to the API and returns its names, nil for the root. It normalizes the path to
// NFC first. A path that breaks a rule is fs.ErrInvalid and one over a bound ErrLimit. With create, the last name
// must also hold only code points assigned in this Go release's Unicode tables.
func (o *options) parsePath(name string, create bool) ([]string, error) {
	if !utf8.ValidString(name) {
		return nil, fs.ErrInvalid
	}
	if !norm.NFC.IsNormalString(name) {
		name = norm.NFC.String(name)
	}
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	if name == "." {
		return nil, nil
	}
	if strings.Count(name, "/") >= o.maxDepth {
		return nil, fmt.Errorf("%w: deeper than %d", ErrLimit, o.maxDepth)
	}
	names := strings.Split(name, "/")
	for _, n := range names {
		if len(n) > o.maxName {
			return nil, fmt.Errorf("%w: name longer than %d bytes", ErrLimit, o.maxName)
		}
		if hasControl(n) {
			return nil, fs.ErrInvalid
		}
	}
	if create && !assigned(names[len(names)-1]) {
		return nil, fs.ErrInvalid
	}
	return names, nil
}

// checkName checks a name read from the log: every rule but the assignment check, which depends on the writer's
// Unicode version. The decoder has already checked that it is UTF-8.
func checkName(name string, maxName int) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("%w: empty, . or .. name", ErrCorrupt)
	case len(name) > maxName:
		return fmt.Errorf("%w: name longer than %d bytes", ErrLimit, maxName)
	case strings.IndexByte(name, '/') >= 0:
		return fmt.Errorf("%w: name with a slash", ErrCorrupt)
	case hasControl(name):
		return fmt.Errorf("%w: name with a control character", ErrCorrupt)
	case !norm.NFC.IsNormalString(name):
		return fmt.Errorf("%w: name not in NFC", ErrCorrupt)
	}
	return nil
}

// hasControl reports whether s holds a character of Unicode general category Cc.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) {
			return true
		}
	}
	return false
}

// assigned reports whether every code point of s has a general category other than Cn (unassigned or
// noncharacter), in the Unicode version of this Go release, which is also the version of its norm tables. Go's
// unicode.C also holds Cn, so the other categories are listed one by one.
func assigned(s string) bool {
	for _, r := range s {
		if !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}
