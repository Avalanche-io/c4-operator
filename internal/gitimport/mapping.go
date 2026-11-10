package gitimport

import (
	"bufio"
	"bytes"
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
//
// A line is appended in one write and ends with a newline. Text at the end
// of the file with no newline after it is an append a crash cut short, or
// one a reader caught half written: if it is not a whole line it is left
// out, and that commit is as if not imported yet. A cut line cannot be
// taken for a whole one, since a JSON object that lacks its end does not
// parse. A line that does end with a newline and does not parse is damage,
// and an error.
func DecodeMapping(r io.Reader) ([]Commit, error) {
	out, _, err := decodeMapping(r)
	return out, err
}

// decodeMapping also returns how many bytes of r are whole lines.
func decodeMapping(r io.Reader) (out []Commit, whole int64, err error) {
	br := bufio.NewReaderSize(r, 1<<16)
	for n := 1; ; n++ {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, 0, rerr
		}
		ended := rerr == nil
		if text := bytes.TrimSpace(line); len(text) > 0 {
			var c Commit
			switch err := json.Unmarshal(text, &c); {
			case err == nil:
				out = append(out, c)
			case ended:
				return nil, 0, fmt.Errorf("mapping line %d does not read (%w). It and the lines after it can be made again from the repository: "+reimport, n, err, n)
			default:
				return out, whole, nil // a cut append: not a line yet
			}
		}
		whole += int64(len(line))
		if !ended {
			return out, whole, nil
		}
	}
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
	if err := mendTail(p, f); err != nil {
		f.Close()
		return nil, err
	}
	return &MappingWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// mendTail leaves the mapping ending at the end of a line before anything
// is appended to it, under the writer's lock: a cut append is taken off,
// and a whole last line that lost only its newline is given one. Appended
// to as it was, the cut text and the next line would make one line that
// does not parse, in the middle of the file, for good.
func mendTail(p string, f *os.File) error {
	r, err := os.Open(p)
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := r.Stat()
	if err != nil {
		return err
	}
	_, whole, err := decodeMapping(r)
	if err != nil {
		return err
	}
	if whole < st.Size() {
		return f.Truncate(whole)
	}
	if whole == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, whole-1); err != nil {
		return err
	}
	if last[0] != '\n' {
		_, err = f.Write([]byte("\n"))
	}
	return err
}

// Write appends one commit.
func (w *MappingWriter) Write(c Commit) error { return w.enc.Encode(c) }

// Close closes the file.
func (w *MappingWriter) Close() error { return w.f.Close() }
