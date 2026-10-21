package farmcli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent is a c4-agent that answers find, register, hooks and ls as the
// real one does, and records what it was asked in calls.
func fakeAgent(t *testing.T, session string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "c4-agent"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >>` + calls + `
case "$1" in
find) if [ "$3" = scout ]; then printf '` + session + `\t/cfg/projects/x/` + session + `.jsonl\n'; fi ;;
register) echo "$3: session $5, phase 0, epoch 0, transcript /cfg/projects/x/$5.jsonl" ;;
hooks) printf '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + "BIN" + ` checkpoint --hook","timeout":30}]}]}}\n' ;;
ls) printf 'MEMBER  STATE  SESSION\nscout   idle   ` + session + `\n' ;;
esac
`
	script = strings.ReplaceAll(script, "BIN", bin)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// agents add finds the session by the agent's name, registers it through
// c4-agent, and merges c4-agent's Stop hook into settings.json once,
// keeping the person's keys and hooks in their order; a second add
// changes nothing.
func TestAddAgentRegistersAndInstallsTheHookOnce(t *testing.T) {
	session := "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10"
	bin, calls := fakeAgent(t, session)
	cd := t.TempDir()
	settings := filepath.Join(cd, "settings.json")
	before := `{"model": "opus", "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]}, "env": {"X": "1"}}`
	os.WriteFile(settings, []byte(before), 0o644)

	a := AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "member    scout · session "+session) || !strings.Contains(out.String(), "installed the hook") {
		t.Errorf("output:\n%s", out.String())
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "find --agent-name scout --config-dir "+cd) ||
		!strings.Contains(string(c), "register --name scout --session "+session+" --config-dir "+cd) {
		t.Errorf("c4-agent was asked:\n%s", c)
	}
	b, _ := os.ReadFile(settings)
	s := string(b)
	model, hooks, env := strings.Index(s, `"model"`), strings.Index(s, `"hooks"`), strings.Index(s, `"env"`)
	if model < 0 || hooks < model || env < hooks || !strings.Contains(s, "guard.sh") ||
		!strings.Contains(s, `"command": "`+bin+` checkpoint --hook"`) || strings.Index(s, `"Stop"`) < strings.Index(s, `"PreToolUse"`) {
		t.Errorf("settings.json:\n%s", s)
	}
	if bk, _ := os.ReadFile(settings + ".c4-backup"); string(bk) != before {
		t.Errorf("backup %q", bk)
	}

	out.Reset()
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "hook      already in") {
		t.Fatalf("second add: %v\n%s", err, out.String())
	}
	if again, _ := os.ReadFile(settings); string(again) != s {
		t.Error("a second add changed settings.json")
	}
	out.Reset()
	if err := ListAgents(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "scout   idle") {
		t.Errorf("ls: %v\n%s", err, out.String())
	}
}

// Declining the hook keeps the member, leaves settings.json as it was, and
// says how to install it later; an agent no session names is refused with
// the flag that names one.
func TestAddAgentAsksBeforeTheHook(t *testing.T) {
	session := "1c6f8d63-9b70-4e1f-8c2d-4a3b5f7e9d21"
	bin, _ := fakeAgent(t, session)
	cd := t.TempDir()
	a := AgentsArgs{Name: "lab", Session: session, ConfigDir: cd, AgentBin: bin}
	var out bytes.Buffer
	err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader("n\n")}, a)
	if err == nil || !strings.Contains(err.Error(), "c4 farm agents add again installs it") || !strings.Contains(out.String(), "Install it? [Y/n]") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(cd, "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json was written after no")
	}
	a.Session = ""
	if err := AddAgent(context.Background(), Out{W: &out}, a); err == nil || !strings.Contains(err.Error(), "--session ID") {
		t.Errorf("no session: %v", err)
	}
}

// fakeObserve is a c4-agent whose observe prints rows; old makes it one too
// old to observe, which says so on stderr as c4-agent does.
func fakeObserve(t *testing.T, rows string, old bool) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "c4-agent")
	script := "#!/bin/sh\n[ \"$1\" = observe ] || exit 2\n"
	if old {
		script += "echo 'c4-agent: unknown command \"observe\" (try c4-agent help)' >&2\nexit 1\n"
	} else {
		script += "cat <<'X'\n{\"observedAt\":\"2026-10-21T09:00:00Z\",\"sessions\":" + rows + "}\nX\n"
	}
	os.WriteFile(bin, []byte(script), 0o755)
	return bin
}

// c4 farm agents, with nothing after it, shows this machine's sessions as
// c4-agent observe reads them: what needs the person first, each state in
// a word, what it has used, and what each waiting session asks.
func TestSessionsShowsWhatNeedsYou(t *testing.T) {
	agent := fakeObserve(t, `[
{"session":"0b5e7c52-8a6f-4d0e","name":"tests","kind":"background","running":true,"word":"working","cwd":"/tmp/repo","startedAt":"2026-10-21T07:30:00Z",
 "usage":{"models":[{"model":"claude-opus-5-5","inputTokens":30000,"outputTokens":11000}]}},
{"session":"1c6f8d63-9b70-4e1f","name":"port","kind":"background","running":true,"word":"needs you","needsYou":"Should I rename the package too?","cwd":"/tmp/repo","startedAt":"2026-10-21T07:30:00Z"},
{"session":"4363d63d-ee8a-4f04","kind":"interactive","running":true,"word":"idle","cwd":"/tmp","startedAt":"2026-10-21T07:30:00Z"}]`, false)
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	o := Out{W: &out, Clock: Clock{Now: func() time.Time { return now }}}
	if err := Sessions(context.Background(), o, SessionsArgs{Agent: agent}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[1], "port ") || !strings.Contains(lines[1], "needs you") ||
		!strings.Contains(lines[2], "41k tokens") || !strings.HasPrefix(lines[3], "4363d63d") || !strings.Contains(lines[3], "1h 30m") ||
		lines[5] != "port needs you: Should I rename the package too?" {
		t.Errorf("agents:\n%s", out.String())
	}
}

// With a c4-agent too old to observe, c4 farm agents reads claude agents
// itself: states, without questions or usage.
func TestSessionsFallBackToClaude(t *testing.T) {
	claude := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(claude, []byte("#!/bin/sh\necho '[{\"name\":\"port\",\"sessionId\":\"1c6f8d63\",\"cwd\":\"/tmp\",\"kind\":\"background\",\"status\":\"idle\",\"state\":\"blocked\",\"startedAt\":1792534445663}]'\n"), 0o755)
	var out bytes.Buffer
	a := SessionsArgs{Agent: fakeObserve(t, "", true), Claude: claude}
	if err := Sessions(context.Background(), Out{W: &out}, a); err != nil || !strings.HasSuffix(out.String(), "\nport needs you.\n") {
		t.Errorf("%v\n%s", err, out.String())
	}
}

// agents --open serves the HUD's web files, this machine's sessions as
// GET /api/v1/observed (each row as c4-agent observe gives it), a snapshot
// the HUD can start on, and nothing of a farm.
func TestSessionsServer(t *testing.T) {
	agent := fakeObserve(t, `[{"session":"0b5e7c52-8a6f","name":"port","kind":"background","running":true,"word":"needs you","needsYou":"Rename it too?","backgroundOpen":0}]`, false)
	h, err := SessionsServer{Agent: agent}.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/api/v1/observed"); code != 200 || !strings.Contains(body, `"word":"needs you"`) ||
		!strings.Contains(body, `"needsYou":"Rename it too?"`) || !strings.Contains(body, `"backgroundOpen":0`) || !strings.Contains(body, `"machine":`) {
		t.Errorf("observed %d %s", code, body)
	}
	if code, body := get("/api/v1/snapshot"); code != 200 || !strings.Contains(body, `"jobs":[]`) || !strings.Contains(body, `"allProjects":true`) {
		t.Errorf("snapshot %d %s", code, body)
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, "<html") {
		t.Errorf("index %d", code)
	}
	if code, _ := get("/api/v1/jobs"); code != 404 {
		t.Errorf("a farm route answered %d", code)
	}
}
