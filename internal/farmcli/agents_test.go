package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent is a c4-agent that answers find, register, hooks, ls and
// unregister as the real one does, and records what it was asked in calls.
// Its sessions' transcripts are in cd and say they ran in folder; members
// lists the members ls shows, "name session" per line.
func fakeAgent(t *testing.T, cd, folder string, sessions ...string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "c4-agent"), filepath.Join(dir, "calls")
	members := filepath.Join(dir, "members")
	os.WriteFile(members, nil, 0o644)
	os.MkdirAll(filepath.Join(cd, "projects", "x"), 0o755)
	for _, s := range sessions {
		os.WriteFile(filepath.Join(cd, "projects", "x", s+".jsonl"),
			[]byte(`{"type":"permission-mode"}`+"\n"+`{"type":"user","cwd":"`+folder+`","sessionId":"`+s+`"}`+"\n"), 0o644)
	}
	script := `#!/bin/sh
echo "$@" >>` + calls + `
case "$1" in
find) if [ "$3" = scout ]; then printf '` + sessions[0] + `\t` + cd + `/projects/x/` + sessions[0] + `.jsonl\n'; fi ;;
register) echo "$3 $5" >>` + members + `; echo "$3: session $5, phase 0, epoch 0" ;;
hooks) printf '{"hooks":{"PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"BIN checkpoint --hook","timeout":10}]}],"Stop":[{"hooks":[{"type":"command","command":"BIN checkpoint --hook","timeout":30}]}]}}\n' ;;
ls) echo "MEMBER  STATE  SESSION"; while read n s; do echo "$n  idle  $s"; done <` + members + ` ;;
unregister) grep -v "^$3 " ` + members + ` >` + members + `.new; mv ` + members + `.new ` + members + ` ;;
esac
`
	script = strings.ReplaceAll(script, "BIN", bin)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// agents add finds the session by the agent's name, registers it, and puts
// every hook c4-agent prints in the member's own folder's
// settings.local.json, never the global settings: the person's bytes stay
// as written (2>&1 unescaped), the file keeps its mode, and a second add
// changes nothing. rm takes out exactly the hooks.
func TestAddAgentPutsEveryHookInItsFolder(t *testing.T) {
	session := "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10"
	cd, folder := t.TempDir(), t.TempDir()
	bin, calls := fakeAgent(t, cd, folder, session)
	settings := filepath.Join(folder, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	head := `{"model": "opus",
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh 2>&1"}]}]`
	before := head + `},
  "env": {"X": "1"}
}
`
	os.WriteFile(settings, []byte(before), 0o640)

	a := AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "member    scout · session "+session+" · "+folder) ||
		!strings.Contains(out.String(), "installed PostToolUse and Stop") {
		t.Errorf("output:\n%s", out.String())
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "register --name scout --session "+session+" --config-dir "+cd) {
		t.Errorf("c4-agent was asked:\n%s", c)
	}
	b, _ := os.ReadFile(settings)
	s := string(b)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil || !strings.HasPrefix(s, head) || !strings.HasSuffix(s, "},\n  \"env\": {\"X\": \"1\"}\n}\n") ||
		strings.Count(s, bin+" checkpoint --hook") != 2 || strings.Contains(s, "\\u0026") {
		t.Errorf("settings.local.json (%v):\n%s", err, s)
	}
	if fi, _ := os.Stat(settings); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(cd, "settings.json")); !os.IsNotExist(err) {
		t.Error("the global settings.json was written")
	}

	out.Reset()
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "hooks     already in") {
		t.Fatalf("second add: %v\n%s", err, out.String())
	}
	if again, _ := os.ReadFile(settings); string(again) != s {
		t.Error("a second add changed the settings")
	}

	out.Reset()
	if err := RemoveAgent(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "loses PostToolUse and Stop") {
		t.Fatalf("rm: %v\n%s", err, out.String())
	}
	b, _ = os.ReadFile(settings)
	if strings.Contains(string(b), "checkpoint") || !strings.Contains(string(b), `"guard.sh 2>&1"`) || !strings.HasPrefix(string(b), `{"model": "opus",`) {
		t.Errorf("after rm:\n%s", b)
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "unregister --name scout") {
		t.Errorf("c4-agent was asked:\n%s", c)
	}
}

// A second add repairs a hook that is missing; rm leaves the hooks while
// another member runs in the same folder.
func TestAddAgentRepairsAndRmKeepsSharedHooks(t *testing.T) {
	s1, s2 := "1c6f8d63-9b70-4e1f-8c2d-4a3b5f7e9d21", "2d7a9e74-ac81-4f2a-9d3e-5b4c6a8f0e32"
	cd, folder := t.TempDir(), t.TempDir()
	bin, _ := fakeAgent(t, cd, folder, s1, s2)
	settings := filepath.Join(folder, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte(`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+bin+` checkpoint --hook"}]}]}}`), 0o644)
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}); err != nil ||
		!strings.Contains(out.String(), "gains PostToolUse,") {
		t.Fatalf("repair: %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(settings); strings.Count(string(b), "checkpoint --hook") != 2 {
		t.Errorf("repaired:\n%s", b)
	}
	if err := AddAgent(context.Background(), Out{W: &out}, AgentsArgs{Name: "lab", Session: s2, ConfigDir: cd, Yes: true, AgentBin: bin}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := RemoveAgent(context.Background(), Out{W: &out}, AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}); err != nil ||
		!strings.Contains(out.String(), "stay in "+settings+": lab runs there too") {
		t.Fatalf("rm: %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(settings); strings.Count(string(b), "checkpoint --hook") != 2 {
		t.Errorf("shared hooks went:\n%s", b)
	}
}

// add asks only a person at a terminal, and no is the default: an empty
// line or the end of input is no. Without a terminal and without --yes it
// refuses. Either way, nothing is registered or written before a yes.
func TestAddAgentAsksOnlyAtATerminal(t *testing.T) {
	session := "3e8b0f85-bd92-4a3b-8e4f-6c5d7b9a1f43"
	cd, folder := t.TempDir(), t.TempDir()
	bin, calls := fakeAgent(t, cd, folder, session)
	settings := filepath.Join(folder, ".claude", "settings.local.json")
	unchanged := func(when string) {
		t.Helper()
		if c, _ := os.ReadFile(calls); strings.Contains(string(c), "register") {
			t.Errorf("%s: registered before a yes", when)
		}
		if _, err := os.Stat(settings); !os.IsNotExist(err) {
			t.Errorf("%s: settings written before a yes", when)
		}
	}
	a := AgentsArgs{Name: "scout", ConfigDir: cd, AgentBin: bin}
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader("y\n")}, a); err == nil ||
		!strings.Contains(err.Error(), "no one is at a terminal to answer; run it at one, or pass --yes") {
		t.Fatalf("no terminal: %v", err)
	}
	unchanged("no terminal")
	a.Terminal = true
	for _, in := range []string{"", "\n", "n\n"} {
		out.Reset()
		if err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader(in)}, a); err == nil ||
			!strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(out.String(), "and install them? [y/N]") {
			t.Fatalf("answer %q: %v\n%s", in, err, out.String())
		}
		unchanged("answer " + strings.TrimSpace(in))
	}
	if err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader("y\n")}, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings); err != nil {
		t.Error("yes wrote no settings")
	}
	a.Name, a.Session = "nobody", ""
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
		!strings.Contains(lines[2], "85k tokens") || !strings.HasPrefix(lines[3], "4363d63d") || !strings.Contains(lines[3], "1h 30m") ||
		lines[5] != "port needs you: Should I rename the package too?" {
		t.Errorf("agents:\n%s", out.String())
	}
}

// A session that ended a day ago or more folds into one line, which --all
// opens; USED weighs cache reads at the model's rate, not as input.
func TestSessionsFoldOldEndedAndWeighUse(t *testing.T) {
	agent := fakeObserve(t, `[
{"session":"0b5e7c52-8a6f-4d0e","name":"tests","kind":"background","running":true,"word":"working","cwd":"/tmp/repo","startedAt":"2026-10-21T07:30:00Z",
 "usage":{"models":[{"model":"claude-opus-5-5","inputTokens":500,"outputTokens":100000,"cacheReadTokens":40000000,"cacheWriteTokens":400000}]}},
{"session":"5d7e9f74-ac81-4a2b","name":"Teammate availability notification","kind":"background","running":false,"word":"failed","startedAt":"2026-08-07T09:00:00Z","lastActive":"2026-08-08T09:00:00Z"},
{"session":"7f90b196-ce03-4c4d","name":"weighed","kind":"background","running":true,"word":"idle","startedAt":"2026-10-21T08:00:00Z","weighted":1234567,
 "usage":{"models":[{"model":"claude-opus-5-5","inputTokens":1}]}},
{"session":"6e8fa085-bd92-4b3c","name":"yesterday","kind":"background","running":false,"word":"done","startedAt":"2026-10-21T06:00:00Z"}]`, false)
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	o := func(w *bytes.Buffer) Out { return Out{W: w, Clock: Clock{Now: func() time.Time { return now }}} }
	var out bytes.Buffer
	if err := Sessions(context.Background(), o(&out), SessionsArgs{Agent: agent}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// 500 + 5*100k + 1.25*400k + 0.05*40M on Opus 5.5 = 3.0M, not 40.5M
	if strings.Contains(s, "Teammate") || !strings.Contains(s, "yesterday") || !strings.Contains(s, "3.0M tokens") || !strings.Contains(s, "1.2M tokens") ||
		!strings.Contains(s, "1 older session ended 74 days ago; c4 farm agents --all lists them") {
		t.Errorf("agents:\n%s", s)
	}
	out.Reset()
	if err := Sessions(context.Background(), o(&out), SessionsArgs{Agent: agent, All: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Teammate") || strings.Contains(out.String(), "older session") {
		t.Errorf("agents --all:\n%s", out.String())
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
// GET /api/v1/observed, a snapshot the HUD can start on, and nothing of a
// farm; the API answers only with the run's token, only on loopback, and
// carries no transcript path and no folder above the home folder.
func TestSessionsServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agent := fakeObserve(t, `[{"session":"0b5e7c52-8a6f","name":"port","kind":"background","running":true,"word":"needs you","needsYou":"Rename it too?","backgroundOpen":0,"cwd":"`+home+`/repo","transcript":"`+home+`/.claude/projects/x/0b5e.jsonl"}]`, false)
	h, err := SessionsServer{Agent: agent, Token: "s3cret"}.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	get := func(path string) (int, string) {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/api/v1/observed"); code != 403 {
		t.Fatalf("observed without the token: %d", code)
	}
	if code, body := get("/?token=s3cret"); code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("index with the token %d", code)
	}
	// the cookie the page load set
	code, body := get("/api/v1/observed")
	if code != 200 || !strings.Contains(body, `"word":"needs you"`) || !strings.Contains(body, `"needsYou":"Rename it too?"`) ||
		!strings.Contains(body, `"cwd":"~/repo"`) || strings.Contains(body, "transcript") || strings.Contains(body, home) {
		t.Errorf("observed %d %s", code, body)
	}
	if code, body := get("/api/v1/snapshot"); code != 200 || !strings.Contains(body, `"jobs":[]`) || !strings.Contains(body, `"allProjects":true`) {
		t.Errorf("snapshot %d %s", code, body)
	}
	if code, _ := get("/api/v1/jobs"); code != 404 {
		t.Errorf("a farm route answered %d", code)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/observed?token=s3cret", nil)
	req.Host = "evil.example:80"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("another host name: %v %v", err, resp)
	}
}
