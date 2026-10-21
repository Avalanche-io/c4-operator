package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
)

// Contents is a bundle read back.
type Contents struct {
	ID        c4.ID
	README    string
	Snapshots []telemetry.Snapshot
	Farm      telemetry.FarmRecord
	Redaction telemetry.RedactionRecord
	Build     telemetry.BuildRecord
}

// Read reads a bundle Write wrote, checking every entry against the
// manifest and the manifest against the bundle's ID, so a replay reads the
// bundle that was reviewed.
func Read(r io.Reader) (Contents, error) {
	var out Contents
	gz, err := gzip.NewReader(r)
	if err != nil {
		return out, err
	}
	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<30))
		if err != nil {
			return out, err
		}
		files[h.Name] = data
	}
	manifest, ok := files["manifest.c4m"]
	if !ok {
		return out, fmt.Errorf("bundle: no manifest.c4m")
	}
	out.ID = c4.Identify(bytes.NewReader(manifest))
	m, err := c4m.Unmarshal(manifest)
	if err != nil {
		return out, err
	}
	listed := map[string]bool{"README.txt": true, "manifest.c4m": true}
	for _, e := range m.Entries {
		if e.IsDir() {
			continue
		}
		name := m.EntryPath(e)
		data, ok := files[name]
		if !ok {
			return out, fmt.Errorf("bundle: %s is in the manifest but not the bundle", name)
		}
		if got := c4.Identify(bytes.NewReader(data)); got != e.C4ID || int64(len(data)) != e.Size {
			return out, fmt.Errorf("bundle: %s does not match the manifest", name)
		}
		listed[name] = true
	}
	for name := range files {
		if !listed[name] {
			return out, fmt.Errorf("bundle: %s is in the bundle but not the manifest", name)
		}
	}
	out.README = string(files["README.txt"])
	if !strings.Contains(out.README, out.ID.String()) {
		return out, fmt.Errorf("bundle: README.txt names another bundle")
	}
	var names []string
	for n := range files {
		if strings.HasPrefix(n, "snapshots/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		var s telemetry.Snapshot
		if err := telemetry.Decode(files[n], &s); err != nil {
			return out, fmt.Errorf("bundle: %s: %v", n, err)
		}
		out.Snapshots = append(out.Snapshots, s)
	}
	for name, into := range map[string]telemetry.Record{"farm.json": &out.Farm, "redaction.json": &out.Redaction, "build.json": &out.Build} {
		if err := telemetry.Decode(files[name], into); err != nil {
			return out, fmt.Errorf("bundle: %s: %v", name, err)
		}
	}
	return out, nil
}
