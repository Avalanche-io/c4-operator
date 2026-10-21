package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/needs"
)

// Blocks (docs/agents/c4-agent.md, Blocks). A plan
// job's units (class graph) run on a host as blocks: each unit is its own
// fresh Claude Code session, started for the unit's first attempt with the
// unit's task as its first prompt, and resumed by its session ID for any
// later attempt (a rework, a yield) with what changed as the prompt. A
// block ends when its task does: the session is done, the runner stops it
// and finishes the pod with the block's control documents.
//
// A pod is a block when its environment says so; the job's template sets
// the job-wide values and a unit's expansion document its own (the last
// value of a name wins, as for a container's environment):
//
//	C4_AGENT_BLOCK=1         the pod is a block
//	C4_BLOCK_PROMPT          the block's task
//	C4_BLOCK_REPO            the project's working tree on this host
//	C4_BLOCK_TOOLS           the tools it may use without asking, space separated
//	C4_BLOCK_MODEL           the model, when not the CLI's default
//	C4_BLOCK_EFFORT          the effort level (low, medium, high, xhigh, max), when not the CLI's default
//	C4_BLOCK_PROFILE         the name of its tool, for its usage.json (default: "default" without C4_BLOCK_TOOLS, else "custom")
//	C4_BLOCK_REWORK          a reworked attempt: what the review asks (a verdict's with.env)
const (
	EnvBlock        = "C4_AGENT_BLOCK"
	EnvBlockPrompt  = "C4_BLOCK_PROMPT"
	EnvBlockRepo    = "C4_BLOCK_REPO"
	EnvBlockTools   = "C4_BLOCK_TOOLS"
	EnvBlockModel   = "C4_BLOCK_MODEL"
	EnvBlockEffort  = "C4_BLOCK_EFFORT"
	EnvBlockProfile = "C4_BLOCK_PROFILE"
	EnvBlockRework  = "C4_BLOCK_REWORK"
)

// blockProfile is the name of a block's tool.
func blockProfile(p *corev1.Pod) string {
	switch {
	case podEnv(p, EnvBlockProfile) != "":
		return podEnv(p, EnvBlockProfile)
	case podEnv(p, EnvBlockTools) == "":
		return "default"
	}
	return "custom"
}

// DefaultBlockTools are the tools a block may use when its job names none:
// reading and editing files, and git and go commands.
const DefaultBlockTools = "Read Edit Write Glob Grep Bash(git:*) Bash(go:*)"

// podEnv is the value of an environment variable in a pod's first
// container: the last one of that name, as the container sees it.
func podEnv(p *corev1.Pod, name string) string {
	if len(p.Spec.Containers) == 0 {
		return ""
	}
	v := ""
	for _, e := range p.Spec.Containers[0].Env {
		if e.Name == name {
			v = e.Value
		}
	}
	return v
}

// isBlock reports whether a unit pod is a block.
func isBlock(p *corev1.Pod) bool { return podEnv(p, EnvBlock) == "1" }

var memberUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// blockMember is the member a block runs as: one per unit of a job, named
// <namespace>.<job>.<unit>.
func blockMember(p *corev1.Pod) string {
	unit := podEnv(p, "C4_UNIT_NAME")
	if unit == "" {
		unit = podEnv(p, "C4_UNIT")
	}
	if unit == "" || p.Labels[c4v1.LabelJob] == "" {
		return ""
	}
	return memberUnsafe.ReplaceAllString(p.Namespace+"."+p.Labels[c4v1.LabelJob]+"."+unit, "_")
}

// StartOptions describe a block's fresh session.
type StartOptions struct {
	Name, Cwd, ConfigDir string
	// Flags go to the CLI once; it keeps them with the session.
	Flags  []string
	Prompt string
	// TranscriptWait bounds the wait for the new session's transcript
	// (default 30 s).
	TranscriptWait time.Duration
}

// Start starts a fresh background session as a new member, a block's.
// The member must not exist: a block's later attempts resume its session.
func (h *Harness) Start(ctx context.Context, o StartOptions) (*Member, error) {
	st, ok := h.Claude.(Starter)
	if !ok {
		return nil, errors.New("this claude cannot start sessions")
	}
	if err := checkFlags(o.Flags); err != nil {
		return nil, err
	}
	unlock, err := h.Store.Lock(o.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := h.Store.Get(o.Name); err == nil {
		return nil, fmt.Errorf("member %s: %w; a block's later attempts resume it", o.Name, ErrHasSession)
	} else if !errors.Is(err, ErrNoMember) {
		return nil, err
	}
	id, err := st.Start(ctx, o.ConfigDir, o.Cwd, o.Flags, o.Prompt)
	if err != nil {
		return nil, err
	}
	session := h.fullID(ctx, o.ConfigDir, id)
	if !ValidSession(session) {
		h.Claude.Stop(ctx, o.ConfigDir, id)
		return nil, fmt.Errorf("member %s: the CLI printed %q, and no running session has that ID", o.Name, id)
	}
	// a session that cannot become the member must not run on: the next
	// attempt would start another
	registered := false
	defer func() {
		if !registered {
			h.Claude.Stop(ctx, o.ConfigDir, session)
		}
	}()
	m := &Member{Name: o.Name, Session: session, Cwd: o.Cwd, ConfigDir: o.ConfigDir, Flags: o.Flags, Block: true, Registered: h.Now().UTC()}
	wait := o.TranscriptWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := h.Now().Add(wait)
	for {
		m.Transcript, err = FindTranscript(m.ConfigDir, m.Cwd, m.Session)
		if err == nil {
			break
		}
		if h.Now().After(deadline) {
			return nil, fmt.Errorf("member %s: session %s started, but its transcript did not appear: %w", o.Name, session, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := h.Store.Put(m); err != nil {
		return nil, err
	}
	registered = true
	if _, _, err := h.checkpointLocked(ctx, m, "register"); err != nil {
		return nil, err
	}
	return m, nil
}

// blockFlags are the flags a block's session starts with:
//   - its name;
//   - restricted mode, with no MCP servers: none of the machine's user,
//     project or local settings, CLAUDE.md, connectors or MCP servers reach
//     a block, and its file tools stay in its working folders (Claude Code
//     2.1.303: --restricted ignores the settings files but keeps
//     --settings; --strict-mcp-config drops MCP servers and connectors);
//   - the job's tools (--tools turns them on in restricted mode,
//     --allowedTools lets them run) and a permission mode that never stops
//     to ask: a tool the job did not allow is refused, not prompted for;
//   - its model, and the Stop hook that checkpoints every turn and answers
//     the farm's safe points.
func (r *Runner) blockFlags(p *corev1.Pod, name string) ([]string, error) {
	return blockFlagsFor(p, name, r.HookCommand)
}

func blockFlagsFor(p *corev1.Pod, name, hookCommand string) ([]string, error) {
	hook, err := HookSettingsCommand(hookCommand)
	if err != nil {
		return nil, err
	}
	flags := []string{"-n", name, "--restricted", "--strict-mcp-config", "--permission-mode", "dontAsk", "--settings", string(hook)}
	tools := podEnv(p, EnvBlockTools)
	if tools == "" {
		tools = DefaultBlockTools
	}
	var names []string
	for _, t := range strings.Fields(tools) {
		n, _, _ := strings.Cut(t, "(")
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	flags = append(append(flags, "--tools"), names...)
	flags = append(flags, "--allowedTools")
	flags = append(flags, strings.Fields(tools)...)
	if model := podEnv(p, EnvBlockModel); model != "" {
		flags = append(flags, "--model", model)
	}
	if effort := podEnv(p, EnvBlockEffort); effort != "" {
		flags = append(flags, "--effort", effort)
	}
	return flags, nil
}

// blockPrompt is the first prompt of a block's session: its task, and
// where its work and its control documents go.
func blockPrompt(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are one block of a c4 plan job: block %s of job %s/%s. Work on your task, deciding what your brief leaves to you; when you lack a fact another unit of this job has, ask that unit as described below.\n\n",
		podEnv(p, "C4_UNIT_NAME"), p.Namespace, p.Labels[c4v1.LabelJob])
	fmt.Fprintf(&b, "Your task:\n%s\n\n", strings.TrimSpace(podEnv(p, EnvBlockPrompt)))
	b.WriteString(whereThingsAre(p, u))
	b.WriteString("When the task is done, write a one-line summary of it to done in that .c4/ directory, reply with the same line, and stop.")
	return b.String()
}

// reworkPrompt wakes a block's session for a later attempt: what the review
// asks, or that the farm resumed it, and its new unit directory.
func reworkPrompt(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	if ids := strings.TrimSpace(podEnv(p, "C4_ANSWER_ONLY")); ids != "" {
		return answerOnlyPrompt(ids, u)
	}
	if recheck(p) {
		return recheckPrompt(p, u)
	}
	if s := suppliedText(p); s != "" {
		b.WriteString(s)
	} else if s := withdrawnText(p); s != "" {
		b.WriteString(s)
	} else if why := strings.TrimSpace(podEnv(p, EnvBlockRework)); why != "" {
		fmt.Fprintf(&b, "A review sent your block back. It asks:\n%s\n\nFix that, and only that.\n\n", why)
	} else if d := decisionText(p, replyDir(p, u)); d != "" {
		b.WriteString(d)
	} else {
		b.WriteString("The farm resumed your block where it stopped. Carry on with your task.\n\n")
	}
	b.WriteString(whereThingsAre(p, u))
	b.WriteString("This attempt has a new output directory: write its control documents there, not in the old one. When the task is done (or if it already was), write a one-line summary of it to done in that .c4/ directory, reply with the same line, and stop.")
	return b.String()
}

func whereThingsAre(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	b.WriteString("Where things are:\n")
	if br, ok := blockBranch(p); ok && isDir(br.Dir) {
		fmt.Fprintf(&b, "- Your own branch of the project's repository, %s, checked out at %s. Do your work there and commit it on that branch: your commits are your output, and the branch is how your work comes back.\n", br.Branch, br.Dir)
	} else if repo := podEnv(p, EnvBlockRepo); repo != "" {
		fmt.Fprintf(&b, "- The project's repository: %s. Do your work there.\n", repo)
	}
	b.WriteString("- Run each shell command on its own: no cd, no &&, ; or pipes. Use absolute paths, git -C <dir> and go -C <dir>; only the commands you were allowed run, and a compound one is refused.\n")
	fmt.Fprintf(&b, "- This attempt's output directory: %s. Control documents go in %s/.c4/:\n", u.Out, u.Out)
	b.WriteString("  - a generator writes its expansion document (version 1) to expand.json there;\n")
	b.WriteString("  - a review writes its verdict (version 1) to verdict.json there.\n")
	b.WriteString("- If your task says it has parts, do one part per turn. When a part is done and more remain, write a one-line note of the next part to more in that .c4/ directory and end your turn: the farm then wakes you for it, or stops you there and resumes you later. After the last part, write no note (to withdraw one, write the file empty).\n")
	b.WriteString(`- If you lack a fact another unit of this job has (the plan's lead, a peer's interface, a reviewer's criterion), ask it: write ask.json in that .c4/ directory, {"to": "unit:<its name>", "ask": "the question, on its own"} (without "to" it goes to the job's answerer), and end your turn. You keep your place and are woken with the answer. Questions to you arrive as messages, with how to answer them.` + "\n")
	b.WriteString(`- Act on your brief: decide everything it leaves to you, and never ask for confirmation of what it says. If it lacks information only a person could give, do not guess: write needs.json in that .c4/ directory, {"needs": "exactly what is missing", "why": "why the brief does not settle it", "about": ["unit:<a peer whose work may settle it>", "path:<where in the tree it would land>"]} (about is optional), and end your turn. That stops this block as an error the pipeline must fix; once the information is supplied, you resume with it, and when work it is about lands you are asked whether it still applies.` + "\n\n")
	return b.String()
}

// holdForAnswer keeps a block that asked another unit a question in its
// slot until the answer comes, and wakes its session with it. Past its hold
// (askHold), a question to a named unit becomes a wait served by that unit
// (c4-unit wait --server unit:<name>): the farm stops the block and gives
// its slot up, and it resumes with the answer as its decision. A question
// to the job's answerer, with no unit to name, wakes the block instead to
// decide within its brief.
func (r *Runner) holdForAnswer(ctx context.Context, p *corev1.Pod, m *Member, u UnitDirs, a *Asking) error {
	if msg, ok := answerFor(u, a.ID); ok {
		markSteerSeen(u, msg.Seq)
		ClearAsking(u.Out)
		if _, err := r.Harness.Resume(ctx, m.Name, answerPrompt(a, msg, u)); err != nil {
			return err
		}
		r.since[p.UID] = r.now()
		r.logf("%s: woke with the answer to %s", m.Name, a.ID)
		return nil
	}
	if r.now().Sub(a.At) < askHold(p) || r.waited[p.UID] {
		return nil
	}
	if a.To != "" {
		ref := ""
		if cp, err := r.Harness.Store.Last(m.Name); err == nil && cp != nil {
			ref = point(m, cp)
		}
		if stopped, err := openAnswerWait(ctx, r.UnitBin, u, a, ref); err == nil && stopped {
			r.waited[p.UID] = true
			r.logf("%s: no answer to %s within %s; waits for %s without its slot", m.Name, a.ID, askHold(p), a.To)
			return nil
		} else if err != nil {
			r.logf("%s: could not open a wait on %s: %v", m.Name, a.To, err)
		}
	}
	// no wait to give the slot up for: the block decides within its brief
	ClearAsking(u.Out)
	if _, err := r.Harness.Resume(ctx, m.Name, noAnswerPrompt(a, askHold(p))); err != nil {
		return err
	}
	r.since[p.UID] = r.now()
	r.logf("%s: no answer to %s within %s; woke it to decide", m.Name, a.ID, askHold(p))
	return nil
}

// noAnswerPrompt wakes a block whose question went unanswered.
func noAnswerPrompt(a *Asking, hold time.Duration) string {
	return fmt.Sprintf("No answer to your question %s came within %s. Decide within your brief, or if only a person could give it, report it missing, and carry on with your task.", a.ID, hold)
}

// openAnswerWait opens a wait served by the unit a block asked, at a safe
// point (c4-unit wait), in the block's unit directory. stopped reports that
// the farm opened it and stops the block there.
func openAnswerWait(ctx context.Context, bin string, u UnitDirs, a *Asking, ref string) (stopped bool, err error) {
	if bin == "" {
		bin = "c4-unit"
	}
	args := []string{"wait", "--server", a.To, "--ask", "the answer to question " + a.ID, "--wait", "8s"}
	if ref != "" {
		args = append(args, "--checkpoint", ref)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "C4_OUTPUT="+u.Out, "C4_CONTROL="+u.Control, "C4_PODINFO="+u.PodInfo)
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 10:
		return true, nil // the farm stops it there: its yield follows
	case errors.As(err, &exit):
		return false, nil // refused, or no decision in time
	}
	return false, err
}

// needsInformation stops a block whose brief lacks information only a
// person could give: a work-stopping error, not a question. The pod fails
// with reason NeedsInformation and the text as its message; the queue does
// not retry it, raises it as an incident, and requeues the unit once the
// information is supplied. Its checkpoint and its session's view are where
// that attempt resumes.
func (r *Runner) needsInformation(ctx context.Context, p *corev1.Pod, m *Member, msg string) error {
	r.logf("%s: needs information: %s", m.Name, strings.ReplaceAll(msg, "\n", "; "))
	if cp, err := r.Harness.Store.Last(m.Name); err == nil && cp != nil {
		if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputCheckpoint: point(m, cp)}); err != nil {
			return err
		}
	}
	if err := r.saveSession(ctx, p, m); err != nil {
		return err
	}
	return r.finish(ctx, p, corev1.PodFailed, "NeedsInformation", msg, PodBlockNeedsInformation)
}

// takeNeeds reads and consumes a block's .c4/needs.json, what its brief
// lacks: the message the farm records for NeedsInformation (the text, then
// "\nwhy: " and why, as c4-unit needs writes it). ok is false when there
// is none; err says why a file is malformed.
func takeNeeds(u UnitDirs) (msg string, ok bool, err error) {
	path := filepath.Join(u.Out, ".c4", "needs.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	os.Remove(path)
	var n struct {
		Needs string   `json:"needs"`
		Why   string   `json:"why,omitempty"`
		About []string `json:"about,omitempty"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return "", true, fmt.Errorf("needs.json: %v", err)
	}
	if strings.TrimSpace(n.Needs) == "" {
		return "", true, errors.New("needs.json: nothing in needs")
	}
	about, err := needs.About(n.About)
	if err != nil {
		return "", true, fmt.Errorf("needs.json: %v", err)
	}
	return needs.Message(n.Needs, n.Why, about), true, nil
}

// answerOnlyPrompt wakes a finished block to answer questions other units
// asked it (the scheduler's wakes: an extra attempt, $C4_ANSWER_ONLY), and
// nothing else.
func answerOnlyPrompt(ids string, u UnitDirs) string {
	return fmt.Sprintf("Your task is done. Other units of this job asked you questions (%s); they reach you as messages when this turn ends. Answer each from what you know and from your work, by writing answer.json in %s/.c4/, {\"ask\": \"<its id>\", \"answer\": \"...\"}, or {\"ask\": \"<its id>\", \"cannot\": \"why\"}. Change nothing else, and end your turn when they are answered.", ids, u.Out)
}

// answerOnly reports whether a block's attempt is a wake to answer only.
func answerOnly(p *corev1.Pod) bool { return strings.TrimSpace(podEnv(p, "C4_ANSWER_ONLY")) != "" }

// suppliedText is what was supplied for the information a block reported
// missing ($C4_NEEDED, $C4_SUPPLIED), for the attempt that resumes it.
func suppliedText(p *corev1.Pod) string {
	supplied := strings.TrimSpace(podEnv(p, "C4_SUPPLIED"))
	if supplied == "" {
		return ""
	}
	if needed := strings.TrimSpace(podEnv(p, "C4_NEEDED")); needed != "" {
		return fmt.Sprintf("You reported that your brief lacked:\n%s\n\nHere it is:\n%s\n\nCarry on with your task.\n\n", needed, supplied)
	}
	return fmt.Sprintf("The information your brief lacked has been supplied:\n%s\n\nCarry on with your task.\n\n", supplied)
}

// decisionText is what a person decided on the question a block asked, for
// the attempt that resumes it ($C4_DECISION), and where the reply's files
// are; "" when the attempt carries no decision.
func decisionText(p *corev1.Pod, replyDir string) string {
	raw := podEnv(p, "C4_DECISION")
	if raw == "" {
		return ""
	}
	var d struct {
		Do    string `json:"do"`
		Note  string `json:"note"`
		Actor string `json:"actor"`
		View  string `json:"view"`
	}
	if json.Unmarshal([]byte(raw), &d) != nil || d.Do == "" {
		return ""
	}
	var b strings.Builder
	who := d.Actor
	if who == "" {
		who = "a person"
	}
	if d.Do == "answer" {
		// a wait served by another unit, decided by its answer
		fmt.Fprintf(&b, "%s answered your question: %s", who, d.Note)
	} else {
		fmt.Fprintf(&b, "%s answered the question you asked: %s.", who, d.Do)
		if d.Note != "" {
			fmt.Fprintf(&b, " Their note: %s", d.Note)
		}
	}
	if d.View != "" && replyDir != "" {
		fmt.Fprintf(&b, " Their reply's files are in %s (read-only).", replyDir)
	}
	b.WriteString(" Carry on with your task in the light of it.\n\n")
	return b.String()
}

// fetchReply brings the reply to a block's question here, for an attempt
// the farm resumed with a decision whose reply is a view: on a pod it is
// mounted ($C4_REPLY); on a host it is fetched into the unit directory's
// reply/, once.
func (r *Runner) fetchReply(ctx context.Context, p *corev1.Pod, u UnitDirs) error {
	var d struct {
		View string `json:"view"`
	}
	if json.Unmarshal([]byte(podEnv(p, "C4_DECISION")), &d) != nil || d.View == "" || r.ViewSrc == nil {
		return nil
	}
	dir := filepath.Join(u.Root, "reply")
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	id, err := c4.Parse(d.View)
	if err != nil {
		return fmt.Errorf("the decision's reply %q: %w", d.View, err)
	}
	project, err := r.projectOf(ctx, p.Namespace)
	if err != nil {
		return err
	}
	tmp := dir + ".partial"
	os.RemoveAll(tmp)
	if err := FetchView(ctx, r.ViewSrc, project, id, tmp); err != nil {
		return fmt.Errorf("the decision's reply %s: %w", d.View, err)
	}
	return os.Rename(tmp, dir)
}

// replyDir is where a resumed block finds the reply to its question: the
// pod's mount ($C4_REPLY), or on a host the copy the runner fetched into
// the unit directory.
func replyDir(p *corev1.Pod, u UnitDirs) string {
	if d := podEnv(p, "C4_REPLY"); d != "" {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	if _, err := os.Stat(filepath.Join(u.Root, "reply")); err == nil {
		return filepath.Join(u.Root, "reply")
	}
	return ""
}

// HookSettingsCommand is the --settings document that installs the
// c4-agent hook for one session, running command at each turn's end and
// after each tool call.
func HookSettingsCommand(command string) ([]byte, error) {
	if command == "" {
		return nil, errors.New("no Stop hook command for block sessions (Runner.HookCommand)")
	}
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	// Stop: the turn's end, a safe point; PostToolUse: after each tool
	// call, a stop the farm asked for ends the turn at that step
	s := map[string]map[string][]group{"hooks": {
		"Stop":        {{Hooks: []hook{{Type: "command", Command: command, Timeout: 30}}}},
		"PostToolUse": {{Matcher: "*", Hooks: []hook{{Type: "command", Command: command, Timeout: 10}}}},
	}}
	return json.Marshal(s)
}

// startBlock starts a block's session for its unit's first attempt and
// reports the pod running.
func (r *Runner) startBlock(ctx context.Context, p *corev1.Pod, name string) error {
	if r.BlocksCwd == "" {
		return r.finish(ctx, p, corev1.PodFailed, "NoBlockFolder", "this runner runs no blocks (--blocks-cwd)", 1)
	}
	u := UnitDirsAt(r.unitRoot(p))
	flags, err := r.blockFlags(p, name)
	if err != nil {
		return err
	}
	b, ok, err := ensureBranch(ctx, p)
	if err != nil {
		return r.finish(ctx, p, corev1.PodFailed, "NoBranch", "the block's own branch: "+err.Error(), 1)
	}
	if ok {
		flags = append(flags, "--add-dir", b.Dir)
		r.logf("%s: works on branch %s in %s", name, b.Branch, b.Dir)
	}
	m, err := r.Harness.Start(ctx, StartOptions{Name: name, Cwd: r.BlocksCwd, Flags: flags, Prompt: blockPrompt(p, u)})
	if err != nil {
		return r.finish(ctx, p, corev1.PodFailed, "BlockNotStarted", err.Error(), 1)
	}
	if _, err := r.prepareUnit(p, m); err != nil {
		return err
	}
	r.logf("started block %s (session %s)", name, m.Session)
	return r.running(ctx, p, m)
}

// turnSince reports whether the member ended a turn after this attempt
// began: its last checkpoint is a Stop hook's, saved after the pod started.
// A session the CLI reports done before then is the last attempt's.
func (r *Runner) turnSince(m *Member, p *corev1.Pod) bool {
	cp, err := r.Harness.Store.Last(m.Name)
	if err != nil || cp == nil || cp.Event != "stop" {
		return false
	}
	if t, ok := r.since[p.UID]; ok && cp.At.Before(t) {
		return false // the turn before this part
	}
	if p.Status.StartTime == nil {
		return true
	}
	return !cp.At.Before(p.Status.StartTime.Time)
}

// takeMore reads and removes a block's note of its next part
// (<out>/.c4/more): a part is done and the task goes on. An empty note is
// one the block withdrew.
func takeMore(u UnitDirs) (string, bool) {
	path := filepath.Join(u.Out, ".c4", "more")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	os.Remove(path)
	note := strings.TrimSpace(string(b))
	return note, note != ""
}

// takeDone reports whether a block said its task is done
// (<out>/.c4/done, its one-line summary). The block's own word decides,
// not the CLI's reading of its last message: that reads a finished task
// whose summary mentions a problem, or that found nothing left to do, as
// blocked (Claude Code 2.1.303).
func takeDone(u UnitDirs) bool {
	b, err := os.ReadFile(filepath.Join(u.Out, ".c4", "done"))
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// partPrompt wakes a block for the next part of its task, in the same
// attempt.
func partPrompt(note string, u UnitDirs) string {
	return fmt.Sprintf("Go on with the next part of your task: %s\n\nThe output directory is still %s. When that part is done, write the note for the one after it to %s/.c4/more and end your turn; if it was the last, write no note, write a one-line summary of the whole task to %s/.c4/done, reply with the same line, and stop.", note, u.Out, u.Out, u.Out)
}

// blockYield decides a block's yield by its safe point. A block is asked
// to yield (c4.avalanche.io/yield) by a hold-stop, a preemption or its
// pod's deletion; the farm decides at the block's next safe point, the end
// of its turn, and may cancel the stop there (its reason passed). So a
// working block is left to reach it, without blocking the runner's tick;
// a block at its safe point waits for the farm's answer; a stop yields it
// (the session stopped at that turn, the pod Yielded with its checkpoint),
// and a continue lets it go on (wait false). A block that announces no
// safe points yields at its turn's end.
func (r *Runner) blockYield(ctx context.Context, p *corev1.Pod, m *Member) (wait bool, err error) {
	live, act, err := r.Harness.live(ctx, m)
	if err != nil {
		return true, err
	}
	if live && !act.Idle() {
		return true, nil
	}
	st := r.relays[p.UID]
	switch {
	case !live || st == nil || st.asked == 0 || st.stopped:
		return true, r.yield(ctx, p, m)
	case st.answered < st.asked:
		return true, nil // the farm has not answered yet
	}
	return false, nil
}

// saveSession leaves a yielding block's session in its unit's output view:
// the session exported into its out/, the directory stored as a view, and
// named in c4.avalanche.io/output-tree, so the queue mounts it for the next
// attempt (C4_RESUME_VIEW) on whichever machine that runs.
func (r *Runner) saveSession(ctx context.Context, p *corev1.Pod, m *Member) error {
	if !m.Block || r.Views == nil {
		return nil
	}
	u := UnitDirsAt(r.unitRoot(p))
	if err := ExportSession(r.Harness.Store, m, u.Out); err != nil {
		return fmt.Errorf("%s: export the session: %w", m.Name, err)
	}
	project, err := r.projectOf(ctx, p.Namespace)
	if err != nil {
		return err
	}
	id, err := CaptureView(ctx, r.Views, project, u.Out)
	if err != nil {
		return fmt.Errorf("%s: store the session's view: %w", m.Name, err)
	}
	r.logf("%s: its session left as view %s", m.Name, id)
	return r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputTree: id.String()})
}

// resumeFromView brings a block's session here for an attempt the queue
// resumes from a view (C4_RESUME_VIEW): the view is fetched into the unit's
// out/, and the session in it imported, rewritten for this host, before the
// runner resumes it by its ID. It may have run anywhere since it was last
// here, in a unit pod or on another host: the view is its history.
func (r *Runner) resumeFromView(ctx context.Context, p *corev1.Pod, name string) error {
	id := podEnv(p, "C4_RESUME_VIEW")
	if !isBlock(p) || id == "" || p.Status.Phase != corev1.PodPending || p.DeletionTimestamp != nil || r.ViewSrc == nil || r.fetched[p.UID] {
		return nil
	}
	vid, err := c4.Parse(id)
	if err != nil {
		return r.finish(ctx, p, corev1.PodFailed, "BadResumeView", fmt.Sprintf("C4_RESUME_VIEW %q: %v", id, err), 1)
	}
	u := UnitDirsAt(r.unitRoot(p))
	project, err := r.projectOf(ctx, p.Namespace)
	if err != nil {
		return err
	}
	defer ClearAsking(u.Out) // an attempt never inherits an open question
	if err := FetchView(ctx, r.ViewSrc, project, vid, u.Out); err != nil {
		return fmt.Errorf("%s: fetch the resume view %s: %w", name, id, err)
	}
	r.fetched[p.UID] = true
	si, err := ReadSessionInfo(u.Out)
	if errors.Is(err, os.ErrNotExist) {
		return nil // a view with no session: the block starts fresh
	}
	if err != nil {
		return err
	}
	if si.Member != name {
		return r.finish(ctx, p, corev1.PodFailed, "WrongSession", fmt.Sprintf("the resume view holds %s's session, not %s's", si.Member, name), 1)
	}
	m, err := r.Harness.ImportSession(ctx, u.Out, "")
	if err != nil {
		return err
	}
	// this host's own flags for it, should the CLI not know the session
	if m.Flags, err = r.blockFlags(p, name); err != nil {
		return err
	}
	m.Block = true
	if err := r.Harness.Store.Put(m); err != nil {
		return err
	}
	r.logf("%s: brought its session %s here from view %s", name, m.Session, id)
	return nil
}
