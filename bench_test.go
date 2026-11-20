package kolofiles

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"
)

// heapOf returns the heap growth in bytes while f builds and returns something it keeps.
func heapOf(f func() any) (float64, any) {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	keep := f()
	runtime.GC()
	runtime.ReadMemStats(&b)
	return float64(b.HeapAlloc) - float64(a.HeapAlloc), keep
}

func benchID(i int) NodeID {
	var id NodeID
	binary.BigEndian.PutUint64(id[8:], uint64(i)+1)
	return id
}

// BenchmarkStateNode reports the heap a live node takes in a tree built by state.apply: one MKDIR per node, under
// 1,000 directories, with a name of 10 bytes.
func BenchmarkStateNode(b *testing.B) {
	const n = 200_000
	for b.Loop() {
		heap, keep := heapOf(func() any {
			st := newState()
			for i := range n {
				o := &op{kind: kindMkdir, node: benchID(i), name: fmt.Sprintf("name-%05d", i)}
				if i >= 1000 {
					o.parent = benchID(i % 1000)
				}
				if _, err := st.apply(uint64(i)+1, o, DefaultMaxDepth, nil); err != nil {
					b.Fatal(err)
				}
			}
			return st
		})
		b.ReportMetric(heap/n, "heap-B/node")
		runtime.KeepAlive(keep)
	}
}

// BenchmarkReplayEntry reports the heap one decoded MKDIR op takes while replay holds every op of the log.
func BenchmarkReplayEntry(b *testing.B) {
	const n = 200_000
	for b.Loop() {
		heap, keep := heapOf(func() any {
			ops := make([]*op, 0, n)
			for i := range n {
				o, err := decodeOp(encodeOp(&op{kind: kindMkdir, node: benchID(i), name: fmt.Sprintf("name-%05d", i)}), DefaultMaxNameLength, false)
				if err != nil {
					b.Fatal(err)
				}
				ops = append(ops, o)
			}
			return ops
		})
		b.ReportMetric(heap/n, "heap-B/entry")
		runtime.KeepAlive(keep)
	}
}

// BenchmarkIndexNode reports the heap the writer's index of entries takes per live node with one op.
func BenchmarkIndexNode(b *testing.B) {
	const n = 200_000
	for b.Loop() {
		heap, keep := heapOf(func() any {
			m := map[NodeID][]uint64{}
			for i := range n {
				m[benchID(i)] = append(m[benchID(i)], uint64(i))
			}
			return m
		})
		b.ReportMetric(heap/n, "heap-B/node")
		runtime.KeepAlive(keep)
	}
}
