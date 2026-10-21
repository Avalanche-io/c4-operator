package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Harness carries out member operations against a Store and the claude
// CLI.
type Harness struct {
	Store  *Store
	Claude Claude
	// Workspace is the workspace mode ("" chooses git inside a repository).
	Workspace string
	// Now is the clock (tests replace it).
	Now func() time.Time
	// Poll is how often Yield checks whether a busy session has finished
	// its turn.
	Poll time.Duration
	// SettleQuiet and SettleMax bound the wait for the transcript to stop
	// growing after a Stop hook.
	SettleQuiet, SettleMax time.Duration
	// KeepCopies leaves running a copy the CLI started by mistake; by
	// default Resume stops it at once, since a copy is never a member.
	KeepCopies bool
}

// New returns a Harness with the defaults.
func New(store *Store, claude Claude) *Harness {
	return &Harness{Store: store, Claude: claude, Now: time.Now, Poll: 2 * time.Second,
		SettleQuiet: 300 * time.Millisecond, SettleMax: 3 * time.Second}
}

// Errors the CLI turns into plain messages.
var (
	// ErrLive: the member's session is running, so resuming it would start
	// a copy.
	ErrLive = errors.New("session is running")
	// ErrHasSession: a member already has a session; a new one is a new
	// phase, never a silent replacement.
	ErrHasSession = errors.New("member already has a session")
	// ErrForked: the CLI started a copy instead of continuing the session.
	ErrForked = errors.New("resume started a copy of the session")
	// ErrBusy: the session did not reach a turn boundary in time.
	ErrBusy = errors.New("session still busy")
)

// forbiddenFlags start a new session or a copy, which the harness exists to
// prevent.
var forbiddenFlags = []string{"--fork-session", "--continue", "-c", "--resume", "-r", "--session-id", "--bg", "--background"}

func checkFlags(flags []string) error {
	for _, f := range flags {
		name := f
		if i := strings.IndexByte(f, '='); i >= 0 {
			name = f[:i]
		}
		for _, bad := range forbiddenFlags {
			if name == bad {
				return fmt.Errorf("flag %s is managed by c4-agent and may not be given", f)
			}
		}
	}
	return nil
}

// RegisterOptions describe a member to register.
type RegisterOptions struct {
	Name, Session, Cwd, ConfigDir string
	Flags                         []string
	// NewPhase replaces the member's session with a new one on purpose. The
	// old session must not be running.
	NewPhase bool
	// Env, when set, is an environment tree to run the member in: its own
	// config directory (ConfigDir, default members/<name>/config under the
	// agents home) is filled from it, and its version is recorded on the
	// member and each checkpoint. EnvID, when set, is the version it must be.
	Env, EnvID string
	// Import is a config directory the session ran in until now (~/.claude-b,
	// say): its transcript is copied into the member's own, unchanged in the
	// source.
	Import string
}

// Register adds a member for an existing session, or updates its flags.
// Giving a member a different session is refused unless NewPhase is set.
func (h *Harness) Register(ctx context.Context, o RegisterOptions) (*Member, error) {
	if !ValidSession(o.Session) {
		return nil, fmt.Errorf("session %q: not a session ID", o.Session)
	}
	if err := checkFlags(o.Flags); err != nil {
		return nil, err
	}
	var env Env
	if o.Env != "" {
		if o.ConfigDir == "" {
			o.ConfigDir = filepath.Join(h.Store.Dir, "members", o.Name, "config")
		}
		var err error
		if env, err = FillEnv(o.Env, o.ConfigDir, o.EnvID); err != nil {
			return nil, err
		}
		o.Flags = append(append([]string(nil), env.Flags...), o.Flags...)
	}
	if o.Import != "" {
		if o.Env == "" {
			return nil, errors.New("--import moves a session into a member's own environment: give --env too")
		}
		if _, err := FindTranscript(o.ConfigDir, o.Cwd, o.Session); err != nil {
			if _, err := h.ImportTranscript(ctx, o.Import, o.ConfigDir, o.Cwd, o.Session); err != nil {
				return nil, err
			}
		}
	}
	unlock, err := h.Store.Lock(o.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	old, err := h.Store.Get(o.Name)
	if err != nil && !errors.Is(err, ErrNoMember) {
		return nil, err
	}
	m := &Member{Name: o.Name, Session: o.Session, Cwd: o.Cwd, ConfigDir: o.ConfigDir, Flags: o.Flags, Env: env.ID, Registered: h.Now().UTC()}
	if old != nil {
		switch {
		case old.Session == o.Session:
			m = old
			if o.Flags != nil {
				m.Flags = o.Flags
			}
			if o.Cwd != "" {
				m.Cwd = o.Cwd
			}
			if env.ID != "" {
				// a new version of its environment, or its first
				m.Env, m.ConfigDir = env.ID, o.ConfigDir
			}
		case !o.NewPhase:
			return nil, fmt.Errorf("member %s has session %s: %w; a different session is a new phase (--new-phase), never a replacement for an agent that has context",
				o.Name, old.Session, ErrHasSession)
		default:
			if live, a, err := h.live(ctx, old); err != nil {
				return nil, err
			} else if live {
				return nil, fmt.Errorf("member %s: session %s is %s: %w; stop or yield it before starting a new phase", old.Name, old.Session, a.Status, ErrLive)
			}
			m.Previous = append(append([]string(nil), old.Previous...), old.Session)
			m.Phase, m.Epoch, m.Registered = old.Phase+1, old.Epoch, old.Registered
			if m.Cwd == "" {
				m.Cwd = old.Cwd
			}
			if m.ConfigDir == "" {
				m.ConfigDir = old.ConfigDir
			}
			if o.Flags == nil {
				m.Flags = old.Flags
			}
		}
	}
	if m.Transcript, err = FindTranscript(m.ConfigDir, m.Cwd, m.Session); err != nil {
		return nil, err
	}
	if err := h.Store.Put(m); err != nil {
		return nil, err
	}
	if _, _, err := h.checkpointLocked(ctx, m, "register"); err != nil {
		return nil, err
	}
	return m, nil
}

// live reports whether a member's session is running.
func (h *Harness) live(ctx context.Context, m *Member) (bool, Active, error) {
	as, err := h.Claude.Active(ctx, m.ConfigDir, false)
	if err != nil {
		return false, Active{}, err
	}
	for _, a := range as {
		if strings.EqualFold(a.SessionID, m.Session) {
			return true, a, nil
		}
	}
	return false, Active{}, nil
}

// knownBackground reports whether the CLI already knows the member's
// session as a background session, running or stopped. Resuming such a
// session with any flag starts a copy, so it is resumed with none.
func (h *Harness) knownBackground(ctx context.Context, m *Member) (bool, error) {
	as, err := h.Claude.Active(ctx, m.ConfigDir, true)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		if strings.EqualFold(a.SessionID, m.Session) && a.Kind != "interactive" {
			return true, nil
		}
	}
	return false, nil
}

// Checkpoint saves a member's turn boundary now. It returns false when the
// transcript has not changed since the last checkpoint.
func (h *Harness) Checkpoint(ctx context.Context, name, event string) (*Checkpoint, bool, error) {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	m, err := h.Store.Get(name)
	if err != nil {
		return nil, false, err
	}
	return h.checkpointLocked(ctx, m, event)
}

func (h *Harness) checkpointLocked(ctx context.Context, m *Member, event string) (*Checkpoint, bool, error) {
	return h.checkpointStopping(ctx, m, event, nil)
}

// checkpointStopping saves a checkpoint as checkpointLocked does, recording
// the background work the stop that follows cuts short (always a new
// checkpoint when there is any).
func (h *Harness) checkpointStopping(ctx context.Context, m *Member, event string, bg []Background) (*Checkpoint, bool, error) {
	prev, err := h.Store.Last(m.Name)
	if err != nil {
		return nil, false, err
	}
	if prev != nil && prev.Session != m.Session {
		prev = nil // a new phase starts its own chain
	}
	cp, err := nextCheckpoint(m.Transcript, prev)
	if err != nil {
		return nil, false, err
	}
	if prev != nil && cp.Offset == prev.Offset && cp.Note == "" && event == "stop" && len(bg) == 0 {
		return prev, false, nil
	}
	cp.Background = bg
	cp.Seq = 1
	if last, _ := h.Store.Last(m.Name); last != nil {
		cp.Seq = last.Seq + 1
	}
	cp.Epoch, cp.At, cp.Event, cp.Session, cp.Env = m.Epoch, h.Now().UTC(), event, m.Session, m.Env
	cp.Workspace = SealWorkspace(ctx, m.Cwd, h.Workspace)
	if err := h.Store.appendLog(m.Name, cp); err != nil {
		return nil, false, err
	}
	return &cp, true, nil
}

// hookInput is the part of a Claude Code hook's stdin the harness reads.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

// Hook handles a Claude Code Stop hook: it checkpoints the member the
// session belongs to. A session that is not a member is ignored, so the
// hook is harmless in sessions the harness does not manage. It returns the
// member's latest checkpoint (the one it saved, or the previous one when the
// turn added nothing), or nil for a session that is not a member.
func (h *Harness) Hook(ctx context.Context, r io.Reader) (*Checkpoint, error) {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&in); err != nil {
		return nil, fmt.Errorf("hook input: %w", err)
	}
	if in.SessionID == "" {
		return nil, nil
	}
	m, err := h.Store.BySession(in.SessionID)
	if errors.Is(err, ErrNoMember) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	path := m.Transcript
	if in.TranscriptPath != "" {
		path = in.TranscriptPath
	}
	if _, err := waitStable(path, h.SettleQuiet, h.SettleMax); err != nil {
		return nil, err
	}
	unlock, err := h.Store.Lock(m.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if m, err = h.Store.Get(m.Name); err != nil {
		return nil, err
	}
	if in.TranscriptPath != "" && in.TranscriptPath != m.Transcript {
		m.Transcript = in.TranscriptPath
		if err := h.Store.Put(m); err != nil {
			return nil, err
		}
	}
	// The latest checkpoint, new or (when the turn added nothing) the
	// previous one: the turn's end is a safe point either way.
	cp, _, err := h.checkpointLocked(ctx, m, "stop")
	if err != nil {
		return nil, err
	}
	return cp, nil
}

// ResumeResult reports a resume.
type ResumeResult struct {
	Member *Member
	// Copy is set when the CLI started a copy (ErrForked), and
	// CopyStopped when the harness stopped it again.
	Copy        string
	CopyStopped bool
}

// fullID maps an ID the CLI printed (often short) to a running session's
// full ID; it returns id unchanged when no running session matches.
func (h *Harness) fullID(ctx context.Context, configDir, id string) string {
	as, err := h.Claude.Active(ctx, configDir, false)
	if err != nil {
		return id
	}
	for _, a := range as {
		if SameSession(id, a.SessionID) {
			return strings.ToLower(a.SessionID)
		}
	}
	return id
}

// Resume continues a stopped member's session in the background with
// `claude --bg --resume`. It refuses a member whose session is running,
// since the CLI would then start a copy, raises the member's epoch, and
// checks that the CLI continued the same session.
func (h *Harness) Resume(ctx context.Context, name, prompt string) (ResumeResult, error) {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return ResumeResult{}, err
	}
	defer unlock()
	m, err := h.Store.Get(name)
	if err != nil {
		return ResumeResult{}, err
	}
	if live, a, err := h.live(ctx, m); err != nil {
		return ResumeResult{}, err
	} else if live {
		return ResumeResult{Member: m}, fmt.Errorf("member %s: session %s is %s (pid %d): %w; resuming it would start a copy. Message it instead, or yield it first",
			m.Name, m.Session, a.Status, a.Pid, ErrLive)
	}
	if err := checkFlags(m.Flags); err != nil {
		return ResumeResult{}, err
	}
	known, err := h.knownBackground(ctx, m)
	if err != nil {
		return ResumeResult{}, err
	}
	if last, err := h.Store.Last(name); err == nil && last != nil && len(last.Background) > 0 {
		// told once: the resume checkpoint below is the last one after this
		if lost := lostText(last.Background); prompt == "" {
			prompt = lost
		} else {
			prompt = lost + "\n\n" + prompt
		}
	}
	flags := m.Flags
	if known {
		// A background session the CLI knows continues only with no flags.
		flags = nil
	}
	if _, _, err := h.checkpointLocked(ctx, m, "resume"); err != nil {
		return ResumeResult{}, err
	}
	m.Epoch++
	if err := h.Store.Put(m); err != nil {
		return ResumeResult{}, err
	}
	id, err := h.Claude.Resume(ctx, m.ConfigDir, m.Cwd, m.Session, flags, prompt)
	if err != nil {
		return ResumeResult{Member: m}, err
	}
	if SameSession(id, m.Session) {
		return ResumeResult{Member: m}, nil
	}
	// The CLI started a copy. A copy is never a member: stop it, and fail.
	copyID := h.fullID(ctx, m.ConfigDir, id)
	res := ResumeResult{Member: m, Copy: copyID}
	if !h.KeepCopies {
		if err := h.Claude.Stop(ctx, m.ConfigDir, copyID); err != nil {
			return res, fmt.Errorf("member %s: the CLI started %s instead of continuing %s, and stopping it failed (%v): %w", m.Name, copyID, m.Session, err, ErrForked)
		}
		res.CopyStopped = true
	}
	return res, fmt.Errorf("member %s: the CLI started %s instead of continuing %s: %w", m.Name, copyID, m.Session, ErrForked)
}

// YieldResult reports a yield.
type YieldResult struct {
	// State is "stopped" (it was running and is now stopped at a turn
	// boundary) or "parked" (it was not running).
	State      string
	Checkpoint *Checkpoint
	// Waiting is the background work a yield is still waiting for.
	Waiting []Background
}

// Yield stops a member at its next turn boundary: at once if its session is
// idle, else after its current turn, waiting up to timeout. It checkpoints
// before stopping. A busy session that does not finish in time is left
// running (ErrBusy).
//
// A session idle with background work still running (an agent or a
// command it started) is not at a safe stop: stopping it would kill that
// work. Until harvestAt the yield waits for it to report, as for a busy
// turn (the session takes a new turn when it does). From harvestAt it
// stops the session anyway, recording the work it cut short in the
// checkpoint, so the attempt that resumes the member is told.
func (h *Harness) Yield(ctx context.Context, name string, timeout time.Duration, harvestAt time.Time) (YieldResult, error) {
	m, err := h.Store.Get(name)
	if err != nil {
		return YieldResult{}, err
	}
	deadline := h.Now().Add(timeout)
	for {
		live, a, err := h.live(ctx, m)
		if err != nil {
			return YieldResult{}, err
		}
		if !live {
			return YieldResult{State: "parked"}, nil
		}
		if a.Idle() {
			bg, err := OpenBackground(m.Transcript, a.Started())
			if err != nil {
				return YieldResult{}, err
			}
			if len(bg) == 0 || !h.Now().Before(harvestAt) {
				return h.stopIdle(ctx, name, bg)
			}
			if !h.Now().Before(deadline) {
				return YieldResult{Waiting: bg}, fmt.Errorf("member %s: %w: %d background task(s) still running, waited %s; left running", name, ErrBusy, len(bg), timeout)
			}
		} else if !h.Now().Before(deadline) {
			return YieldResult{}, fmt.Errorf("member %s: %w after %s; left running", name, ErrBusy, timeout)
		}
		select {
		case <-ctx.Done():
			return YieldResult{}, ctx.Err()
		case <-time.After(h.Poll):
		}
	}
}

func (h *Harness) stopIdle(ctx context.Context, name string, bg []Background) (YieldResult, error) {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return YieldResult{}, err
	}
	defer unlock()
	m, err := h.Store.Get(name)
	if err != nil {
		return YieldResult{}, err
	}
	live, a, err := h.live(ctx, m)
	if err != nil {
		return YieldResult{}, err
	}
	if !live {
		return YieldResult{State: "parked"}, nil
	}
	if !a.Idle() {
		return YieldResult{}, fmt.Errorf("member %s: %w (it took a new turn); try again", name, ErrBusy)
	}
	if _, err := waitStable(m.Transcript, h.SettleQuiet, h.SettleMax); err != nil {
		return YieldResult{}, err
	}
	cp, _, err := h.checkpointStopping(ctx, m, "yield", bg)
	if err != nil {
		return YieldResult{}, err
	}
	if err := h.Claude.Stop(ctx, m.ConfigDir, m.Session); err != nil {
		return YieldResult{}, err
	}
	return YieldResult{State: "stopped", Checkpoint: cp}, nil
}

// RecordStop saves a checkpoint recording the background work a stop of
// the member's session is about to cut short.
func (h *Harness) RecordStop(ctx context.Context, name string, bg []Background) error {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := h.Store.Get(name)
	if err != nil {
		return err
	}
	_, _, err = h.checkpointStopping(ctx, m, "stop", bg)
	return err
}

// ForceStop stops a member's session now, even mid-turn, after saving a
// checkpoint at its last complete line: the turn in flight is lost. It is
// for a hard limit (a power line that must not be crossed), after a yield
// has waited too long.
func (h *Harness) ForceStop(ctx context.Context, name string) (*Checkpoint, error) {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := h.Store.Get(name)
	if err != nil {
		return nil, err
	}
	live, a, err := h.live(ctx, m)
	if err != nil {
		return nil, err
	}
	var bg []Background
	if live {
		if bg, err = OpenBackground(m.Transcript, a.Started()); err != nil {
			return nil, err
		}
	}
	cp, _, err := h.checkpointStopping(ctx, m, "forced-stop", bg)
	if err != nil {
		return nil, err
	}
	if live {
		if err := h.Claude.Stop(ctx, m.ConfigDir, m.Session); err != nil {
			return nil, err
		}
	}
	return cp, nil
}

// MemberStatus is a member and what is known of it now.
type MemberStatus struct {
	Member *Member
	// State is busy, idle (running, at a turn boundary) or stopped.
	State string
	Pid   int
	Last  *Checkpoint
}

// Status reports every member.
func (h *Harness) Status(ctx context.Context) ([]MemberStatus, error) {
	ms, err := h.Store.List()
	if err != nil {
		return nil, err
	}
	actives := map[string]map[string]Active{}
	var out []MemberStatus
	for _, m := range ms {
		as, ok := actives[m.ConfigDir]
		if !ok {
			list, err := h.Claude.Active(ctx, m.ConfigDir, false)
			if err != nil {
				return nil, err
			}
			as = map[string]Active{}
			for _, a := range list {
				as[strings.ToLower(a.SessionID)] = a
			}
			actives[m.ConfigDir] = as
		}
		st := MemberStatus{Member: m, State: "stopped"}
		if a, ok := as[strings.ToLower(m.Session)]; ok {
			st.State, st.Pid = a.Status, a.Pid
		}
		if st.Last, err = h.Store.Last(m.Name); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// HookSettings is the Claude Code settings fragment that runs the
// harness's checkpoint at the end of every turn. exe is the path of the
// c4-agent binary.
func HookSettings(exe string) ([]byte, error) {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Hooks []hook `json:"hooks"`
	}
	s := map[string]map[string][]group{"hooks": {"Stop": {{Hooks: []hook{{Type: "command", Command: exe + " checkpoint --hook", Timeout: 30}}}}}}
	return json.MarshalIndent(s, "", "  ")
}
