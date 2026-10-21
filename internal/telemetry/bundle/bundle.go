// Package bundle writes a diagnostic bundle (DESIGN.md section 6.9): a
// gzipped tar a farm admin reviews and attaches to an issue by hand. Its
// first entry is README.txt, in plain words: what the bundle holds and
// what it never holds, the key's epoch, the faithfulness result and the
// bundle's C4 ID. Every other entry is a telemetry record written by the
// one encoder, listed in manifest.c4m with its size and C4 ID; the
// bundle's ID is the manifest's. No entry carries a time: the tar and
// gzip headers are zeroed. If any step fails, nothing is written.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
)

// Bundle is a diagnostic bundle's contents before it is written.
type Bundle struct {
	Snapshots []telemetry.Snapshot
	// Dropped says the ring let older snapshots go to stay in bounds.
	Dropped bool
	// Coarsened says unit durations and work were cut to two figures.
	Coarsened bool
}

// FileName is the bundle's file name: c4-diagnose-<12 of its ID>.tgz, with
// no farm name and no date.
func FileName(id c4.ID) string {
	s := id.String()
	if len(s) > 12 {
		s = s[:12]
	}
	return "c4-diagnose-" + s + ".tgz"
}

type file struct {
	name string
	data []byte
}

// Write checks each snapshot's faithfulness by replaying it through the
// policy it names, and writes the bundle to w, returning its ID. On any
// error w gets nothing.
func Write(w io.Writer, b Bundle) (c4.ID, error) {
	var files []file
	add := func(name string, r telemetry.Record) error {
		out, err := telemetry.Encode(r)
		if err != nil {
			return err
		}
		files = append(files, file{name, out})
		return nil
	}
	red := telemetry.RedactionRecord{MoneyScaled: true, BytesTwoFigures: true, Coarsened: b.Coarsened}
	faithful := 0
	for i := range b.Snapshots {
		s := &b.Snapshots[i]
		if policy := s.Policy.Func(); policy != nil {
			telemetry.CheckFaithful(s, policy)
		} else {
			// A policy this build does not know cannot be replayed.
			s.Faithful = false
			s.Divergences = []telemetry.DivergenceCount{{Kind: telemetry.DivergeOther, Count: 1}}
		}
		if s.Faithful {
			faithful++
		}
		red.KeyEpoch = s.KeyEpoch
		red.Snapshots = append(red.Snapshots, telemetry.SnapshotFaith{Index: telemetry.Int(i), Faithful: s.Faithful, Divergences: s.Divergences})
		if err := add("snapshots/"+pad(i)+".json", *s); err != nil {
			return c4.ID{}, err
		}
	}
	build := telemetry.BuildRecord{Manager: telemetry.CurrentBuild()}
	farm := telemetry.FarmRecord{}
	if n := len(b.Snapshots); n > 0 {
		last := b.Snapshots[n-1]
		for _, p := range last.Pools {
			farm.Pools = append(farm.Pools, p.Name)
			farm.Slots += p.Slots
		}
		for _, p := range last.Projects {
			farm.Projects = append(farm.Projects, p.Name)
		}
		farm.Jobs, farm.Links = telemetry.Int(len(last.Jobs)), telemetry.Int(len(last.Links))
		red.Pseudonyms = []telemetry.KindCount{{Kind: telemetry.KindPool, Count: telemetry.Int(len(last.Pools))},
			{Kind: telemetry.KindProject, Count: telemetry.Int(len(last.Projects))}, {Kind: telemetry.KindJob, Count: telemetry.Int(len(last.Jobs))},
			{Kind: telemetry.KindLink, Count: telemetry.Int(len(last.Links))}}
	}
	if err := add("build.json", build); err != nil {
		return c4.ID{}, err
	}
	if err := add("farm.json", farm); err != nil {
		return c4.ID{}, err
	}
	if err := add("redaction.json", red); err != nil {
		return c4.ID{}, err
	}
	// The event log and the counters are not projected yet; the entries
	// are there, empty, so a reader finds every name the format promises.
	files = append(files, file{"events.jsonl", nil}, file{"counters.jsonl", nil})

	manifest, err := manifestOf(files)
	if err != nil {
		return c4.ID{}, err
	}
	id := c4.Identify(bytes.NewReader(manifest))
	readme := readme(id, len(b.Snapshots), faithful, red.KeyEpoch, b.Dropped)

	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return c4.ID{}, err
	}
	gz.Header = gzip.Header{OS: 255}
	tw := tar.NewWriter(gz)
	all := append([]file{{"README.txt", readme}, {"manifest.c4m", manifest}}, files...)
	for _, f := range all {
		hdr := &tar.Header{Name: f.name, Mode: 0o444, Size: int64(len(f.data)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return c4.ID{}, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return c4.ID{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return c4.ID{}, err
	}
	if err := gz.Close(); err != nil {
		return c4.ID{}, err
	}
	if _, err := w.Write(out.Bytes()); err != nil {
		return c4.ID{}, err
	}
	return id, nil
}

// manifestOf is the c4m manifest of files, with no times: the snapshots
// under a snapshots/ directory, the rest at the top.
func manifestOf(files []file) ([]byte, error) {
	b := c4m.NewBuilder()
	var dir *c4m.DirBuilder
	for _, f := range files {
		opts := []c4m.EntryOption{c4m.WithAttrs(c4.Identify(bytes.NewReader(f.data)), int64(len(f.data)), 0o444, c4m.NullTimestamp())}
		sub, ok := strings.CutPrefix(f.name, "snapshots/")
		if !ok {
			b.AddFile(f.name, opts...)
			continue
		}
		if dir == nil {
			dir = b.AddDir("snapshots/", c4m.WithMode(fs.ModeDir|0o555), c4m.WithSize(-1), c4m.WithTimestamp(c4m.NullTimestamp()))
		}
		dir.AddFile(sub, opts...)
	}
	m, err := b.Build()
	if err != nil {
		return nil, err
	}
	m.SortEntries()
	m.Canonicalize()
	return c4m.Marshal(m)
}

func pad(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// readme is the bundle's README.txt, from fixed words and the bundle's
// numbers only.
func readme(id c4.ID, snaps, faithful int, epoch uint32, dropped bool) []byte {
	var b strings.Builder
	line := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
		}
		b.WriteString("\n")
	}
	line("C4 diagnostic bundle ", id.String())
	line()
	line("What this holds: snapshots of the scheduler's input and decisions,")
	line("the build, and the farm's shape. Every name (shows, shots, jobs, units,")
	line("people, pools, sites, links, classes) is a keyed pseudonym that only")
	line("this farm can map back; every time is relative to its snapshot, with")
	line("no date or zone; every amount of money is scaled by a secret factor")
	line("that is not in this bundle; sizes keep two significant figures.")
	line()
	line("What this never holds: names, paths, file contents, prompts, messages,")
	line("logs, environment, parameters, images, raw C4 IDs of the work, dates,")
	line("or the farm's key. Every entry but this one is listed in manifest.c4m")
	line("with its size and C4 ID, and the bundle's ID is the manifest's.")
	line()
	line("Snapshots: ", strconv.Itoa(snaps), ", of which ", strconv.Itoa(faithful), " replay to the farm's own decisions.")
	if dropped {
		line("Older snapshots were dropped to keep the farm's record in bounds.")
	}
	line("Key epoch: ", strconv.FormatUint(uint64(epoch), 10))
	line()
	line("Replay: hack/sim -bundle <this file> -replay decide")
	return []byte(b.String())
}
