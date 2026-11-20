package kolofiles_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	kolofiles "github.com/ciranda-dev/kolo-files"
	koloid "github.com/ciranda-dev/kolo-id"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// checkout writes the tree of v into dir, which should be empty. Each folder and file is created under an
// os.Root, with Mkdir and O_EXCL, so a name that aliases one already written (a case-only pair on a
// case-insensitive disk, a Windows 8.3 short name) fails with fs.ErrExist instead of overwriting it, and no link
// leads outside dir.
func checkout(v fs.FS, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(v, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		local, err := kolofiles.LocalPath(name)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return root.Mkdir(local, 0o777)
		}
		src, err := v.Open(name)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			dst.Close()
			return err
		}
		return dst.Close()
	})
}

// ExampleLocalPath writes a volume to a new directory on disk with checkout.
func ExampleLocalPath() {
	st := kolostore.New(kolostore.NewMemory())
	defer st.Close()
	me, err := koloid.Generate()
	if err != nil {
		log.Fatal(err)
	}
	meta, err := st.Create(me)
	if err != nil {
		log.Fatal(err)
	}
	content, err := st.Create(me)
	if err != nil {
		log.Fatal(err)
	}
	v, err := kolofiles.Create(meta, content)
	if err != nil {
		log.Fatal(err)
	}
	defer v.Close()
	if err := v.Mkdir("notes"); err != nil {
		log.Fatal(err)
	}
	if err := v.WriteFile(context.Background(), "notes/todo.txt", strings.NewReader("buy bread")); err != nil {
		log.Fatal(err)
	}

	dir, err := os.MkdirTemp("", "checkout")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := checkout(v, dir); err != nil {
		log.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "notes", "todo.txt"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(b))
	// A second checkout into the same directory overwrites nothing.
	fmt.Println(errors.Is(checkout(v, dir), fs.ErrExist))
	// Output:
	// buy bread
	// true
}
