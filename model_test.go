package kolofiles

import (
	"bytes"
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// mnode is a node of the model: a parent model ID, a name, and a directory flag or a file's content.
type mnode struct {
	parent int
	name   string
	dir    bool
	data   string
}

// model is a plain in-memory tree with one state per version, against which a Volume is checked.
type model struct {
	nodes    map[int]mnode
	next     int
	versions []map[int]mnode // versions[v] is the tree after v entries
	subject  []int           // subject[i] is the model node of entry i, -1 for none
	removes  map[int]bool    // entries that are a REMOVE
	erased   map[int]bool    // nodes destroyed by a delete
	ids      map[int]NodeID
}

func newModel() *model {
	m := &model{nodes: map[int]mnode{0: {dir: true}}, next: 1, removes: map[int]bool{}, erased: map[int]bool{}, ids: map[int]NodeID{0: rootID}}
	m.versions = []map[int]mnode{nil, clone(m.nodes)}
	m.subject = []int{-1}
	return m
}

func clone(m map[int]mnode) map[int]mnode {
	c := make(map[int]mnode, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// record adds the entries the volume appended since the last record, each with the current tree; the first is about
// subject, and is a REMOVE when remove is set.
func (m *model) record(v *Volume, subject int, remove bool) {
	first := len(m.subject)
	for uint64(len(m.subject)) < v.Len() {
		i, s := len(m.subject), -1
		if i == first {
			s = subject
			m.removes[i] = remove
		}
		m.subject = append(m.subject, s)
		m.versions = append(m.versions, clone(m.nodes))
	}
}

func (m *model) path(nodes map[int]mnode, id int) (string, bool) {
	var names []string
	for id != 0 {
		n, ok := nodes[id]
		if !ok || m.erased[id] {
			return "", false
		}
		names = append(names, n.name)
		id = n.parent
	}
	slices.Reverse(names)
	if len(names) == 0 {
		return ".", true
	}
	return strings.Join(names, "/"), true
}

// tree lists version ver as tree does, with deletion winning over history.
func (m *model) tree(ver int) []string {
	nodes := m.versions[ver]
	var out []string
	for id, n := range nodes {
		p, ok := m.path(nodes, id)
		if !ok {
			continue
		}
		if n.dir {
			out = append(out, p+"/")
		} else {
			out = append(out, p+"="+n.data)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(walkKey(a), walkKey(b)) })
	return out
}

// walkKey orders paths as fs.WalkDir visits them: by path elements in byte order.
func walkKey(s string) string {
	s = strings.TrimSuffix(strings.SplitN(s, "=", 2)[0], "/")
	if s == "." {
		return ""
	}
	return "\x00" + strings.ReplaceAll(s, "/", "\x00")
}

func sortWalk(list []string) []string {
	slices.SortFunc(list, func(a, b string) int { return cmp.Compare(walkKey(a), walkKey(b)) })
	return list
}

func (m *model) dirs() []int {
	var out []int
	for id, n := range m.nodes {
		if n.dir {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (m *model) child(parent int, name string) (int, bool) {
	for id, n := range m.nodes {
		if n.parent == parent && n.name == name && id != 0 {
			return id, true
		}
	}
	return 0, false
}

func (m *model) under(id, anc int) bool {
	for ; id != 0; id = m.nodes[id].parent {
		if id == anc {
			return true
		}
	}
	return anc == 0
}

// expectDiff computes the changes between versions a and b from the model.
func (m *model) expectDiff(a, b int) []Change {
	touched := map[int]bool{}
	for i := a; i < b; i++ {
		s := m.subject[i]
		if s < 0 || (m.erased[s] && !m.removes[i]) {
			continue
		}
		touched[s] = touched[s] || m.removes[i]
	}
	var out []Change
	for id, removed := range touched {
		if removed {
			out = append(out, Change{Node: m.ids[id], Deleted: true})
			continue
		}
		pa, okA := m.path(m.versions[a], id)
		pb, okB := m.path(m.versions[b], id)
		c := Change{Node: m.ids[id], Old: pa, New: pb}
		c.Modified = okA && okB && m.versions[a][id].data != m.versions[b][id].data
		if c.Old != c.New || c.Modified {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y Change) int {
		return cmp.Or(cmp.Compare(changePath(x), changePath(y)), bytes.Compare(x.Node[:], y.Node[:]))
	})
	return out
}

var names = []string{"a", "b", "c", "Ab", "é"}

// step makes one random valid change to v and the model.
func (m *model) step(t *testing.T, v *Volume, rng *rand.Rand) {
	dirs := m.dirs()
	parent := dirs[rng.IntN(len(dirs))]
	name := names[rng.IntN(len(names))]
	ppath, _ := m.path(m.nodes, parent)
	full := name
	if ppath != "." {
		full = ppath + "/" + name
	}
	existing, taken := m.child(parent, name)
	switch k := rng.IntN(10); {
	case k < 2 && !taken:
		mkdir(t, v, full)
		id := m.next
		m.next++
		m.nodes[id] = mnode{parent: parent, name: name, dir: true}
		m.ids[id] = nodeOf(t, v, full).ID
		m.record(v, id, false)
	case k < 5 && (!taken || !m.nodes[existing].dir):
		data := fmt.Sprint(rng.IntN(4))
		write(t, v, full, []byte(data))
		id := existing
		if !taken {
			id = m.next
			m.next++
			m.ids[id] = nodeOf(t, v, full).ID
		}
		m.nodes[id] = mnode{parent: parent, name: name, data: data}
		m.record(v, id, false)
	case k < 8:
		var ids []int
		for id := range m.nodes {
			if id != 0 && !m.under(parent, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 || taken {
			return
		}
		slices.Sort(ids)
		id := ids[rng.IntN(len(ids))]
		from, _ := m.path(m.nodes, id)
		move(t, v, from, full)
		n := m.nodes[id]
		n.parent, n.name = parent, name
		m.nodes[id] = n
		m.record(v, id, false)
	default:
		if !taken {
			return
		}
		var gone []int
		for id := range m.nodes {
			if id != 0 && m.under(id, existing) {
				gone = append(gone, id)
			}
		}
		del(t, v, full)
		for _, id := range gone {
			delete(m.nodes, id)
			m.erased[id] = true
		}
		m.record(v, existing, true)
	}
}

// Random changes against the model: every version's snapshot and many pairs' diffs match it.
func TestModelSnapshotsAndDiffs(t *testing.T) {
	steps := 60
	if testing.Short() || raceEnabled {
		steps = 25
	}
	for seed := range uint64(3) {
		rng := rand.New(rand.NewPCG(seed, 7))
		v, e := newVolume(t)
		m := newModel()
		for range steps {
			m.step(t, v, rng)
		}
		if uint64(len(m.versions)-1) != v.Len() {
			t.Fatalf("model has %d versions, volume %d", len(m.versions)-1, v.Len())
		}
		for ver := 1; ver < len(m.versions); ver++ {
			s, err := v.Snapshot(uint64(ver))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := sortWalk(tree(t, s)), m.tree(ver); !slices.Equal(got, want) {
				t.Fatalf("seed %d version %d:\n got %q\nwant %q", seed, ver, got, want)
			}
		}
		n := len(m.versions) - 1
		check := func(a, b int) {
			got, err := v.Diff(uint64(a), uint64(b))
			if err != nil {
				t.Fatal(err)
			}
			if want := m.expectDiff(a, b); !slices.Equal(got, want) {
				t.Fatalf("seed %d diff %d..%d:\n got %+v\nwant %+v", seed, a, b, got, want)
			}
		}
		for a := 1; a <= n; a++ {
			for b := a; b <= min(n, a+3); b++ {
				check(a, b)
			}
			check(a, n)
			check(1, a)
		}
		for range 50 {
			a := 1 + rng.IntN(n)
			check(a, a+rng.IntN(n-a+1))
		}
		// The current snapshot equals the tree, and the writer and a reader agree after a reopen.
		cur, err := v.Snapshot(v.Len())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tree(t, cur), tree(t, v)) {
			t.Fatal("Snapshot(Len) differs from the tree")
		}
		r := newReader(t, e, false)
		if !slices.Equal(tree(t, r.v), tree(t, v)) || r.v.Len() != v.Len() {
			t.Fatal("reader differs")
		}
		if got, _ := r.v.Diff(1, uint64(n)); !slices.Equal(got, m.expectDiff(1, n)) {
			t.Fatal("reader diff differs")
		}
		want := tree(t, v)
		v = e.reopen(t, v)
		if !slices.Equal(tree(t, v), want) {
			t.Fatal("reopen differs")
		}
	}
	v, _ := newVolume(t)
	for _, p := range [][2]uint64{{0, 1}, {2, 1}, {1, 2}} {
		if _, err := v.Diff(p[0], p[1]); err == nil {
			t.Fatalf("Diff(%d, %d) accepted", p[0], p[1])
		}
	}
	for _, ver := range []uint64{0, 2} {
		if _, err := v.Snapshot(ver); err == nil {
			t.Fatalf("Snapshot(%d) accepted", ver)
		}
	}
}
