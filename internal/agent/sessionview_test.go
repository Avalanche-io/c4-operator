package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

// memStore is a project store in memory: a ViewStore and a ViewSource.
type memStore struct {
	mu   sync.Mutex
	objs map[c4.ID][]byte
}

func (m *memStore) PutFile(ctx context.Context, project string, id c4.ID, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return m.PutBytes(ctx, project, id, b)
}

func (m *memStore) PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error {
	if got := c4.Identify(bytes.NewReader(data)); got != id {
		return fmt.Errorf("stored as %s, is %s", id, got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objs == nil {
		m.objs = map[c4.ID][]byte{}
	}
	m.objs[id] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) Get(ctx context.Context, project string, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[id]
	if !ok {
		return nil, 0, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b[offset:])), int64(len(b)), int64(len(b)), nil
}

// A session moves between machines as a view: exported into an output
// root on one, captured, fetched on another with different paths (another
// Claude Code config dir, another agents home), imported, and continued.
// Its checkpoint chain verifies on the new machine and carries on from
// where the first left it, and a member live on the new machine refuses
// the import: one live copy only.
func TestSessionMovesAsAView(t *testing.T) {
	ctx := context.Background()
	turn := func(path, text string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(f, `{"type":"assistant","message":{"id":%q,"model":"opus","content":[{"type":"text","text":%q}]}}`+"\n", text, text)
		f.Close()
	}
	// machine A: a member with two turns checkpointed
	cfgA, cwd := t.TempDir(), "/private/tmp"
	session := "11111111-2222-4333-8444-555555555555"
	trA := filepath.Join(cfgA, "projects", ProjectSlug(cwd), session+".jsonl")
	os.MkdirAll(filepath.Dir(trA), 0o700)
	turn(trA, "part 1")
	a := New(&Store{Dir: t.TempDir()}, newFake())
	a.SettleQuiet, a.SettleMax = time.Millisecond, 50*time.Millisecond
	ma := &Member{Name: "practice.move.notes", Session: session, Cwd: cwd, ConfigDir: cfgA, Transcript: trA, Block: true}
	if err := a.Store.Put(ma); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Checkpoint(ctx, ma.Name, "stop"); err != nil {
		t.Fatal(err)
	}
	turn(trA, "part 2")
	cpA, _, err := a.Checkpoint(ctx, ma.Name, "stop")
	if err != nil {
		t.Fatal(err)
	}

	// A exports and captures; B fetches the view by its C4 ID
	outA := t.TempDir()
	os.MkdirAll(filepath.Join(outA, ".c4"), 0o755)
	os.WriteFile(filepath.Join(outA, ".c4", "safepoint"), []byte("seq 1\n"), 0o644)
	os.WriteFile(filepath.Join(outA, "notes.md"), []byte("part 1\npart 2\n"), 0o644)
	if err := ExportSession(a.Store, ma, outA); err != nil {
		t.Fatal(err)
	}
	st := &memStore{}
	id, err := CaptureView(ctx, st, "practice", outA)
	if err != nil {
		t.Fatal(err)
	}
	outB := filepath.Join(t.TempDir(), "out")
	if err := FetchView(ctx, st, "practice", id, outB); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outB, "notes.md")); string(b) != "part 1\npart 2\n" {
		t.Fatalf("the work did not travel: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outB, ".c4")); err == nil {
		t.Error("control documents travelled in the view")
	}

	// machine B: another config dir and agents home
	cfgB := t.TempDir()
	fb := newFake()
	b := New(&Store{Dir: t.TempDir()}, fb)
	b.SettleQuiet, b.SettleMax = time.Millisecond, 50*time.Millisecond
	mb, err := b.ImportSession(ctx, outB, cfgB)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mb.Transcript, cfgB) || mb.Session != session || !mb.Block {
		t.Fatalf("imported %+v", mb)
	}
	if r, err := b.Store.Verify(mb.Name); err != nil || r.Broken != 0 || r.Checked != 2 {
		t.Fatalf("the chain on B: %+v %v", r, err)
	}
	turn(mb.Transcript, "part 3")
	cpB, _, err := b.Checkpoint(ctx, mb.Name, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if cpB.Seq != cpA.Seq+1 || cpB.Note != "" {
		t.Fatalf("B's checkpoint %+v does not carry on from A's %+v", cpB, cpA)
	}
	if r, err := b.Store.Verify(mb.Name); err != nil || r.Broken != 0 || r.Checked != 3 {
		t.Fatalf("the chain after a turn on B: %+v %v", r, err)
	}

	// one live copy: B refuses an import while the session runs there
	fb.mu.Lock()
	fb.active[session] = Active{SessionID: session, Status: "idle"}
	fb.mu.Unlock()
	if _, err := b.ImportSession(ctx, outB, cfgB); err == nil {
		t.Fatal("imported over a live session")
	}
}
