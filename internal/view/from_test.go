package view

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
)

// kindAt says what a view holds at a path: nothing, a file or symlink, or a
// directory.
func kindAt(v *View, p string) string {
	p = strings.TrimSuffix(p, "/")
	dir, name := SplitPath(p)
	n := v.Root.Lookup(dir)
	if n == nil {
		return ""
	}
	if n.Files[name] != nil {
		return "file"
	}
	if n.Dirs[name+"/"] != nil {
		return "dir"
	}
	return ""
}

// meant spells out a unit's edits as what its patch can say about them,
// for replaying on another view. An opaque edit (a directory's contents
// replaced) is each thing the directory held in the unit's view prev
// removed, one path at a time. A removal is of the thing the unit saw: a
// path that has become another kind in the view it is replayed on (a file
// where the unit removed a directory, or the other way round) is someone
// else's later write, and stays. Every other edit is its own meaning. It
// also reports how many removals were left out that way.
func meant(t *testing.T, prev, on c4.ID, edits []Edit, src RecordSource) (out []Edit, kept int) {
	t.Helper()
	ctx := context.Background()
	pv, err := Load(ctx, prev, src, 4)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := Load(ctx, on, src, 4)
	if err != nil {
		t.Fatal(err)
	}
	remove := func(p string) {
		if was, is := kindAt(pv, p), kindAt(ov, p); is != "" && was != is {
			kept++
			return
		}
		out = append(out, Edit{Path: p})
	}
	for _, e := range edits {
		switch {
		case e.Opaque:
			if n := pv.Root.Lookup(e.Path); n != nil {
				for name := range n.Files {
					remove(e.Path + name)
				}
				for name := range n.Dirs {
					remove(e.Path + name)
				}
			}
			out = append(out, Edit{Path: e.Path, Entry: e.Entry})
		case e.Entry == nil:
			remove(e.Path)
		default:
			out = append(out, e)
		}
	}
	return out, kept
}

func storeRecords(t *testing.T, ram *store.RAM, a *Applied) {
	t.Helper()
	for _, r := range a.Records {
		putBytes(t, ram, r.ID, r.Data)
	}
}

// mutateKinds is mutate with changes of kind added: a file that becomes a
// directory or a symlink, and a directory that becomes a file or a symlink.
func mutateKinds(t *testing.T, rng *rand.Rand, dir string, step int) []Edit {
	t.Helper()
	k := rng.IntN(10)
	if k >= 4 {
		return mutate(t, rng, dir, step)
	}
	var files, dirs []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, filepath.ToSlash(rel))
		} else {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	sort.Strings(dirs)
	from := files
	if k >= 2 {
		from = dirs
	}
	if len(from) == 0 {
		return mutate(t, rng, dir, step)
	}
	p := from[rng.IntN(len(from))]
	full := filepath.Join(dir, p)
	if err := os.RemoveAll(full); err != nil {
		t.Fatal(err)
	}
	switch k {
	case 0: // a file becomes a directory with a file in it
		content := fmt.Sprintf("inside %d", step)
		if err := os.Mkdir(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "inner.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return []Edit{{Path: p + "/inner.txt", Entry: FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)}}
	case 2: // a directory becomes a file
		content := fmt.Sprintf("was a directory %d", step)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return []Edit{{Path: p, Entry: FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)}}
	default: // a file or a directory becomes a symlink
		target := fmt.Sprintf("elsewhere-%d", step)
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
		return []Edit{{Path: p, Entry: SymlinkEntry("", target)}}
	}
}

// Two units start from one view. The first changes it. The second's
// changes, each a changeset over the second's own previous view, are then
// applied to the first's result. That must give what replaying the meaning
// of the second unit's edits there gives: an oracle that reads no patch at
// all.
//
// Both units draw their new file names and contents from the same step
// numbers, so they often write the same bytes to the same path. A patch
// applied to the first's result as it stands reads that as a removal. Both
// also change the kind of what a path holds, so the second's patch meets
// files where it saw directories and directories where it saw files.
func TestApplyPatchFromIsTheEditsReplayed(t *testing.T) {
	ctx := context.Background()
	changed, sameWrites, droppedSome, removalsKept := 0, 0, 0, 0
	for seed := range 400 {
		dirA, dirB := t.TempDir(), t.TempDir()
		writeTree(t, dirA, show)
		writeTree(t, dirB, show)
		base, err := ScanDir(dirA)
		if err != nil {
			t.Fatal(err)
		}
		ram := store.NewRAM()
		src := StoreRecords(ram)
		publish(t, ram, base)
		rngA := rand.New(rand.NewPCG(uint64(seed), 11))
		rngB := rand.New(rand.NewPCG(uint64(seed), 13))
		for step := range 1 + rngA.IntN(6) {
			mutateKinds(t, rngA, dirA, step)
		}
		first, err := ScanDir(dirA)
		if err != nil {
			t.Fatal(err)
		}
		publish(t, ram, first)

		target, oracle, prev := first.ID, first.ID, base.ID
		for step := range 1 + rngB.IntN(6) {
			edits := mutateKinds(t, rngB, dirB, step)
			own, err := Apply(ctx, prev, edits, src)
			if err != nil {
				t.Fatal(err)
			}
			if scan, _ := ScanDir(dirB); scan.ID != own.ID {
				t.Fatalf("seed %d step %d: the edits do not describe the disk", seed, step)
			}
			if own.ID == prev {
				continue
			}
			storeRecords(t, ram, own)
			ch, err := ReadChain(own.Changeset)
			if err != nil {
				t.Fatal(err)
			}
			patch := ch.Sections[0].Patch

			replay, kept := meant(t, prev, oracle, edits, src)
			removalsKept += kept
			want, err := Apply(ctx, oracle, replay, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, want)
			got, err := ApplyPatchFrom(ctx, target, prev, patch, src)
			if err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			storeRecords(t, ram, got)
			if got.ID != want.ID {
				t.Fatalf("seed %d step %d: the patch applied from its base gives %s, the edits replayed give %s\npatch:\n%s", seed, step, got.ID, want.ID, own.Changeset)
			}
			// Applied as it stands, the same patch would have given
			// something else whenever it restates what the target holds.
			if plain, err := ApplyPatch(ctx, target, patch, src); err == nil && plain.ID != got.ID {
				sameWrites++
			}
			// What it reports as dropped was in the target as that kind,
			// and is not in the result as that kind.
			if len(got.Dropped) > 0 {
				droppedSome++
				before, err := Load(ctx, target, src, 4)
				if err != nil {
					t.Fatal(err)
				}
				after, err := Load(ctx, got.ID, src, 4)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range got.Dropped {
					kind := "file"
					if strings.HasSuffix(d, "/") {
						kind = "dir"
					}
					if kindAt(before, d) != kind {
						t.Fatalf("seed %d step %d: %s is reported dropped, and the target held %q there", seed, step, d, kindAt(before, d))
					}
					// A directory that was removed and written again by
					// the same patch is there again, as another directory.
					if kind == "file" && kindAt(after, d) == "file" {
						t.Fatalf("seed %d step %d: %s is reported dropped and is still a file", seed, step, d)
					}
				}
			}
			if got.ID == target {
				if len(got.Changeset) != 0 || len(got.Records) != 0 {
					t.Fatalf("seed %d step %d: nothing changed, and there is a changeset or records", seed, step)
				}
			} else {
				changed++
				// Its changeset is an ordinary one over the target: applied
				// there by the plain rule, it gives the same view.
				gch, err := ReadChain(got.Changeset)
				if err != nil || gch.Base != target || len(gch.Sections) != 1 {
					t.Fatalf("seed %d step %d: the changeset over the target: %v", seed, step, err)
				}
				back, err := ApplyPatch(ctx, target, gch.Sections[0].Patch, src)
				if err != nil || back.ID != got.ID {
					t.Fatalf("seed %d step %d: the changeset over the target applies to %v, want %s (%v)\n%s", seed, step, back, got.ID, err, got.Changeset)
				}
			}
			// Idempotent: the same patch, read against the same base,
			// changes nothing the second time.
			again, err := ApplyPatchFrom(ctx, got.ID, prev, patch, src)
			if err != nil || again.ID != got.ID || len(again.Changeset) != 0 {
				t.Fatalf("seed %d step %d: applied a second time it gives %v (%v)", seed, step, again, err)
			}
			target, oracle, prev = got.ID, want.ID, own.ID
		}
	}
	t.Logf("%d changes; %d where the plain rule would differ; %d that dropped something unnamed; %d removals that met another kind and left it", changed, sameWrites, droppedSome, removalsKept)
	if changed < 400 || sameWrites < 25 || droppedSome < 25 || removalsKept < 5 {
		t.Fatal("the cases didn't cover enough")
	}
}

// Over its own base, ApplyPatchFrom is ApplyPatch, and its changeset is the
// one Apply made.
func TestApplyPatchFromItsOwnBaseIsApplyPatch(t *testing.T) {
	ctx := context.Background()
	chain, views, ram := buildChain(t, 21, 10)
	ch, err := ReadChain(chain)
	if err != nil {
		t.Fatal(err)
	}
	src := StoreRecords(ram)
	for i, sec := range ch.Sections {
		got, err := ApplyPatchFrom(ctx, sec.Base, sec.Base, sec.Patch, src)
		if err != nil || got.ID != views[i+1] {
			t.Fatalf("section %d: %v", i+1, err)
		}
		want, err := EncodeChangeset(sec.Base, sec.Patch)
		if err != nil || string(got.Changeset) != string(want) {
			t.Fatalf("section %d: the changeset over its own base is not the changeset it came from\n%s\n---\n%s", i+1, got.Changeset, want)
		}
	}
}

func fileEdit(path, content string) Edit {
	return Edit{Path: path, Entry: FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)}
}

// filesOf maps each file of a view to its content's ID.
func filesOf(t *testing.T, id c4.ID, src RecordSource) (map[string]c4.ID, *View) {
	t.Helper()
	v, err := Load(context.Background(), id, src, 4)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]c4.ID{}
	for _, f := range v.Files {
		have[f.Path] = f.ID
	}
	return have, v
}

// The cases by name, each a unit's changeset over the view both units
// started from (keep.txt and dir/in.txt), applied to what the other unit
// left.
func TestApplyPatchFromCases(t *testing.T) {
	ctx := context.Background()
	put := fileEdit
	del := func(path string) Edit { return Edit{Path: path} }
	mkdir := func(path string) Edit { return Edit{Path: path, Entry: &c4m.Entry{Name: lastName(path)}} }
	for _, c := range []struct {
		name          string
		first, second []Edit
		want          map[string]string // path -> content
		gone          []string
		dirs          []string // directories the result holds
		dropped       []string
	}{
		{name: "both write the same bytes to a new path",
			first: []Edit{put("out/shared.txt", "same")}, second: []Edit{put("out/shared.txt", "same")},
			want: map[string]string{"out/shared.txt": "same", "keep.txt": "keep"}},
		{name: "both write different bytes: the later write is kept",
			first: []Edit{put("out/a.txt", "first")}, second: []Edit{put("out/a.txt", "second")},
			want: map[string]string{"out/a.txt": "second"}},
		{name: "both change an existing file the same way",
			first: []Edit{put("keep.txt", "new")}, second: []Edit{put("keep.txt", "new")},
			want: map[string]string{"keep.txt": "new"}},
		{name: "the second removes what the first changed",
			first: []Edit{put("keep.txt", "changed")}, second: []Edit{del("keep.txt")},
			gone: []string{"keep.txt"}},
		{name: "the second changes what the first removed",
			first: []Edit{del("keep.txt")}, second: []Edit{put("keep.txt", "back")},
			want: map[string]string{"keep.txt": "back"}},
		{name: "both remove the same file",
			first: []Edit{del("keep.txt")}, second: []Edit{del("keep.txt")},
			gone: []string{"keep.txt"}, want: map[string]string{"dir/in.txt": "in"}},
		{name: "the second removes a directory the first added to",
			first: []Edit{put("dir/more.txt", "more")}, second: []Edit{del("dir/")},
			gone: []string{"dir/in.txt", "dir/more.txt"}, want: map[string]string{"keep.txt": "keep"}, dropped: []string{"dir/"}},
		{name: "the second adds to a directory the first removed",
			first: []Edit{del("dir/")}, second: []Edit{put("dir/new.txt", "new")},
			want: map[string]string{"dir/new.txt": "new"}, gone: []string{"dir/in.txt"}},
		{name: "the second only removes under a directory the first removed: it does not come back",
			first: []Edit{del("dir/")}, second: []Edit{put("dir/extra.txt", "x"), del("dir/in.txt")},
			want: map[string]string{"dir/extra.txt": "x"}, gone: []string{"dir/in.txt"}},
		{name: "each adds its own file to the same new directory",
			first: []Edit{put("out/a.txt", "a")}, second: []Edit{put("out/b.txt", "b")},
			want: map[string]string{"out/a.txt": "a", "out/b.txt": "b", "keep.txt": "keep"}},
		{name: "the second writes a file where the first made a directory",
			first: []Edit{put("x/y.txt", "y")}, second: []Edit{put("x", "a file")},
			want: map[string]string{"x": "a file"}, gone: []string{"x/y.txt"}, dropped: []string{"x/"}},
		{name: "the second writes under a name the first made a file",
			first: []Edit{put("x", "a file")}, second: []Edit{put("x/y.txt", "y")},
			want: map[string]string{"x/y.txt": "y"}, gone: []string{"x"}, dropped: []string{"x"}},
		{name: "the second makes an empty directory the first has filled: what the first wrote stays",
			first: []Edit{put("out/a.txt", "a")}, second: []Edit{mkdir("out/")},
			want: map[string]string{"out/a.txt": "a"}},
		{name: "the second makes an empty directory where the first wrote a file",
			first: []Edit{put("out", "a file")}, second: []Edit{mkdir("out/")},
			gone: []string{"out"}, dirs: []string{"out/"}, dropped: []string{"out"}},
		{name: "the second removes a file the first replaced with a directory: the directory stays",
			first: []Edit{put("keep.txt/in.txt", "inside")}, second: []Edit{del("keep.txt")},
			want: map[string]string{"keep.txt/in.txt": "inside"}},
		{name: "the second removes a directory the first replaced with a file: the file stays",
			first: []Edit{put("dir", "a file")}, second: []Edit{del("dir/")},
			want: map[string]string{"dir": "a file"}},
		{name: "the second only removes under a directory the first replaced with a file: the file stays",
			first: []Edit{put("dir", "a file")}, second: []Edit{del("dir/in.txt")},
			want: map[string]string{"dir": "a file"}},
		{name: "the second turns a directory into a file, the first had added to it",
			first: []Edit{put("dir/more.txt", "more")}, second: []Edit{put("dir", "now a file")},
			want: map[string]string{"dir": "now a file"}, gone: []string{"dir/in.txt", "dir/more.txt"}, dropped: []string{"dir/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ram := store.NewRAM()
			src := StoreRecords(ram)
			putBytes(t, ram, EmptyID(), nil)
			base, err := Apply(ctx, EmptyID(), []Edit{put("keep.txt", "keep"), put("dir/in.txt", "in")}, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, base)
			first, err := Apply(ctx, base.ID, c.first, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, first)
			second, err := Apply(ctx, base.ID, c.second, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, second)
			ch, err := ReadChain(second.Changeset)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ApplyPatchFrom(ctx, first.ID, base.ID, ch.Sections[0].Patch, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, got)
			have, v := filesOf(t, got.ID, src)
			for p, content := range c.want {
				if id, ok := have[p]; !ok || id != c4.Identify(strings.NewReader(content)) {
					t.Errorf("%s: want %q there; the view holds %v", p, content, keys(have))
				}
			}
			for _, p := range c.gone {
				if _, ok := have[p]; ok {
					t.Errorf("%s is still there; the view holds %v", p, keys(have))
				}
			}
			for _, d := range c.dirs {
				if v.Root.Lookup(d) == nil {
					t.Errorf("%s is not a directory of the result; the view holds %v", d, keys(have))
				}
			}
			if fmt.Sprint(got.Dropped) != fmt.Sprint(c.dropped) {
				t.Errorf("dropped %v, want %v", got.Dropped, c.dropped)
			}
		})
	}
}

// A directory entry with nothing under it, as a patch from elsewhere may
// write one (capture follows a changed directory with its changes): by its
// C4 ID alone, and with no C4 ID at all.
func TestApplyPatchFromADirectoryWithNoChildrenListed(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	putBytes(t, ram, EmptyID(), nil)
	apply := func(on c4.ID, edits ...Edit) *Applied {
		t.Helper()
		a, err := Apply(ctx, on, edits, src)
		if err != nil {
			t.Fatal(err)
		}
		storeRecords(t, ram, a)
		return a
	}
	base := apply(EmptyID(), fileEdit("keep.txt", "keep"))
	// The patch: d/ holding b.txt, written over base, with the line for
	// b.txt left off, so that d/ is named by its ID alone.
	whole := apply(base.ID, fileEdit("d/b.txt", "b"))
	ch, err := ReadChain(whole.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	byID := c4m.NewManifest()
	for _, e := range ch.Sections[0].Patch.Entries {
		if e.IsDir() {
			byID.AddEntry(e)
		}
	}
	if len(byID.Entries) != 1 || byID.Entries[0].C4ID.IsNil() {
		t.Fatalf("the patch is not one directory by its ID: %v", byID.Entries)
	}
	noID := c4m.NewManifest()
	bare := *byID.Entries[0]
	bare.C4ID = c4.ID{}
	bare.Size = -1
	noID.AddEntry(&bare)

	other := apply(base.ID, fileEdit("d/a.txt", "a"))
	isFile := apply(base.ID, fileEdit("d", "a file"))

	for _, c := range []struct {
		name    string
		target  c4.ID
		patch   *c4m.Manifest
		want    []string
		dropped []string
	}{
		{name: "by ID, where the target has no such directory", target: base.ID, patch: byID, want: []string{"d/b.txt", "keep.txt"}},
		{name: "by ID, where the target holds another directory: it is replaced", target: other.ID, patch: byID, want: []string{"d/b.txt", "keep.txt"}, dropped: []string{"d/"}},
		{name: "by ID, where the target holds that directory already", target: whole.ID, patch: byID, want: []string{"d/b.txt", "keep.txt"}},
		{name: "by ID, where the target holds a file of the name", target: isFile.ID, patch: byID, want: []string{"d/b.txt", "keep.txt"}, dropped: []string{"d"}},
		{name: "with no ID, where the target has no such directory: an empty one", target: base.ID, patch: noID, want: []string{"keep.txt"}},
		{name: "with no ID, where the target holds a directory: it stays", target: other.ID, patch: noID, want: []string{"d/a.txt", "keep.txt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ApplyPatchFrom(ctx, c.target, base.ID, c.patch, src)
			if err != nil {
				t.Fatal(err)
			}
			storeRecords(t, ram, got)
			have, v := filesOf(t, got.ID, src)
			names := keys(have)
			if fmt.Sprint(names) != fmt.Sprint(c.want) {
				t.Errorf("the result holds %v, want %v", names, c.want)
			}
			if v.Root.Lookup("d/") == nil {
				t.Error("d/ is not a directory of the result")
			}
			if fmt.Sprint(got.Dropped) != fmt.Sprint(c.dropped) {
				t.Errorf("dropped %v, want %v", got.Dropped, c.dropped)
			}
			if got.ID == c.target {
				return
			}
			gch, err := ReadChain(got.Changeset)
			if err != nil {
				t.Fatal(err)
			}
			back, err := ApplyPatch(ctx, c.target, gch.Sections[0].Patch, src)
			if err != nil || back.ID != got.ID {
				t.Errorf("its changeset over the target does not give the result: %v\n%s", err, got.Changeset)
			}
		})
	}

	// The same entry with no ID, applied to its own base by the plain
	// rule: the directory is in the result under its name.
	plain, err := ApplyPatch(ctx, base.ID, noID, src)
	if err != nil {
		t.Fatal(err)
	}
	storeRecords(t, ram, plain)
	if _, v := filesOf(t, plain.ID, src); v.Root.Lookup("d/") == nil {
		t.Error("the plain rule lost a directory written with no ID")
	}
}

func keys(m map[string]c4.ID) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
