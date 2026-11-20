package kolofiles

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"

	kololog "github.com/ciranda-dev/kolo-log"
)

// Readers, snapshots and diffs run while a writer creates, moves and deletes; the only errors they may see are
// those of a node or a version that was deleted under them.
func TestConcurrentReadsAndWrites(t *testing.T) {
	checkGoroutines(t)
	v, e := newVolume(t)
	expected := func(err error) bool {
		return err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, kololog.ErrDeleted)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	reader := func(work func() error) {
		wg.Go(func() {
			for !stop.Load() {
				if err := work(); !expected(err) {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 3 {
		reader(func() error {
			return fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				_, err = fs.ReadFile(v, p)
				return err
			})
		})
	}
	reader(func() error {
		s, err := v.Snapshot(v.Len())
		if err != nil {
			return err
		}
		_, err = fs.ReadDir(s, ".")
		return err
	})
	reader(func() error {
		n := v.Len()
		_, err := v.Diff(1, n)
		if err == nil {
			_, err = v.Snapshot(max(n/2, 1))
		}
		return err
	})
	reader(func() error { _ = v.Stats(); _ = v.Len(); return nil })

	for i := range 12 {
		d := fmt.Sprintf("d%d", i)
		mkdir(t, v, d)
		write(t, v, d+"/f", randBytes(byte(i), 100<<10))
		write(t, v, d+"/g", []byte("g"))
		move(t, v, d+"/f", d+"/h")
		write(t, v, d+"/h", randBytes(byte(i+1), 50<<10))
		if i%2 == 0 {
			del(t, v, d)
		}
	}
	stop.Store(true)
	wg.Wait()
	if got := len(tree(t, v)); got != 1+6*3 {
		t.Fatalf("tree has %d entries", got)
	}
	// A reader device refreshes while its files are read.
	r := newReader(t, e, false)
	stop.Store(false)
	reader(func() error { _, err := fs.ReadFile(r.v, "d1/g"); return err })
	for range 50 {
		if err := r.v.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
}
