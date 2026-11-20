// Command basic keeps a small tree in a volume on disk, in the directory given as argument, then lists it, checks
// out an older version, compares two versions and deletes a file.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	kolofiles "github.com/ciranda-dev/kolo-files"
	koloid "github.com/ciranda-dev/kolo-id"
	kolostore "github.com/ciranda-dev/kolo-store"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: basic DIR")
	}
	ctx := context.Background()
	db, err := kolostore.OpenPebble(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	st := kolostore.New(db)
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

	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	must(v.Mkdir("notes"))
	must(v.WriteFile(ctx, "notes/todo.txt", strings.NewReader("buy bread")))
	first := v.Len()
	must(v.WriteFile(ctx, "notes/todo.txt", strings.NewReader("buy bread and milk")))
	must(v.Move("notes/todo.txt", "notes/shopping.txt"))
	must(v.WriteFile(ctx, "draft.txt", strings.NewReader("not for keeping")))

	list := func(name string, fsys fs.FS) {
		fmt.Println(name)
		must(fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(fsys, p)
			fmt.Printf("  %s: %s\n", p, b)
			return err
		}))
	}
	list("now:", v)
	old, err := v.Snapshot(first)
	must(err)
	list(fmt.Sprintf("at version %d:", first), old)

	changes, err := v.Diff(first, v.Len())
	must(err)
	for _, c := range changes {
		fmt.Printf("changed: %q -> %q, content modified: %v\n", c.Old, c.New, c.Modified)
	}

	// Delete destroys the name, the key and every object of the file; no version shows it again.
	must(v.Delete("draft.txt"))
	_, err = v.Stat("draft.txt")
	fmt.Println("draft.txt after delete:", err)
}
