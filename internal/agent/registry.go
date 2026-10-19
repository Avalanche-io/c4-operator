// Package agent is the harness that makes a Claude Code session a member of
// an agent bundle (proposals/agent-jobs, FIRST-SLICE step 1). It records each
// member's session, saves a checkpoint at every turn boundary, resumes a
// member by its session ID and never starts a fresh agent in its place, and
// keeps at most one live copy of each member.
//
// It drives Claude Code only through its published interfaces: the session
// transcript files, hooks, and the `claude` CLI (`agents --json`, `--bg
// --resume`, `stop`). It never edits Claude Code's settings.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Member is one agent of a bundle: a named Claude Code session.
type Member struct {
	// Name is the member's name in the bundle, and the session's display
	// name (`claude -n`).
	Name string `json:"name"`
	// Session is the Claude Code session ID.
	Session string `json:"session"`
	// Cwd is the directory the session runs in; Claude Code keeps the
	// transcript under a directory derived from it.
	Cwd string `json:"cwd"`
	// ConfigDir is the session's CLAUDE_CONFIG_DIR; empty means ~/.claude.
	ConfigDir string `json:"configDir,omitempty"`
	// Transcript is the path of the session's transcript file.
	Transcript string `json:"transcript"`
	// Flags are passed to `claude` when the member is resumed from stopped,
	// for example the permission mode. They are never passed to a live
	// session.
	Flags []string `json:"flags,omitempty"`
	// Epoch is the member's lease number: it rises by one on every resume,
	// so a record says which copy wrote it.
	Epoch int `json:"epoch"`
	// Phase counts deliberate new sessions for this member (a new phase is a
	// new agent, started on purpose; never in place of a live one).
	Phase int `json:"phase"`
	// UnitDir is the unit directory of the farm unit the member runs as on
	// a host runner (out/, control/, podinfo), so the Stop hook can answer
	// the farm's safe points from inside the session.
	UnitDir string `json:"unitDir,omitempty"`
	// Block marks a block's member: a fresh session the runner started for
	// a plan job's unit, which ends when its task does.
	Block bool `json:"block,omitempty"`
	// Profile is the tool profile a block's current attempt declares
	// (C4_BLOCK_PROFILE): the name its usage.json reports as its tools.
	Profile string `json:"profile,omitempty"`
	// Previous lists the sessions of earlier phases, oldest first.
	Previous []string `json:"previous,omitempty"`
	// Registered is when the member was first registered.
	Registered time.Time `json:"registered"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var sessionRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidName reports whether s can name a member.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidSession reports whether s looks like a Claude Code session ID.
func ValidSession(s string) bool { return sessionRE.MatchString(s) }

// Store keeps the registry and the checkpoint logs under one directory.
type Store struct {
	Dir string
}

// DefaultHome is $C4_AGENT_HOME, else ~/.c4/agents.
func DefaultHome() (string, error) {
	if h := os.Getenv("C4_AGENT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".c4", "agents"), nil
}

// ErrNoMember is returned for a name that is not registered.
var ErrNoMember = errors.New("no such member")

func (s *Store) membersDir() string { return filepath.Join(s.Dir, "members") }

func (s *Store) memberPath(name string) string {
	return filepath.Join(s.membersDir(), name+".json")
}

func (s *Store) logPath(name string) string {
	return filepath.Join(s.membersDir(), name+".checkpoints.jsonl")
}

// Lock takes the member's exclusive lock. Every change to a member, its
// checkpoint log and its session (resume, stop) happens under it.
func (s *Store) Lock(name string) (unlock func(), err error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("member name %q: want letters, digits, '.', '_' or '-'", name)
	}
	if err := os.MkdirAll(s.membersDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.membersDir(), name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// Get reads a member.
func (s *Store) Get(name string) (*Member, error) {
	data, err := os.ReadFile(s.memberPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, ErrNoMember)
	}
	if err != nil {
		return nil, err
	}
	var m Member
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("member %s: %w", name, err)
	}
	return &m, nil
}

// Put writes a member atomically. The caller holds its lock.
func (s *Store) Put(m *Member) error {
	if err := os.MkdirAll(s.membersDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.memberPath(m.Name), append(data, '\n'))
}

// List returns every member, by name.
func (s *Store) List() ([]*Member, error) {
	ents, err := os.ReadDir(s.membersDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Member
	for _, e := range ents {
		n := e.Name()
		if filepath.Ext(n) != ".json" {
			continue
		}
		m, err := s.Get(n[:len(n)-len(".json")])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BySession finds the member a session belongs to.
func (s *Store) BySession(session string) (*Member, error) {
	ms, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.Session == session {
			return m, nil
		}
	}
	return nil, fmt.Errorf("session %s: %w", session, ErrNoMember)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
