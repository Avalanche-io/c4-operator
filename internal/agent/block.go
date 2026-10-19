package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Blocks: the builder driver (proposals agent-jobs 10.14 step 4). A plan
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
//	C4_BLOCK_REWORK          a reworked attempt: what the review asks (a verdict's with.env)
const (
	EnvBlock       = "C4_AGENT_BLOCK"
	EnvBlockPrompt = "C4_BLOCK_PROMPT"
	EnvBlockRepo   = "C4_BLOCK_REPO"
	EnvBlockTools  = "C4_BLOCK_TOOLS"
	EnvBlockModel  = "C4_BLOCK_MODEL"
	EnvBlockRework = "C4_BLOCK_REWORK"
)

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

// blockFlags are the flags a block's session starts with: its name, a
// permission mode that never stops to ask (a tool the job did not allow is
// refused, not prompted for), the job's tools, its model, and the Stop
// hook that checkpoints every turn and answers the farm's safe points.
func (r *Runner) blockFlags(p *corev1.Pod, name string) ([]string, error) {
	hook, err := HookSettingsCommand(r.HookCommand)
	if err != nil {
		return nil, err
	}
	flags := []string{"-n", name, "--permission-mode", "dontAsk", "--settings", string(hook)}
	tools := podEnv(p, EnvBlockTools)
	if tools == "" {
		tools = DefaultBlockTools
	}
	flags = append(flags, "--allowedTools")
	flags = append(flags, strings.Fields(tools)...)
	if model := podEnv(p, EnvBlockModel); model != "" {
		flags = append(flags, "--model", model)
	}
	return flags, nil
}

// blockPrompt is the first prompt of a block's session: its task, and
// where its work and its control documents go.
func blockPrompt(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are one block of a c4 plan job: block %s of job %s/%s. Work on your task alone; nobody will answer questions, so do not ask any.\n\n",
		podEnv(p, "C4_UNIT_NAME"), p.Namespace, p.Labels[c4v1.LabelJob])
	fmt.Fprintf(&b, "Your task:\n%s\n\n", strings.TrimSpace(podEnv(p, EnvBlockPrompt)))
	b.WriteString(whereThingsAre(p, u))
	b.WriteString("When the task is done, reply with a one-line summary and stop.")
	return b.String()
}

// reworkPrompt wakes a block's session for a later attempt: what the review
// asks, or that the farm resumed it, and its new unit directory.
func reworkPrompt(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	if why := strings.TrimSpace(podEnv(p, EnvBlockRework)); why != "" {
		fmt.Fprintf(&b, "A review sent your block back. It asks:\n%s\n\nFix that, and only that.\n\n", why)
	} else {
		b.WriteString("The farm resumed your block where it stopped. Carry on with your task.\n\n")
	}
	b.WriteString(whereThingsAre(p, u))
	b.WriteString("This attempt has a new output directory: write its control documents there, not in the old one. When the task is done, reply with a one-line summary and stop.")
	return b.String()
}

func whereThingsAre(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	b.WriteString("Where things are:\n")
	if repo := podEnv(p, EnvBlockRepo); repo != "" {
		fmt.Fprintf(&b, "- The project's repository: %s. Do your work there.\n", repo)
	}
	fmt.Fprintf(&b, "- This attempt's output directory: %s. Control documents go in %s/.c4/:\n", u.Out, u.Out)
	b.WriteString("  - a generator writes its expansion document (version 1) to expand.json there;\n")
	b.WriteString("  - a review writes its verdict (version 1) to verdict.json there.\n\n")
	return b.String()
}

// HookSettingsCommand is the --settings document that installs the
// c4-agent Stop hook for one session, running command.
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
		Hooks []hook `json:"hooks"`
	}
	s := map[string]map[string][]group{"hooks": {"Stop": {{Hooks: []hook{{Type: "command", Command: command, Timeout: 30}}}}}}
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
	if p.Status.StartTime == nil {
		return true
	}
	return !cp.At.Before(p.Status.StartTime.Time)
}
