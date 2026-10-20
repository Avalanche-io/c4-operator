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
	Env  func(string) string
	Logf func(format string, args ...any)
}

// Exit codes of a block's attempt: done, yielded at a safe point (also a
// success for the pod: the farm resumes it from its view), gave up.
const (
	PodBlockDone    = 0
	PodBlockYielded = 0
	PodBlockGaveUp  = 3
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
	ns := "default"
	if raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		ns = strings.TrimSpace(string(raw))
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
		if b.stopped(u) {
			b.logf("%s: the farm said stop at this safe point; it yields with its session in its view", m.Name)
			return PodBlockYielded, nil
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
