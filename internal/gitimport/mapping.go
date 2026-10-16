package gitimport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Avalanche-io/c4"
)

// The mapping file records one JSON object per imported commit, oldest
// first: the Commit fields, so the CI class can go from a commit SHA to its
// view and changeset. A later import appends to it and continues from the
// newest commit it lists.

// ReadMapping reads a mapping file. A missing file is an empty mapping.
func ReadMapping(p string) ([]Commit, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeMapping(f)
}

// DecodeMapping reads mapping lines.
func DecodeMapping(r io.Reader) ([]Commit, error) {
	var out []Commit
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 64<<20)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c Commit
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("mapping line %d: %w", n, err)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// KnownViews returns commit to view ID for a mapping.
func KnownViews(m []Commit) map[string]c4.ID {
	out := make(map[string]c4.ID, len(m))
	for _, c := range m {
		if c.Commit != "" {
			out[c.Commit] = c.View
		}
	}
	return out
}

// KnownFiles returns commit to file count for a mapping.
func KnownFiles(m []Commit) map[string]int {
	out := make(map[string]int, len(m))
	for _, c := range m {
		if c.Commit != "" && !c.RefOnly {
			out[c.Commit] = c.Files
		}
	}
	return out
}

// LastByRef returns, for each ref the mapping labels, the newest commit
// recorded for it: where an import of that ref can continue. A deleted
// ref maps to "".
func LastByRef(m []Commit) map[string]string {
	out := map[string]string{}
	for _, c := range m {
		if c.Ref != "" {
			out[c.Ref] = c.Commit
		}
	}
	return out
}

// MappingWriter appends mapping lines to a file, one per commit, flushed
// as each commit finishes, so an interrupted import keeps what it did.
type MappingWriter struct {
	f   *os.File
	enc *json.Encoder
}

// AppendMapping opens p for appending, creating it if needed. It holds an
// exclusive lock on the file until Close, so concurrent imports (two
// post-receive hooks, or a hook and a listener) take turns; open the
// writer before reading the mapping to see what the previous holder wrote.
func AppendMapping(p string) (*MappingWriter, error) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	return &MappingWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// Write appends one commit.
func (w *MappingWriter) Write(c Commit) error { return w.enc.Encode(c) }

// Close closes the file.
func (w *MappingWriter) Close() error { return w.f.Close() }
