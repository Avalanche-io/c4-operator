package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// A session as a view. A block's session history travels between machines
// (a host runner, a unit pod) as part of its unit's output view: the
// directory SessionDir in the output root holds the transcript and the
// member's record and checkpoint log, so the next attempt, wherever it
// runs, mounts the view and resumes the same session by its ID with its
// checkpoint chain intact. The chain is over the transcript's bytes, not
// its path, so it verifies on any machine (checkpoint.go).
//
//	<out>/c4-session/session.json                  {member, session, cwd}
//	<out>/c4-session/transcript.jsonl              the session's transcript
//	<out>/c4-session/agents/members/<member>.json  its member record
//	<out>/c4-session/agents/members/<member>.checkpoints.jsonl
//
// The credential never travels: it stays in the holder's Claude Code
// store, which is not part of any view.
const SessionDir = "c4-session"

// SessionInfo is <out>/c4-session/session.json.
type SessionInfo struct {
	Member  string `json:"member"`
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
}

// ExportSession writes a member's session into out's SessionDir: its
// transcript as it is now, its member record and its checkpoint log.
func ExportSession(st *Store, m *Member, out string) error {
	dir := filepath.Join(out, SessionDir)
	if err := os.MkdirAll(filepath.Join(dir, "agents", "members"), 0o755); err != nil {
		return err
	}
	if err := copyFile(m.Transcript, filepath.Join(dir, "transcript.jsonl")); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	dst := &Store{Dir: filepath.Join(dir, "agents")}
	if filepath.Clean(dst.Dir) != filepath.Clean(st.Dir) {
		if err := copyFile(st.memberPath(m.Name), dst.memberPath(m.Name)); err != nil {
			return err
		}
		if err := copyFile(st.logPath(m.Name), dst.logPath(m.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	info, _ := json.MarshalIndent(SessionInfo{Member: m.Name, Session: m.Session, Cwd: m.Cwd}, "", "  ")
	return os.WriteFile(filepath.Join(dir, "session.json"), append(info, '\n'), 0o644)
}

// ReadSessionInfo reads out's SessionDir, if there is one.
func ReadSessionInfo(out string) (*SessionInfo, error) {
	b, err := os.ReadFile(filepath.Join(out, SessionDir, "session.json"))
	if err != nil {
		return nil, err
	}
	var si SessionInfo
	if err := json.Unmarshal(b, &si); err != nil {
		return nil, err
	}
	if !ValidSession(si.Session) || si.Member == "" {
		return nil, fmt.Errorf("session.json: member %q, session %q", si.Member, si.Session)
	}
	return &si, nil
}

// ImportSession takes a session exported to out into this machine: its
// transcript goes where this machine's Claude Code looks for it (configDir,
// the session's cwd), and its member record and checkpoint log into st,
// rewritten for this machine's paths. The chain carries on from where the
// other machine left it. It refuses a session whose member is live here.
func (h *Harness) ImportSession(ctx context.Context, out, configDir string) (*Member, error) {
	si, err := ReadSessionInfo(out)
	if err != nil {
		return nil, err
	}
	src := &Store{Dir: filepath.Join(out, SessionDir, "agents")}
	m, err := src.Get(si.Member)
	if err != nil {
		return nil, err
	}
	unlock, err := h.Store.Lock(m.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if cur, err := h.Store.Get(m.Name); err == nil {
		if live, a, err := h.live(ctx, cur); err != nil {
			return nil, err
		} else if live {
			return nil, fmt.Errorf("member %s: session %s is %s here: %w; one live copy only", m.Name, cur.Session, a.Status, ErrLive)
		}
		m.Epoch = max(m.Epoch, cur.Epoch)
	}
	cd, err := ConfigDirOrDefault(configDir)
	if err != nil {
		return nil, err
	}
	m.ConfigDir, m.Cwd = configDir, si.Cwd
	m.Transcript = filepath.Join(cd, "projects", ProjectSlug(si.Cwd), si.Session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(m.Transcript), 0o700); err != nil {
		return nil, err
	}
	if err := copyFile(filepath.Join(out, SessionDir, "transcript.jsonl"), m.Transcript); err != nil {
		return nil, fmt.Errorf("transcript: %w", err)
	}
	if err := os.MkdirAll(h.Store.membersDir(), 0o700); err != nil {
		return nil, err
	}
	if filepath.Clean(src.Dir) != filepath.Clean(h.Store.Dir) {
		if err := copyFile(src.logPath(m.Name), h.Store.logPath(m.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := h.Store.Put(m); err != nil {
		return nil, err
	}
	return m, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ViewStore stores a view's objects and records in a project's store: a
// tier.Uploader to a regional replica.
type ViewStore interface {
	PutFile(ctx context.Context, project string, id c4.ID, path string) error
	PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error
}

// CaptureView stores dir as a view, the way a unit's output is captured,
// and returns its C4 ID: what c4.avalanche.io/output-tree names. Control
// documents (.c4/) are left out, as the node daemon leaves them out.
func CaptureView(ctx context.Context, vs ViewStore, project, dir string) (c4.ID, error) {
	stage, err := os.MkdirTemp("", "c4-view-")
	if err != nil {
		return c4.ID{}, err
	}
	defer os.RemoveAll(stage)
	// a copy without .c4/, so the view is what a node would capture
	if err := copyTree(dir, stage, func(rel string) bool { return rel == ".c4" }); err != nil {
		return c4.ID{}, err
	}
	v, err := view.ScanDir(stage)
	if err != nil {
		return c4.ID{}, err
	}
	paths := map[c4.ID]string{}
	for _, f := range v.Files {
		if _, ok := paths[f.ID]; !ok {
			paths[f.ID] = filepath.Join(stage, filepath.FromSlash(f.Path))
		}
	}
	for _, o := range v.Objects() {
		if err := vs.PutFile(ctx, project, o.ID, paths[o.ID]); err != nil {
			return c4.ID{}, fmt.Errorf("object %s: %w", o.ID, err)
		}
	}
	for _, r := range v.Records() {
		if err := vs.PutBytes(ctx, project, r.ID, r.Data); err != nil {
			return c4.ID{}, fmt.Errorf("record %s: %w", r.ID, err)
		}
	}
	return v.ID, nil
}

// ViewSource reads a project's objects and records: a tier.HTTPSource to a
// regional replica.
type ViewSource interface {
	Get(ctx context.Context, project string, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error)
}

type viewStore struct {
	ctx     context.Context
	src     ViewSource
	project string
}

func (s viewStore) Open(id c4.ID) (io.ReadCloser, error) {
	rc, _, _, err := s.src.Get(s.ctx, s.project, id, 0)
	return rc, err
}

// FetchView writes view id into dir (which it creates): the host's way of
// mounting a unit's resume view.
func FetchView(ctx context.Context, src ViewSource, project string, id c4.ID, dir string) error {
	st := viewStore{ctx: ctx, src: src, project: project}
	r, err := view.FromID(id, st, view.Filter{})
	if err != nil {
		return err
	}
	v := r.View()
	for _, d := range v.Dirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
			return err
		}
	}
	for _, f := range v.Files {
		data, err := view.ReadObject(st, f.ID)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		mode := os.FileMode(0o644)
		if f.Exec {
			mode = 0o755
		}
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
	}
	for _, l := range v.Symlinks {
		p := filepath.Join(dir, filepath.FromSlash(l.Path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(l.Target, p); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// copyTree copies regular files and directories from src to dst, leaving
// out the top-level entries skip names.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if skip(filepath.ToSlash(rel)) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, 0o755)
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, fi.Mode().Perm())
		}
		return nil
	})
}
