package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
)

// Checkpoint is one turn boundary of a member, as saved.
//
// The transcript is Claude Code's own session file, which it appends to.
// A checkpoint names the file's prefix up to the last complete line by a
// hash chain: each checkpoint's Section is the C4 ID of the bytes added
// since the one before, and Head is the C4 ID of the previous Head followed
// by Section. Computing it reads only the new bytes, so a 100 MB transcript
// costs no more per turn than a small one, and Verify can walk the file to
// prove the chain.
type Checkpoint struct {
	Seq   int       `json:"seq"`
	Epoch int       `json:"epoch"`
	At    time.Time `json:"at"`
	// Event is what caused the checkpoint: stop (Claude Code's Stop hook,
	// the end of a turn), manual, yield, or resume.
	Event string `json:"event"`
	// Session is the session the checkpoint belongs to.
	Session string `json:"session"`
	// Offset is the length of the transcript prefix the checkpoint covers.
	Offset int64 `json:"offset"`
	// Section is the C4 ID of the bytes since the previous checkpoint;
	// empty when nothing was added.
	Section string `json:"section,omitempty"`
	// Head is the chain head after this checkpoint.
	Head string `json:"head"`
	// Tail is the C4 ID of the last tailBytes of the prefix, which the next
	// checkpoint uses to detect a transcript that was rewritten rather than
	// appended to.
	Tail string `json:"tail"`
	// Workspace is the member's working tree at the same point.
	Workspace Workspace `json:"workspace"`
	// Note records anything unusual: a rewritten transcript, a lagging
	// write.
	Note string `json:"note,omitempty"`
	// Background is the background work (agents, commands) still running
	// when the session was stopped here, and so stopped with it: what it was
	// doing and where its work so far is. The attempt that resumes the
	// member is told.
	Background []Background `json:"background,omitempty"`
}

const tailBytes = 4096

func identify(b []byte) string { return c4.Identify(bytes.NewReader(b)).String() }

func chainHead(prev, section string) string {
	return c4.Identify(strings.NewReader(prev + section)).String()
}

// readLog returns a member's checkpoints, oldest first.
func (s *Store) readLog(name string) ([]Checkpoint, error) {
	f, err := os.Open(s.logPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Checkpoint
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var cp Checkpoint
		if err := json.Unmarshal(sc.Bytes(), &cp); err != nil {
			return nil, fmt.Errorf("checkpoint log %s: %w", name, err)
		}
		out = append(out, cp)
	}
	return out, sc.Err()
}

// Checkpoints returns a member's checkpoint log, oldest first.
func (s *Store) Checkpoints(name string) ([]Checkpoint, error) { return s.readLog(name) }

// Last returns a member's latest checkpoint, or nil.
func (s *Store) Last(name string) (*Checkpoint, error) {
	cps, err := s.readLog(name)
	if err != nil || len(cps) == 0 {
		return nil, err
	}
	return &cps[len(cps)-1], nil
}

func (s *Store) appendLog(name string, cp Checkpoint) error {
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.logPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// completePrefix returns the length of the longest prefix of the file that
// ends in a newline: the transcript up to its last complete line.
func completePrefix(f *os.File, size int64) (int64, error) {
	const chunk = 64 * 1024
	end := size
	buf := make([]byte, chunk)
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

func readRange(f *os.File, from, to int64) ([]byte, error) {
	if to <= from {
		return nil, nil
	}
	b := make([]byte, to-from)
	if _, err := f.ReadAt(b, from); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

func tailOf(f *os.File, offset int64) (string, error) {
	from := offset - tailBytes
	if from < 0 {
		from = 0
	}
	b, err := readRange(f, from, offset)
	if err != nil {
		return "", err
	}
	return identify(b), nil
}

// waitStable waits until the transcript's size has not changed for quiet,
// up to max. Claude Code writes the transcript asynchronously, so at the
// Stop hook the last lines may still be arriving.
func waitStable(path string, quiet, max time.Duration) (int64, error) {
	deadline := time.Now().Add(max)
	last := int64(-1)
	since := time.Now()
	for {
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if st.Size() != last {
			last, since = st.Size(), time.Now()
		}
		if time.Since(since) >= quiet || time.Now().After(deadline) {
			return last, nil
		}
		time.Sleep(quiet / 4)
	}
}

// nextCheckpoint computes the checkpoint that follows prev for the
// transcript at path. It reads only the bytes after prev's offset, and the
// tail before it.
func nextCheckpoint(path string, prev *Checkpoint) (Checkpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return Checkpoint{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Checkpoint{}, err
	}
	end, err := completePrefix(f, st.Size())
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	from, head := int64(0), ""
	if prev != nil {
		from, head = prev.Offset, prev.Head
		switch {
		case end < prev.Offset:
			cp.Note = fmt.Sprintf("transcript shrank from %d to %d bytes; chain restarted", prev.Offset, end)
			from, head = 0, ""
		default:
			t, err := tailOf(f, prev.Offset)
			if err != nil {
				return Checkpoint{}, err
			}
			if t != prev.Tail {
				cp.Note = "transcript rewritten before the last checkpoint; chain restarted"
				from, head = 0, ""
			}
		}
	}
	sec, err := readRange(f, from, end)
	if err != nil {
		return Checkpoint{}, err
	}
	if len(sec) > 0 {
		cp.Section = identify(sec)
		head = chainHead(head, cp.Section)
	}
	cp.Offset, cp.Head = end, head
	cp.Tail, err = tailOf(f, end)
	return cp, err
}

// VerifyResult reports a chain check.
type VerifyResult struct {
	Checked int
	// Broken is the sequence number of the first checkpoint the file no
	// longer matches, or 0.
	Broken int
	Reason string
}

// Verify recomputes a member's chain from its transcript and compares it
// with the log. Checkpoints before a restart note are skipped, since the
// transcript they describe no longer exists.
func (s *Store) Verify(name string) (VerifyResult, error) {
	m, err := s.Get(name)
	if err != nil {
		return VerifyResult{}, err
	}
	cps, err := s.readLog(name)
	if err != nil {
		return VerifyResult{}, err
	}
	start := 0
	for i, cp := range cps {
		if strings.Contains(cp.Note, "chain restarted") || (i > 0 && cp.Session != cps[i-1].Session) {
			start = i
		}
	}
	f, err := os.Open(m.Transcript)
	if err != nil {
		return VerifyResult{}, err
	}
	defer f.Close()
	var res VerifyResult
	from, head := int64(0), ""
	for _, cp := range cps[start:] {
		if cp.Session != m.Session {
			continue
		}
		sec, err := readRange(f, from, cp.Offset)
		if err != nil {
			return res, err
		}
		if len(sec) > 0 {
			if got := identify(sec); got != cp.Section {
				return VerifyResult{Checked: res.Checked, Broken: cp.Seq, Reason: "section differs from the transcript"}, nil
			}
			head = chainHead(head, cp.Section)
		}
		if head != cp.Head {
			return VerifyResult{Checked: res.Checked, Broken: cp.Seq, Reason: "chain head differs"}, nil
		}
		from = cp.Offset
		res.Checked++
	}
	return res, nil
}
