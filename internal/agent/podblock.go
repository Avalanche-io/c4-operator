package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A block in a unit pod (c4-agent block-pod): the same block a host runner
// runs as a background session, run instead inside a container as one
// attempt, in Claude Code's print mode, one invocation per part. Its
// session lives in the unit's output view (SessionDir), so the attempt
// after a yield resumes it from the view by its ID, here or on a host.
//
// The pod's environment is the block's (C4_BLOCK_*, C4_UNIT_NAME), the
// unit's ($C4_OUTPUT, $C4_CONTROL, $C4_PODINFO, $C4_RESUME_VIEW) and the
// holder's store (CLAUDE_CONFIG_DIR, a volume the holder signed in to
// through Claude Code's own flow, resident mode). The credential stays in
// that store; the output view never holds it.
type PodBlock struct {
	// Out is the unit's output root ($C4_OUTPUT, the writable input the
	// resume view is mounted at).
	Out string
	// ConfigDir is the holder's Claude Code store (CLAUDE_CONFIG_DIR).
	ConfigDir string
	// LeaseDir, when set, is where the pod's credential lease is mounted,
	// read-only ($C4_LEASE_DIR): the store's credentials file is a link to
	// the lease's, so a renewal reaches the running session.
	LeaseDir string
	// Cwd is the folder the session runs in. The transcript is filed by it,
	// so a session that moves keeps the same one on every machine.
	Cwd string
	// Claude is the claude binary; HookCommand the Stop hook block sessions
	// run (c4-agent checkpoint --hook, with this agents home).
	Claude, HookCommand string
	// Env reads the pod's environment.
	Env func(string) string
	// AskPoll is how often a block holding for an answer looks for it
	// (default 2 s).
	AskPoll time.Duration
	Logf    func(format string, args ...any)
}

// Exit codes of a block's attempt: done, yielded at a safe point (also a
// success for the pod: the farm resumes it from its view), gave up.
const (
	PodBlockDone    = 0
	PodBlockYielded = 0
	PodBlockGaveUp  = 3
	// PodBlockNeedsInformation: the brief lacks information only a person
	// could give (the queue's NeedsInformation; c4-unit needs's status).
	PodBlockNeedsInformation = 12
)

func (b *PodBlock) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// pod is the pod as far as the block's prompts and flags read it.
func (b *PodBlock) pod() *corev1.Pod {
	var env []corev1.EnvVar
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	ns := b.Env("C4_NAMESPACE") // a unit pod holds no service-account token
	if ns == "" {
		ns = "default"
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: map[string]string{c4v1.LabelJob: b.Env("C4_JOB")}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "block", Env: env}}}}
}

// Run runs the attempt and returns its exit code.
func (b *PodBlock) Run(ctx context.Context) (int, error) {
	p := b.pod()
	u := UnitDirs{Root: b.Out, Out: b.Out, Control: b.Env("C4_CONTROL"), PodInfo: b.Env("C4_PODINFO")}
	st := &Store{Dir: filepath.Join(b.Out, SessionDir, "agents")}
	h := New(st, podClaude{})
	if err := os.MkdirAll(filepath.Join(b.Out, ".c4"), 0o755); err != nil {
		return 1, err
	}
	if err := b.linkLease(); err != nil {
		return 1, err
	}
	name := blockMember(p)
	flags, err := blockFlagsFor(p, name, b.HookCommand)
	if err != nil {
		return 1, err
	}
	var m *Member
	var prompt string
	// an attempt never inherits an open question: the one an earlier
	// attempt asked comes back as this attempt's decision or a message
	ClearAsking(b.Out)
	if _, err := ReadSessionInfo(b.Out); err == nil {
		// resumed from a view: the session comes with it
		if m, err = h.ImportSession(ctx, b.Out, b.ConfigDir); err != nil {
			return 1, err
		}
		m.Epoch++
		m.Profile = blockProfile(p)
		if err := st.Put(m); err != nil {
			return 1, err
		}
		b.logf("resumed %s (session %s, epoch %d) from its view", m.Name, m.Session, m.Epoch)
		prompt = reworkPrompt(p, u)
		if err := b.claude(ctx, append(append([]string{"-p", "--resume", m.Session}, flags...), "--", prompt), nil); err != nil {
			return 1, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		session := newSessionID()
		m = &Member{Name: name, Session: session, Cwd: b.Cwd, ConfigDir: b.ConfigDir, Flags: flags, Block: true, Profile: blockProfile(p), Registered: time.Now().UTC()}
		b.logf("started %s (session %s)", name, session)
		register := func(exited <-chan struct{}) error { return b.register(ctx, h, m, exited) }
		if err := b.claude(ctx, append(append([]string{"-p", "--session-id", session}, flags...), "--", blockPrompt(p, u)), register); err != nil {
			return 1, err
		}
	} else {
		return 1, err
	}
	for {
		if err := ExportSession(st, m, b.Out); err != nil {
			return 1, err
		}
		if msg, ok, err := takeNeeds(u); ok {
			if err != nil {
				b.logf("%s: reported missing information, but %v", m.Name, err)
				return PodBlockGaveUp, nil
			}
			return b.needsInformation(m, msg)
		}
		if b.stopped(u) {
			b.logf("%s: the farm said stop at this safe point; it yields with its session in its view", m.Name)
			return PodBlockYielded, nil
		}
		if answerOnly(p) {
			b.logf("%s: answered what it was asked", m.Name)
			return PodBlockDone, nil
		}
		if a, ok := ReadAsking(b.Out); ok {
			prompt, yielded, err := b.holdForAnswer(ctx, p, u, st, m, a)
			if err != nil {
				return 1, err
			}
			if yielded {
				b.logf("%s: waits for %s's answer without its slot; it yields with its session in its view", m.Name, a.To)
				return PodBlockYielded, nil
			}
			if err := b.claude(ctx, append(append([]string{"-p", "--resume", m.Session}, flags...), "--", prompt), nil); err != nil {
				return 1, err
			}
			continue
		}
		if note, more := takeMore(u); more {
			b.logf("%s: a part is done; next: %s", m.Name, note)
			if err := b.claude(ctx, append(append([]string{"-p", "--resume", m.Session}, flags...), "--", partPrompt(note, u)), nil); err != nil {
				return 1, err
			}
			continue
		}
		if takeDone(u) {
			b.logf("%s: its task is done", m.Name)
			return PodBlockDone, nil
		}
		b.logf("%s: ended its turn without finishing its task or noting a next part", m.Name)
		return PodBlockGaveUp, nil
	}
}

// linkLease points the store's credentials file at the lease's.
func (b *PodBlock) linkLease() error {
	if b.LeaseDir == "" {
		return nil
	}
	target := filepath.Join(b.LeaseDir, c4v1.LeaseCredentialKey)
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("the pod's credential lease: %w", err)
	}
	if err := os.MkdirAll(b.ConfigDir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(b.ConfigDir, ".credentials.json")
	os.Remove(link)
	return os.Symlink(target, link)
}

// holdForAnswer keeps the block in its slot until the answer to its
// question comes, and returns the prompt that wakes it with the answer.
// Past its hold, a question to a named unit becomes a wait served by that
// unit (yielded: the farm stops the block there), and one to the job's
// answerer wakes it to decide within its brief.
func (b *PodBlock) holdForAnswer(ctx context.Context, p *corev1.Pod, u UnitDirs, st *Store, m *Member, a *Asking) (prompt string, yielded bool, err error) {
	poll := b.AskPoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	hold := askHold(p)
	b.logf("%s: asked %s; holds up to %s for the answer", m.Name, a.ID, hold)
	for {
		if msg, ok := answerFor(u, a.ID); ok {
			markSteerSeen(u, msg.Seq)
			ClearAsking(b.Out)
			b.logf("%s: the answer to %s came", m.Name, a.ID)
			return answerPrompt(a, msg, u), false, nil
		}
		if time.Since(a.At) >= hold {
			break
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(poll):
		}
	}
	if a.To != "" {
		ref := ""
		if cp, err := st.Last(m.Name); err == nil && cp != nil {
			ref = point(m, cp)
		}
		stopped, err := openAnswerWait(ctx, b.Env("C4_UNIT_BIN"), u, a, ref)
		if err != nil {
			b.logf("%s: could not open a wait on %s: %v", m.Name, a.To, err)
		}
		if stopped {
			return "", true, nil
		}
	}
	ClearAsking(b.Out)
	return noAnswerPrompt(a, hold), false, nil
}

// needsInformation ends the attempt for information its brief lacks: the
// text is the container's termination message, and the status 12 tells
// the farm (c4-unit needs does the same).
func (b *PodBlock) needsInformation(m *Member, msg string) (int, error) {
	b.logf("%s: needs information: %s", m.Name, strings.ReplaceAll(msg, "\n", "; "))
	path := b.Env("C4_TERMINATION_LOG")
	if path == "" {
		path = "/dev/termination-log"
	}
	if err := os.WriteFile(path, []byte(msg), 0o644); err != nil {
		return 1, err
	}
	return PodBlockNeedsInformation, nil
}

// register records the new session's member once its transcript appears,
// so the Stop hook finds it at the turn's end. A claude that exits without
// one (not signed in, say) leaves nothing to register.
func (b *PodBlock) register(ctx context.Context, h *Harness, m *Member, exited <-chan struct{}) error {
	deadline := time.Now().Add(60 * time.Second)
	for last := false; ; {
		tr, err := FindTranscript(m.ConfigDir, m.Cwd, m.Session)
		if err == nil {
			m.Transcript = tr
			break
		}
		if last || time.Now().After(deadline) {
			return fmt.Errorf("session %s: its transcript did not appear: %w", m.Session, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			last = true
		case <-time.After(200 * time.Millisecond):
		}
	}
	unlock, err := h.Store.Lock(m.Name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.Store.Put(m); err != nil {
		return err
	}
	_, _, err = h.checkpointLocked(ctx, m, "register")
	return err
}

// claude runs one print-mode invocation in the session's folder, the
// holder's store as its config dir, and waits for it. while, when set, runs
// once the process has started; exited closes when it ends.
func (b *PodBlock) claude(ctx context.Context, args []string, while func(exited <-chan struct{}) error) error {
	if err := os.MkdirAll(b.Cwd, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, b.Claude, args...)
	cmd.Dir = b.Cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+b.ConfigDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- cmd.Wait(); close(exited) }()
	var werr error
	if while != nil {
		werr = while(exited)
	}
	err := <-done
	if werr != nil {
		return werr
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// a turn the hook ended (stop at a safe point) or a failed turn:
		// the loop reads what the block left
		b.logf("claude exited %d", exit.ExitCode())
		return nil
	}
	return err
}

// stopped reports whether the farm's answer at the block's latest safe
// point is stop.
func (b *PodBlock) stopped(u UnitDirs) bool {
	sp, ok := readSafepoint(u.Out)
	if !ok || u.Control == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(u.Control, "safepoint.decision"))
	if err != nil {
		return false
	}
	var d hostDecision
	if json.Unmarshal(raw, &d) != nil {
		return false
	}
	return d.Seq == sp.Seq && d.Do == "stop"
}

// podClaude is the CLI as a pod sees it: no background sessions to list or
// stop, since each turn is one print-mode process.
type podClaude struct{}

func (podClaude) Active(ctx context.Context, configDir string, all bool) ([]Active, error) {
	return nil, nil
}

func (podClaude) Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error) {
	return "", errors.New("a pod resumes its session in print mode")
}

func (podClaude) Stop(ctx context.Context, configDir, session string) error { return nil }

// newSessionID is a random version 4 UUID, a session ID Claude Code
// accepts with --session-id.
func newSessionID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
