package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// replayTool is `c4-unit run --replay FILE`: a captured log read through
// the tool's protocol and patterns as the runner reads a live one, running
// nothing, and what the runner would have reported: the progress it saw
// and where, the last status, the frames finished, and the failure class
// and reason, with the line each came from. A TD checks a tool's patterns
// against a real night's log this way (c4 farm tool test --log).
func replayTool(e env, path string, stdout io.Writer) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return setupCode, err
	}
	defer f.Close()
	tree, err := os.MkdirTemp("", "c4-replay-")
	if err != nil {
		return setupCode, err
	}
	defer os.RemoveAll(tree)
	if err := os.MkdirAll(filepath.Join(tree, ".c4"), 0o755); err != nil {
		return setupCode, err
	}
	r := &runner{e: e, tree: tree, start: e.clock(), done: map[int]bool{}, previewed: map[int]bool{}}
	if _, err := r.prepare(); err != nil {
		return setupCode, err
	}
	r.previewQ = make(chan previewJob, 1<<16) // counted, never made

	var (
		steps              []string
		lastStep, n        int
		status             string
		statusLine, reason int
	)
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		l, err := br.ReadString('\n')
		if l != "" {
			n++
			l = strings.TrimRight(l, "\r\n")
			before, class, pat := r.written, r.class, r.patClass
			r.line(l, io.Discard)
			if r.written > before {
				steps, lastStep = append(steps, percent(r.written)), n
			}
			if v, ok := cutProto(l, "openjd_status"); ok {
				status, statusLine = r.maskLocked(v), n
			}
			if r.class != class || r.patClass != pat {
				reason = n
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 1, err
		}
	}

	fmt.Fprintf(stdout, "replay    %s · %s\n", filepath.Base(path), plural(n, "line"))
	switch {
	case len(steps) == 0:
		fmt.Fprintln(stdout, "progress  none: no line matched the tool's progress patterns")
	default:
		if len(steps) > 8 {
			steps = append(append(steps[:3:3], "…"), steps[len(steps)-4:]...)
		}
		fmt.Fprintf(stdout, "progress  %s (line %d)\n", strings.Join(steps, " · "), lastStep)
	}
	if status != "" {
		fmt.Fprintf(stdout, "status    %s (line %d)\n", status, statusLine)
	}
	if len(r.done) > 0 {
		fmt.Fprintf(stdout, "frames    %s finished\n", plural(len(r.done), "frame"))
	}
	if r.class == "" && r.patClass == "" {
		fmt.Fprintln(stdout, "outcome   no failure reported: the exit status decides (0 succeeds, anything else is retried)")
		return 0, nil
	}
	o := r.result(1, false, nil)
	fmt.Fprintf(stdout, "outcome   failed · %s · %q (line %d)\n", o.Class, o.Reason, reason)
	return 0, nil
}

func percent(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/10, 'f', -1, 64) + "%"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
