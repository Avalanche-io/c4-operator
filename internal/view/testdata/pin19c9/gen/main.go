// Generates the pin move's fixture: views, their records and changesets,
// written by the operator's view package at the c4 pin before the move
// (19c97485), so a test at the new pin holds what an old store holds. Run
// from a worktree at the old pin: go run ./_pingen <out dir>. Every object
// is made by the library; nothing is written by hand.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

type item struct {
	path string
	kind string // file, null (a null-sized file: a sequence frame's shape), link, exec
	data string
}

// mem is a store of what the generator writes.
type mem map[c4.ID][]byte

func (m mem) put(data []byte) c4.ID {
	id := c4.Identify(bytes.NewReader(data))
	m[id] = append([]byte(nil), data...)
	return id
}

func (m mem) Record(_ context.Context, id c4.ID) ([]byte, error) {
	d, ok := m[id]
	if !ok {
		return nil, fmt.Errorf("%s not in store", id)
	}
	return d, nil
}

func main() {
	out := os.Args[1]
	objs := filepath.Join(out, "objects")
	if err := os.MkdirAll(objs, 0o755); err != nil {
		panic(err)
	}
	ctx := context.Background()
	st := mem{}
	st.put(nil)

	corpus := map[string][]item{
		"plain":       {{"a.txt", "file", "a"}, {"d/b.txt", "file", "b"}, {"d/e/c.txt", "file", "c"}},
		"link-depth1": {{"a/t.txt", "file", "t"}, {"a/l", "link", "t.txt"}},
		"link-depth2": {{"a/d/t.txt", "file", "t"}, {"a/d/l", "link", "t.txt"}},
		"null-depth2": {{"s/r/f.0001.exr", "null", "frame1"}, {"s/r/f.0002.exr", "null", "frame2"}},
		"mixed-deep":  {{"p/q/r/l", "link", "../../a"}, {"p/q/f.0001.exr", "null", "fr"}, {"p/a", "file", "a"}, {"top", "file", "t"}},
	}
	names := make([]string, 0, len(corpus))
	for n := range corpus {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	views := map[string]c4.ID{}
	for _, n := range names {
		id, cs := build(ctx, st, c4view.EmptyID(), corpus[n])
		views[n] = id
		lines = append(lines, fmt.Sprintf("view %s %s", n, id), fmt.Sprintf("changeset %s %s %s", n, cs, c4view.EmptyID()))
	}
	// A chain: link-depth2's tree, then a frame two directories down, then
	// a file at the top, each a changeset over the view before it.
	prev := views["link-depth2"]
	for i, step := range [][]item{
		{{"s/r/f.0003.exr", "null", "frame3"}},
		{{"z.txt", "file", "z"}},
	} {
		id, cs := build(ctx, st, prev, step)
		name := fmt.Sprintf("chain%d", i+1)
		lines = append(lines, fmt.Sprintf("view %s %s", name, id), fmt.Sprintf("changeset %s %s %s", name, cs, prev))
		prev = id
	}
	for id, data := range st {
		if err := os.WriteFile(filepath.Join(objs, id.String()), data, 0o444); err != nil {
			panic(err)
		}
	}
	sort.Strings(lines)
	head := "# made at c4 " + libVersion() + " by the pin move's generator; the names are views (name, C4 ID) and changesets (name, C4 ID, parent view)\n"
	if err := os.WriteFile(filepath.Join(out, "index.txt"), []byte(head+strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		panic(err)
	}
}

func libVersion() string {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		panic(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "github.com/Avalanche-io/c4" {
			return f[1]
		}
	}
	panic("no c4 in go.mod")
}

func build(ctx context.Context, st mem, parent c4.ID, items []item) (c4.ID, c4.ID) {
	var edits []c4view.Edit
	for _, it := range items {
		name := it.path[strings.LastIndex(it.path, "/")+1:]
		var e *c4m.Entry
		switch it.kind {
		case "link":
			e = c4view.SymlinkEntry(name, it.data)
		default:
			id := st.put([]byte(it.data))
			size := int64(len(it.data))
			if it.kind == "null" {
				size = -1
			}
			e = c4view.FileEntry(name, size, id, it.kind == "exec")
		}
		edits = append(edits, c4view.Edit{Path: it.path, Entry: e})
	}
	c4view.SortEdits(edits)
	a, err := c4view.Apply(ctx, parent, edits, st)
	if err != nil {
		panic(err)
	}
	for _, r := range a.Records {
		st.put(r.Data)
	}
	return a.ID, st.put(a.Changeset)
}
