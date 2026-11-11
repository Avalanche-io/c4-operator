package capture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// pinnedTree writes the fixed tree whose IDs TestIDsArePinned pins: an
// executable, a nested directory, two empty directories, an empty file,
// two files with the same content, and two symlinks.
func pinnedTree(t *testing.T, dir string) {
	t.Helper()
	for p, c := range map[string]string{
		"README.md":           "readme\n",
		"bin/run.sh":          "#!/bin/sh\necho run\n",
		"a/b/c/deep.txt":      "deep\n",
		"a/b/sibling.txt":     "sibling\n",
		"data/same1.bin":      "same\n",
		"data/same2.bin":      "same\n",
		"zero/empty-file.txt": "",
	} {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		mode := os.FileMode(0o644)
		if filepath.Ext(p) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	os.MkdirAll(filepath.Join(dir, "a/empty-nested"), 0o755)
	os.Symlink("bin/run.sh", filepath.Join(dir, "run"))
	os.Symlink("../README.md", filepath.Join(dir, "a/readme-link"))
}

// The IDs this module computes for a fixed tree, its layer and two
// captures in a chain are pinned: a view's ID must reproduce on any
// machine and with any version of the c4 library, so an upgrade that
// changes one fails here instead of silently splitting every cache and
// layer by version.
//
// The two changeset IDs changed on purpose on 2026-11-09, and nothing else
// did. Until then a changeset ended with the bare ID of the view it
// produces; a c4m file never ends with a bare C4 ID, so that line is gone.
// before0 and before1 are the IDs pinned until then, kept to prove the
// line is the whole difference: a changeset with its output's ID put back
// after it is, byte for byte, the changeset that was stored before.
func TestIDsArePinned(t *testing.T) {
	const (
		baseView   = "c44Ks7KVUA4sB6ztN22vmPRccF9r22hoezTt8wVkDrghVFJFKxjzpDHwRVG4D8o6qQjhyKk12HaoYifireeWpYz6ok"
		baseLayer  = "f4Ks7KVUA4sB6ztN22vmPRccF9r22hoezTt8wVkDr"
		out0       = "c43oADB1QZ3Pbr144Z1KgqrEAXveUZRmMbNnXgDxPXRddULz9NN8k2rSue5dZtg1xHWcjrT8EKJB4TFmyyNdpwGDBg"
		changeset0 = "c43RnMseaR6uSZsts7SdiaqARqN2h4KvpYLvHUpyakYvUkM4KZ5uYZ5c3Zc2KqRbZBEvtZ2B5BN4QJuNVEHmpfQcMa"
		before0    = "c429NdXmipqgQjXvStenbzKKUEHMSAcMzByxt3QqUeFjtMYw7GNBF9BYcV8qGHX1FcxxT9orotwmVmFVvgLbeDDN8h"
		out1       = "c41n5DrBBCB5KmWqKqLZNDGPMknzdmF3vnCWsXNETwAyrGHC6mrNsNGz793Esi9bppcPQSFW1iSxb1PUX5jF8ybWfm"
		changeset1 = "c456JAMjrwWbrdRBZMqQfXXihcJoS4jAobsTnt1m6DwCAyXsxrWF7nYY7EtZwnD8QgX4wiRdLMeaM4zV7X829GoA6Y"
		before1    = "c41hYFqy7XQ2mZqtr4c5FZ1sRcM6VprzVSaAkzvpn3aKCpFFFoVQo2o9WZSSYrViD93r1Sv1sisZHMkMHXJYofx687"
	)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	pinnedTree(t, src)
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID.String() != baseView {
		t.Fatalf("view %s, pinned %s", v.ID, baseView)
	}
	if k := layer.FullKey(v.ID); k != baseLayer {
		t.Fatalf("layer key %s, pinned %s", k, baseLayer)
	}
	set, _ := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Budget: 1 << 30})
	cache, _ := set.Partition("p-x")
	sink := &storeSink{objs: map[c4.ID][]byte{}}
	for _, f := range v.Files {
		b, _ := os.ReadFile(filepath.Join(src, f.Path))
		cache.Put(f.ID, b)
	}
	recs, rerr := v.Records()
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, r := range recs {
		cache.Put(r.ID, r.Data)
		sink.PutBytes(context.Background(), "", r.ID, r.Data)
	}

	parent := v.ID
	for step, c := range []struct {
		files                     map[string]string
		gone                      []string
		output, changeset, before string
	}{
		{map[string]string{"a/b/c/deep.txt": "deeper\n", "new/added.txt": "added\n"}, []string{"data/same2.bin", "empty"}, out0, changeset0, before0},
		{map[string]string{"README.md": "readme v3\n", "bin/tool.sh": "#!/bin/sh\n"}, nil, out1, changeset1, before1},
	} {
		upper := filepath.Join(root, "upper", string(rune('0'+step)))
		var entries []overlay.Entry
		for p, content := range c.files {
			full := filepath.Join(upper, p)
			os.MkdirAll(filepath.Dir(full), 0o755)
			mode := os.FileMode(0o644)
			if filepath.Ext(p) == ".sh" {
				mode = 0o755
			}
			os.WriteFile(full, []byte(content), mode)
			entries = append(entries, overlay.Entry{Path: p, Full: full, Kind: overlay.File, Size: int64(len(content)), Exec: mode&0o111 != 0})
		}
		for _, p := range c.gone {
			entries = append(entries, overlay.Entry{Path: p, Kind: overlay.Whiteout})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		res, err := Run(context.Background(), Input{
			Project: "x", Parent: parent, Upper: upper, Cache: cache, Sink: sink, Workers: 1,
			Walk:    func(string) ([]overlay.Entry, error) { return entries, nil },
			Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Output.String() != c.output || res.Changeset.String() != c.changeset {
			t.Fatalf("capture %d: output %s changeset %s, pinned %s %s", step, res.Output, res.Changeset, c.output, c.changeset)
		}
		stored := sink.objs[res.Changeset]
		if got := c4.Identify(bytes.NewReader(stored)); got != res.Changeset {
			t.Fatalf("capture %d: the stored changeset hashes to %s", step, got)
		}
		// The changeset starts with its parent and ends on an entry; with
		// its output's ID after it, it is the changeset stored before.
		if !bytes.HasPrefix(stored, []byte(parent.String()+"\n")) || bytes.HasSuffix(stored, []byte(res.Output.String()+"\n")) {
			t.Fatalf("capture %d: the changeset does not start with its parent, or ends with its output\n%s", step, stored)
		}
		old := append(append([]byte(nil), stored...), res.Output.String()+"\n"...)
		if got := c4.Identify(bytes.NewReader(old)).String(); got != c.before {
			t.Fatalf("capture %d: with its output's ID after it the changeset hashes to %s; before 2026-11-09 it was %s", step, got, c.before)
		}
		parent = res.Output
	}
}
