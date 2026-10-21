// Command c4-unit is the helper every C4 job image carries, for the parts
// of the unit contract a script would otherwise spell out: progress, frame
// previews, yielding at a checkpoint, and resuming.
//
//	c4-unit run                     run the tool in $C4_TOOLS/tool.yaml with the
//	                                parameters in $C4_PARAMS: progress, previews,
//	                                stalls and .c4/outcome from what it prints (run.go)
//	                                on a re-check, a tool not of kind agent does not
//	                                run: its question stands as asked ($C4_NEEDED,
//	                                $C4_NEEDED_WHY, $C4_NEEDED_ABOUT), exit 12
//	c4-unit progress 0.4            write the unit's progress (0 to 1)
//	c4-unit preview IMAGE FRAME     write preview/FRAME.png for the HUD's player
//	c4-unit yield-requested         exit 0 if the farm asked this unit to yield
//	c4-unit safepoint [--checkpoint REF] [--wait 20s]
//	                                at a safe point, ask the farm whether to stop
//	                                here: exit 10 (and print "stop") or 0 ("continue")
//	c4-unit wait --server person:ID [--ask TEXT] [--choices a,b] [--reply]
//	             [--expect 2h] [--checkpoint REF] [--wait 20s]
//	                                at a safe point, ask a person (or a party, or a
//	                                clock) and stop until they decide: exit 10
//	                                ("waits") to stop; 1 if the farm refused
//	c4-unit steer [--json]          print the messages sent to this unit since it
//	                                last read them (a person steering it while it
//	                                runs), and note them read; exit 1 if none
//	c4-unit ask --to unit:NAME --ask TEXT
//	                                ask another unit of the job for a fact this one
//	                                lacks; prints the question's id. The answer
//	                                comes as a steering message (c4-unit steer)
//	c4-unit answer --ask ID (--answer TEXT | --cannot WHY)
//	                                answer a question another unit asked this one
//	c4-unit needs --what TEXT [--why TEXT] [--about path:P|unit:U|job:NS/J]...
//	                                stop: the brief lacks information only a person
//	                                could give (exit 12); the unit runs again, with
//	                                it in $C4_SUPPLIED, once someone supplies it;
//	                                when work it is --about lands, the farm asks
//	                                it to re-check ($C4_RECHECK, $C4_NEEDED)
//	c4-unit withdraw --why TEXT     in a re-check: the question no longer applies
//	                                (exit 0); the unit runs again with the why in
//	                                $C4_WITHDRAWN
//	c4-unit decision                print the decision this attempt resumed with
//	                                (JSON: seq, do, note, actor, at, view, reply),
//	                                exit 1 if none
//	c4-unit measure [--of N] [--name WHAT] [--detail TEXT] VALUE
//	                                report the job's measure of done (361 of 379
//	                                rules sited), as .c4/measure.json
//	c4-unit usage --model M [--input N] [--output N] [--cache-read N] [--cache-write N]
//	                [--cost USD] [--cost-source S]
//	                                add a model call's tokens and cost to the
//	                                unit's .c4/usage.json (a model unit's spend)
//	c4-unit resume-dir              print the writable mount if this attempt resumes
//	c4-unit output                  print the writable mount
//
// The writable mount is $C4_OUTPUT, else --dir, else the one writable C4
// view found in the container's mounts.
//
// A safe point is a moment the unit could stop and lose nothing it wants
// kept: the end of an agent turn, a frame or chunk boundary, a training
// checkpoint. Arriving at one is a decision point: the unit asks, and the
// farm decides then whether it stops there or goes on.
//
//	c4-unit safepoint --checkpoint step-4000; [ $? -eq 10 ] && exit 0
//
// The request goes through the node daemon to the queue, and the answer
// comes back in $C4_CONTROL/safepoint.decision, within --wait. A soft stop
// the farm asked for earlier (the c4.avalanche.io/yield annotation, with
// its deadline in c4.avalanche.io/yield-by) is advisory: if capacity has
// returned or stopping is no longer worth it, the answer is continue. Only
// when no answer arrives in time does the command fall back on that
// request, seen through $C4_PODINFO: stop if one is still pending. After a
// soft stop's deadline the unit is stopped anyway.
//
// A unit that needs someone at a safe point (an approval, an answer, an
// edited plan) asks with c4-unit wait. The farm stops it there and frees
// its slot; when the wait is decided, the unit runs again and resumes from
// what it saved, with the decision in $C4_DECISION and, for a wait with
// --reply, the reply mounted read-only at $C4_REPLY.
//
//	c4-unit wait --server person:owner --ask "split the chapter?" --choices split,keep --checkpoint turn-12
//	[ $? -eq 10 ] && exit 0
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/needs"
)

const (
	yieldAnnotation   = "c4.avalanche.io/yield"
	yieldByAnnotation = "c4.avalanche.io/yield-by"
	// stopCode is safepoint's exit status when the unit should stop.
	stopCode = 10
	// needsCode is the exit status of a unit that stopped for information
	// its brief lacked (c4v1.NeedsExitCode).
	needsCode = 12
	// withdrawnPrefix opens a re-check's termination message when it
	// withdraws its question (c4v1.WithdrawnPrefix).
	withdrawnPrefix = "withdrawn: "
)

func main() {
	code, err := run(os.Args[1:], os.Stdout, env{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "c4-unit:", err)
	}
	os.Exit(code)
}

// env is the process environment and filesystem, swappable in tests.
type env struct {
	getenv    func(string) string
	mountinfo string // path of the mount table
	oiiotool  string // the converter for EXR and TIFF
	now       func() time.Time
	sleep     func(time.Duration)

	// for run
	environ   func() []string  // the environment the tool starts from
	stderr    io.Writer        // the runner's own messages
	signals   <-chan os.Signal // forwarded to the tool (default SIGTERM, SIGINT)
	minuteLen time.Duration    // a stall's minute
	stopGrace time.Duration    // SIGTERM to SIGKILL
	pollEvery time.Duration    // how often the tree is read for frames
}

func (e env) get(k string) string {
	if e.getenv != nil {
		return e.getenv(k)
	}
	return os.Getenv(k)
}

func run(args []string, stdout io.Writer, e env) (int, error) {
	fs := flag.NewFlagSet("c4-unit", flag.ContinueOnError)
	dir := fs.String("dir", "", "the unit's writable mount (default $C4_OUTPUT, or found from the mounts)")
	width := fs.Int("width", 960, "preview width in pixels")
	checkpoint := fs.String("checkpoint", "", "with safepoint: what the unit saved there (a step, a path, a session turn), for the record")
	wait := fs.Duration("wait", 20*time.Second, "with safepoint or wait: how long to wait for the farm's decision before falling back on its last request")
	server := fs.String("server", "", "with wait: who decides: person:<id>, party:<name> or clock:<RFC 3339 time>")
	ask := fs.String("ask", "", "with wait or ask: the question; with answer: the question's id")
	choices := fs.String("choices", "", "with wait: the decisions that resume the unit besides approve, comma-separated (fail always may)")
	reply := fs.Bool("reply", false, "with wait: the decision carries content, mounted at $C4_REPLY when the unit resumes")
	expect := fs.Duration("expect", 0, "with wait: how long the decision is expected to take")
	to := fs.String("to", "", "with ask: who answers, unit:<name> (default: the job's answerer)")
	answerText := fs.String("answer", "", "with answer: the answer")
	cannot := fs.String("cannot", "", "with answer: why this unit cannot answer")
	what := fs.String("what", "", "with needs: the information the brief lacks")
	why := fs.String("why", "", "with needs: why it is needed, or why the brief does not settle it; with withdraw: what settled the question")
	var about aboutFlag
	fs.Var(&about, "about", "with needs: work whose landing may settle it, so the farm asks this unit to re-check: path:<in the tree>, unit:<name> or job:<namespace>/<name>; repeat for more")
	asJSON := fs.Bool("json", false, "with steer: print each message as a line of JSON {seq, at, actor, text}")
	of := fs.Int64("of", 0, "with measure: the target (default the job's)")
	name := fs.String("name", "", "with measure: what is counted (default the job's)")
	detail := fs.String("detail", "", "with measure: a line of detail for the record")
	model := fs.String("model", "", "with usage: the model called")
	input := fs.Int64("input", 0, "with usage: input tokens")
	outTokens := fs.Int64("output", 0, "with usage: output tokens")
	cacheRead := fs.Int64("cache-read", 0, "with usage: input tokens read from a cache")
	cacheWrite := fs.Int64("cache-write", 0, "with usage: input tokens written to a cache")
	cost := fs.Float64("cost", -1, "with usage: the call's cost in US dollars, if known")
	costSource := fs.String("cost-source", "", "with usage: where the price came from (a metering proxy, a price table)")
	if len(args) == 0 {
		return 2, errors.New("usage: c4-unit run | progress N | preview IMAGE FRAME | yield-requested | safepoint [--checkpoint REF] | wait --server S | steer [--json] | decision | resume-dir | output")
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	pos := fs.Args()
	switch cmd {
	case "run":
		if len(pos) != 0 {
			return 2, errors.New("usage: c4-unit run (the tool is $C4_TOOLS/tool.yaml, its parameters $C4_PARAMS)")
		}
		return runTool(e, stdout, e.errOut())
	case "progress":
		if len(pos) != 1 {
			return 2, errors.New("usage: c4-unit progress N (0 to 1)")
		}
		f, err := strconv.ParseFloat(pos[0], 64)
		if err != nil || f < 0 || f > 1 {
			return 2, fmt.Errorf("progress %q: a number from 0 to 1", pos[0])
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		return code(writeAtomic(filepath.Join(out, ".c4", "progress"), []byte(strconv.FormatFloat(f, 'f', -1, 64)+"\n")))
	case "preview":
		if len(pos) != 2 {
			return 2, errors.New("usage: c4-unit preview IMAGE FRAME")
		}
		frame, err := strconv.Atoi(pos[1])
		if err != nil {
			return 2, fmt.Errorf("frame %q is not a number", pos[1])
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		return code(preview(e, pos[0], filepath.Join(out, "preview", strconv.Itoa(frame)+".png"), *width))
	case "measure":
		if len(pos) != 1 {
			return 2, errors.New("usage: c4-unit measure [--of N] [--name WHAT] VALUE")
		}
		n, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil || n < 0 || *of < 0 {
			return 2, fmt.Errorf("measure %q: a whole number, 0 or more", pos[0])
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		doc, _ := json.Marshal(map[string]any{"value": n, "of": *of, "name": *name, "detail": *detail})
		return code(writeAtomic(filepath.Join(out, ".c4", "measure.json"), append(doc, '\n')))
	case "usage":
		if *input < 0 || *outTokens < 0 || *cacheRead < 0 || *cacheWrite < 0 || *model == "" {
			return 2, errors.New("usage: --model, and tokens of 0 or more")
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		return code(addUsage(filepath.Join(out, ".c4", "usage.json"), *model, *input, *outTokens, *cacheRead, *cacheWrite, *cost, *costSource))
	case "safepoint":
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		seq, err := safepoint(e, filepath.Join(out, ".c4", "safepoint"), *checkpoint, nil)
		if err != nil {
			return 1, err
		}
		if d, ok := awaitDecision(e, seq, *wait); ok {
			if d.Do == "stop" {
				fmt.Fprintln(stdout, "stop"+suffix(d.Why))
				return stopCode, nil
			}
			fmt.Fprintln(stdout, "continue"+suffix(d.Why))
			return 0, nil
		}
		// No answer in time: the last request the farm made stands.
		note := " [no decision in " + wait.String() + "]"
		if e.get("C4_CONTROL") == "" {
			note = " [no decision channel]"
		}
		stop, err := softStop(e)
		if err != nil {
			return 1, err
		}
		if stop.asked {
			fmt.Fprintln(stdout, stop.String()+note)
			return stopCode, nil
		}
		fmt.Fprintln(stdout, "continue"+note)
		return 0, nil
	case "wait":
		if !strings.Contains(*server, ":") || *expect < 0 {
			return 2, errors.New("usage: c4-unit wait --server person:<id>|party:<name>|clock:<time> [--ask TEXT] [--choices a,b] [--reply] [--expect D]")
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		w := map[string]any{"server": *server}
		if *ask != "" {
			w["ask"] = *ask
		}
		if *choices != "" {
			w["choices"] = strings.Split(*choices, ",")
		}
		if *reply {
			w["reply"] = true
		}
		if *expect > 0 {
			w["expect"] = expect.Seconds()
		}
		seq, err := safepoint(e, filepath.Join(out, ".c4", "safepoint"), *checkpoint, w)
		if err != nil {
			return 1, err
		}
		d, ok := awaitDecision(e, seq, *wait)
		switch {
		case ok && d.Do == "stop":
			fmt.Fprintln(stdout, "waits"+suffix(d.Why))
			return stopCode, nil
		case ok:
			// The farm answered continue: it did not open the wait.
			fmt.Fprintln(stdout, "refused"+suffix(d.Why))
			return 1, nil
		case e.get("C4_CONTROL") == "":
			fmt.Fprintln(stdout, "refused: no decision channel")
			return 1, nil
		}
		// No answer in time. The farm may still open the wait; if it does,
		// the next safe point stops.
		fmt.Fprintln(stdout, "no decision in "+wait.String())
		return 1, nil
	case "steer":
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		n, err := steer(e, out, stdout, *asJSON)
		if err != nil {
			return 1, err
		}
		if n == 0 {
			return 1, nil
		}
		return 0, nil
	case "ask":
		if strings.TrimSpace(*ask) == "" || *to != "" && !strings.HasPrefix(*to, "unit:") {
			return 2, errors.New("usage: c4-unit ask [--to unit:<name>] --ask TEXT")
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		seq, err := appendNote(filepath.Join(out, ".c4", "asks"), map[string]any{"to": *to, "ask": strings.TrimSpace(*ask)}, e)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "%s#%d\n", e.get("C4_UNIT"), seq)
		return 0, nil
	case "answer":
		if *ask == "" || (*answerText == "") == (*cannot == "") {
			return 2, errors.New("usage: c4-unit answer --ask ID (--answer TEXT | --cannot WHY)")
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		note := map[string]any{"ask": *ask}
		if *answerText != "" {
			note["answer"] = *answerText
		} else {
			note["cannot"] = *cannot
		}
		if _, err := appendNote(filepath.Join(out, ".c4", "answers"), note, e); err != nil {
			return 1, err
		}
		return 0, nil
	case "needs":
		if strings.TrimSpace(*what) == "" {
			return 2, errors.New("usage: c4-unit needs --what TEXT [--why TEXT] [--about path:P|unit:U|job:NS/J]...")
		}
		ab, err := needs.About(about)
		if err != nil {
			return 2, err
		}
		msg := needs.Message(*what, *why, ab)
		// The container's termination message is how the farm learns it.
		if err := os.WriteFile(terminationLog(e), []byte(msg), 0o644); err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, "needs: "+strings.ReplaceAll(msg, "\n", "; "))
		return needsCode, nil
	case "withdraw":
		if strings.TrimSpace(*why) == "" {
			return 2, errors.New("usage: c4-unit withdraw --why TEXT")
		}
		if e.get("C4_RECHECK") != "1" {
			return 2, errors.New("only a re-check withdraws a question (C4_RECHECK is not set)")
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		w := strings.TrimSpace(*why)
		doc, _ := json.Marshal(map[string]string{"why": w})
		if err := writeAtomic(filepath.Join(out, ".c4", "withdraw"), append(doc, '\n')); err != nil {
			return 1, err
		}
		// the farm reads the withdrawal as the re-check's termination
		// message
		if err := os.WriteFile(terminationLog(e), []byte(withdrawnPrefix+w), 0o644); err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, "withdrawn: "+w)
		return 0, nil
	case "decision":
		d := e.get("C4_DECISION")
		if d == "" {
			return 1, nil
		}
		fmt.Fprintln(stdout, d)
		return 0, nil
	case "yield-requested":
		asked, err := yieldRequested(e)
		if err != nil {
			return 1, err
		}
		if asked {
			return 0, nil
		}
		return 1, nil
	case "resume-dir":
		if e.get("C4_RESUME_VIEW") == "" {
			return 0, nil
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, out)
		return 0, nil
	case "output":
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, out)
		return 0, nil
	}
	return 2, fmt.Errorf("no command %q: run, withdraw, progress, preview, yield-requested, safepoint, wait, steer, ask, answer, needs, decision, resume-dir or output", cmd)
}

// terminationLog is where the farm reads the container's message.
func terminationLog(e env) string {
	if p := e.get("C4_TERMINATION_LOG"); p != "" {
		return p
	}
	return "/dev/termination-log"
}

// aboutFlag is a repeatable --about.
type aboutFlag []string

func (a *aboutFlag) String() string     { return strings.Join(*a, ",") }
func (a *aboutFlag) Set(v string) error { *a = append(*a, v); return nil }

func code(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// output finds the unit's writable mount.
func output(e env, dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if d := e.get("C4_OUTPUT"); d != "" {
		return d, nil
	}
	mi := e.mountinfo
	if mi == "" {
		mi = "/proc/self/mountinfo"
	}
	f, err := os.Open(mi)
	if err != nil {
		return "", errors.New("no writable mount: set C4_OUTPUT or pass --dir")
	}
	defer f.Close()
	var found []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// mountinfo: id parent major:minor root mountpoint options ... - fstype source superoptions
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, x := range fields {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) || len(fields) < 6 {
			continue
		}
		mp, opts, fstype, super := fields[4], fields[5], fields[sep+1], strings.Join(fields[sep+2:], " ")
		// a C4 view is an overlay whose layers live in the node's c4 cache
		if fstype != "overlay" || mp == "/" || !strings.Contains(super, "/var/lib/c4/") || !strings.HasPrefix(opts, "rw") {
			continue
		}
		found = append(found, unescape(mp))
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errors.New("no writable C4 view is mounted: set C4_OUTPUT or pass --dir")
	}
	return "", fmt.Errorf("more than one writable C4 view (%s): pass --dir", strings.Join(found, ", "))
}

// unescape undoes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// yieldRequested reports whether the farm asked the unit to stop.
func yieldRequested(e env) (bool, error) {
	s, err := softStop(e)
	return s.asked, err
}

// stopRequest is the farm's soft stop, from the pod's annotations.
type stopRequest struct {
	asked      bool
	reason, by string
}

func (s stopRequest) String() string {
	out := "stop"
	if s.reason != "" {
		out += ": " + s.reason
	}
	if s.by != "" {
		out += " (by " + s.by + ")"
	}
	return out
}

// softStop reads the pod's annotations from the downward API file
// (key="value" lines).
func softStop(e env) (stopRequest, error) {
	p := e.get("C4_PODINFO")
	if p == "" {
		return stopRequest{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return stopRequest{}, err
	}
	var s stopRequest
	for _, l := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(strings.TrimSpace(v)); err == nil {
			v = u
		}
		switch strings.TrimSpace(k) {
		case yieldAnnotation:
			s.asked, s.reason = true, v
		case yieldByAnnotation:
			s.by = v
		}
	}
	return s, nil
}

// decision is the farm's answer at a safe point
// ($C4_CONTROL/safepoint.decision, written by the node daemon).
type decision struct {
	Seq int    `json:"seq"`
	Do  string `json:"do"`
	Why string `json:"why"`
}

func suffix(why string) string {
	if why == "" {
		return ""
	}
	return ": " + why
}

// awaitDecision waits up to wait for the farm's answer to safe point seq.
func awaitDecision(e env, seq int, wait time.Duration) (decision, bool) {
	dir := e.get("C4_CONTROL")
	if dir == "" || wait <= 0 {
		return decision{}, false
	}
	now, sleep := time.Now, time.Sleep
	if e.now != nil {
		now = e.now
	}
	if e.sleep != nil {
		sleep = e.sleep
	}
	until := now().Add(wait)
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "safepoint.decision")); err == nil {
			var d decision
			if json.Unmarshal(b, &d) == nil && d.Seq == seq && (d.Do == "stop" || d.Do == "continue") {
				return d, true
			}
		}
		if !now().Before(until) {
			return decision{}, false
		}
		sleep(200 * time.Millisecond)
	}
}

// appendNote adds a note, numbered one past the last and timed, to a JSON
// list file the node daemon relays to the farm (.c4/asks, .c4/answers),
// and returns its number.
func appendNote(path string, note map[string]any, e env) (int, error) {
	var notes []map[string]any
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &notes); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	at := time.Now()
	if e.now != nil {
		at = e.now()
	}
	note["seq"], note["at"] = len(notes)+1, at.UTC().Format(time.RFC3339)
	notes = append(notes, note)
	b, err := json.Marshal(notes)
	if err != nil {
		return 0, err
	}
	return len(notes), writeAtomic(path, b)
}

// steerMsg is a message to the unit ($C4_CONTROL/steer, written by the
// node daemon).
type steerMsg struct {
	Seq   int       `json:"seq"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor,omitempty"`
	Text  string    `json:"text"`
	// Ask is a question to this unit, Answer one to a question it asked.
	Ask    json.RawMessage `json:"ask,omitempty"`
	Answer json.RawMessage `json:"answer,omitempty"`
}

// steer prints the messages after the last one read (out/.c4/steer-seen),
// notes the last as read, and returns how many it printed.
func steer(e env, out string, w io.Writer, asJSON bool) (int, error) {
	dir := e.get("C4_CONTROL")
	if dir == "" {
		return 0, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "steer"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var msgs []steerMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		return 0, fmt.Errorf("steer: %w", err)
	}
	seenPath := filepath.Join(out, ".c4", "steer-seen")
	seen := 0
	if s, err := os.ReadFile(seenPath); err == nil {
		seen, _ = strconv.Atoi(strings.TrimSpace(string(s)))
	}
	n, last := 0, seen
	for _, m := range msgs {
		if m.Seq <= seen {
			continue
		}
		if asJSON {
			line, _ := json.Marshal(m)
			fmt.Fprintln(w, string(line))
		} else {
			who := m.Actor
			if who == "" {
				who = "someone"
			}
			fmt.Fprintf(w, "%s (%s): %s\n", who, m.At.UTC().Format(time.RFC3339), m.Text)
		}
		n, last = n+1, max(last, m.Seq)
	}
	if last > seen {
		if err := writeAtomic(seenPath, []byte(strconv.Itoa(last)+"\n")); err != nil {
			return n, err
		}
	}
	return n, nil
}

// safepoint records a safe point in path: its sequence number (one past
// the last), the time, what was saved there, and the wait it asks for, if
// any; and returns the number. The node daemon asks the farm for a
// decision on it.
func safepoint(e env, path, checkpoint string, wait map[string]any) (int, error) {
	seq := 1
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "seq "); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					seq = n + 1
				}
			}
		}
	}
	at := time.Now()
	if e.now != nil {
		at = e.now()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seq %d\nat %s\n", seq, at.UTC().Format(time.RFC3339Nano))
	if checkpoint != "" {
		fmt.Fprintf(&b, "checkpoint %s\n", strings.ReplaceAll(checkpoint, "\n", " "))
	}
	if wait != nil {
		w, err := json.Marshal(wait) // one line: JSON escapes newlines
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(&b, "wait %s\n", w)
	}
	return seq, writeAtomic(path, []byte(b.String()))
}

// preview writes a PNG preview of an image. PNG and JPEG are converted
// here; EXR, TIFF and anything else go through oiiotool, color-managed
// from ACES to the sRGB display with OCIO's built-in studio config.
func preview(e env, src, dst string, width int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(src)) {
	case ".png", ".jpg", ".jpeg":
		return previewNative(src, dst, width)
	}
	tool := e.oiiotool
	if tool == "" {
		tool = "oiiotool"
	}
	tmp := dst + ".tmp.png"
	cmd := exec.Command(tool, src, "--colorconfig", "ocio://studio-config-latest",
		"--ociodisplay", "sRGB - Display", "ACES 2.0 - SDR 100 nits (Rec.709)",
		"--resize", fmt.Sprintf("%dx0", width), "-d", "uint8", "-o", tmp)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %w", tool, err)
	}
	return os.Rename(tmp, dst)
}

// previewNative decodes a PNG or JPEG and writes it, scaled down to width
// by averaging whole-pixel boxes.
func previewNative(src, dst string, width int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	if b.Dx() > width && width > 0 {
		img = scale(img, width)
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func scale(src image.Image, w int) image.Image {
	b := src.Bounds()
	h := max(1, b.Dy()*w/b.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+max((y+1)*b.Dy()/h, y*b.Dy()/h+1)
		for x := range w {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+max((x+1)*b.Dx()/w, x*b.Dx()/w+1)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), uint8(a/n>>8)
		}
	}
	return dst
}

// writeAtomic writes a small file so readers never see it half written.
func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// addUsage adds one model call to the unit's usage file, under its model:
// the attempt's totals, read by the queue when the unit ends. cost < 0 is
// a call with no price known; source says where prices came from.
func addUsage(path, model string, input, output, cacheRead, cacheWrite int64, cost float64, source string) error {
	type use struct {
		Model            string   `json:"model"`
		InputTokens      int64    `json:"inputTokens"`
		OutputTokens     int64    `json:"outputTokens"`
		CacheReadTokens  int64    `json:"cacheReadTokens,omitempty"`
		CacheWriteTokens int64    `json:"cacheWriteTokens,omitempty"`
		CostUSD          *float64 `json:"costUSD,omitempty"`
	}
	var u struct {
		Models     []use  `json:"models"`
		CostSource string `json:"costSource,omitempty"`
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &u)
	}
	i := -1
	for k := range u.Models {
		if u.Models[k].Model == model {
			i = k
		}
	}
	if i < 0 {
		u.Models = append(u.Models, use{Model: model})
		i = len(u.Models) - 1
	}
	m := &u.Models[i]
	m.InputTokens += input
	m.OutputTokens += output
	m.CacheReadTokens += cacheRead
	m.CacheWriteTokens += cacheWrite
	if cost >= 0 {
		c := cost
		if m.CostUSD != nil {
			c += *m.CostUSD
		}
		m.CostUSD = &c
	}
	if source != "" {
		u.CostSource = source
	}
	b, _ := json.Marshal(u)
	return writeAtomic(path, append(b, '\n'))
}
