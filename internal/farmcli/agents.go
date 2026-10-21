package farmcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// AgentsArgs are `c4 farm agents add` and `rm`'s.
type AgentsArgs struct {
	Name string
	// Session is the Claude Code session to add; empty finds the newest
	// whose transcript names the agent Name.
	Session string
	// ConfigDir is the Claude Code config dir the session is in (default
	// $CLAUDE_CONFIG_DIR, else ~/.claude).
	ConfigDir string
	// Folder is the member's own folder, whose .claude/settings.local.json
	// gets the hooks (default the session's working folder).
	Folder string
	// Yes changes settings without asking; without it, add and rm ask,
	// and only at a terminal.
	Yes bool
	// Terminal says the person is at a terminal to answer.
	Terminal bool
	// AgentBin is c4-agent, whose CLI keeps the members (default
	// c4-agent on PATH).
	AgentBin string
}

// hookCommand is what marks c4-agent's hooks in a settings file.
const hookCommand = "checkpoint --hook"

// hookWhy is what each of c4-agent's hooks does, as add explains it.
var hookWhy = map[string]string{
	"Stop":        "saves each turn the member ends",
	"PostToolUse": "ends a turn at the step the farm asks it to stop",
}

// isC4Hook says a hook command is c4-agent's checkpoint.
func isC4Hook(cmd string) bool {
	return strings.Contains(cmd, "c4-agent") && strings.Contains(cmd, hookCommand)
}

// agentBin is c4-agent, by its absolute path, for the hook to name.
func (a *AgentsArgs) agentBin() (string, error) {
	if a.AgentBin != "" {
		return a.AgentBin, nil
	}
	p, err := exec.LookPath("c4-agent")
	if err != nil {
		return "", errors.New("c4-agent is not on PATH; brew install mrjoshuak/tap/c4 installs it beside c4 farm")
	}
	return filepath.Abs(p)
}

func (a *AgentsArgs) configDir() (string, error) {
	if a.ConfigDir != "" {
		return a.ConfigDir, nil
	}
	if cd := os.Getenv("CLAUDE_CONFIG_DIR"); cd != "" {
		return cd, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func (a *AgentsArgs) cdArgs() []string {
	if a.ConfigDir == "" {
		return nil
	}
	return []string{"--config-dir", a.ConfigDir}
}

// agentCmd runs c4-agent and returns what it printed; its own message is
// the error when it fails.
func agentCmd(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("c4-agent %s: %s", args[0], msg)
	}
	return string(out), nil
}

// ask is a yes-or-no question whose default is no: an empty line, end of
// input or anything but yes is no.
func ask(o Out, q string) bool {
	o.printf("%s [y/N] ", q)
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// agree is the person's leave to change settings: --yes, or yes at a
// terminal. Without a terminal, nobody is there to agree, so it is no.
func (a *AgentsArgs) agree(o Out, q, refused string) error {
	switch {
	case a.Yes:
		return nil
	case !a.Terminal || o.In == nil:
		return fmt.Errorf("%s: c4 farm agents asks before it changes settings, and no one is at a terminal to answer; run it at one, or pass --yes", refused)
	case !ask(o, q):
		return fmt.Errorf("%s: nothing was changed", refused)
	}
	return nil
}

// findSession is the newest session whose transcript names the agent,
// with its transcript.
func findSession(ctx context.Context, bin string, a *AgentsArgs) (session, transcript string, err error) {
	out, err := agentCmd(ctx, bin, append([]string{"find", "--agent-name", a.Name}, a.cdArgs()...)...)
	if err != nil {
		return "", "", err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	session, transcript, _ = strings.Cut(first, "\t")
	if session == "" {
		return "", "", fmt.Errorf("no Claude Code session names the agent %q; give its session with --session ID (claude --resume lists them)", a.Name)
	}
	return session, transcript, nil
}

// transcriptOf is a session's transcript in the config dir, if any.
func transcriptOf(configDir, session string) string {
	m, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", session+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// folderOf is the working folder a transcript's session ran in.
func folderOf(transcript string) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<20))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var line struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Cwd != "" {
			return line.Cwd
		}
	}
	return ""
}

// memberFolder is the folder whose settings get the member's hooks.
func (a *AgentsArgs) memberFolder(transcript string) (string, error) {
	if a.Folder != "" {
		return filepath.Abs(a.Folder)
	}
	if dir := folderOf(transcript); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("could not tell which folder the session of %q runs in; give it with --folder DIR", a.Name)
}

// localSettings is a folder's Claude Code settings that are this
// machine's alone: the hooks go there, never into the global settings,
// so they run only in sessions started in that folder.
func localSettings(folder string) string {
	return filepath.Join(folder, ".claude", "settings.local.json")
}

// AddAgent is `c4 farm agents add NAME`: the Claude Code session becomes
// a member the farm can run (c4-agent register), and its folder's
// .claude/settings.local.json gets every hook c4-agent hooks prints: Stop,
// which saves each turn the member ends, and PostToolUse, which ends a
// turn at the step the farm asked it to stop. What changes is shown and
// asked first, at a terminal, and nothing is registered or written before
// the answer. Both are idempotent, and a second add repairs a hook that is
// missing.
func AddAgent(ctx context.Context, o Out, a AgentsArgs) error {
	if a.Name == "" {
		return errors.New("give the agent's name: c4 farm agents add NAME")
	}
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	cd, err := a.configDir()
	if err != nil {
		return err
	}
	session, transcript := a.Session, ""
	if session == "" {
		if session, transcript, err = findSession(ctx, bin, &a); err != nil {
			return err
		}
	} else {
		transcript = transcriptOf(cd, session)
	}
	folder, err := a.memberFolder(transcript)
	if err != nil {
		return err
	}
	frag, err := agentCmd(ctx, bin, "hooks")
	if err != nil {
		return err
	}
	settings := localSettings(folder)
	b, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, added, err := withHooks(b, []byte(frag))
	if err != nil {
		return fmt.Errorf("%s: %w", settings, err)
	}
	o.printf("member    %s · session %s · %s\n", a.Name, session, folder)
	if len(added) == 0 {
		o.printf("hooks     already in %s\n", settings)
	} else {
		o.printf("hooks     %s gains %s, for sessions started in %s:\n", settings, andList(added), folder)
		for _, ev := range added {
			if why, ok := hookWhy[ev]; ok {
				o.printf("          %s %s\n", ev, why)
			}
		}
		if err := a.agree(o, "Register "+a.Name+" and install them?", a.Name+" is not added"); err != nil {
			return err
		}
	}
	if _, err := agentCmd(ctx, bin, append([]string{"register", "--name", a.Name, "--session", session}, a.cdArgs()...)...); err != nil {
		return err
	}
	if len(added) > 0 {
		if err := writeAtomic(settings, merged); err != nil {
			return err
		}
		o.printf("installed %s; c4 farm agents rm %s takes them out\n", andList(added), a.Name)
	}
	o.printf("added     %s\n", a.Name)
	return nil
}

// PickAgent is `c4 farm agents add` with no name, at a terminal: the
// running sessions on this machine that are not agents yet, numbered, and
// the person's pick, named as the session is (or as the person names it).
// It fills Name and Session for AddAgent, which asks its own question
// after. Nothing is changed here.
func PickAgent(ctx context.Context, o *Out, a *AgentsArgs) error {
	if !a.Terminal || o.In == nil {
		return errors.New("give the agent's name: c4 farm agents add NAME (at a terminal, c4 farm agents add alone lists the sessions to pick from)")
	}
	// one reader for every answer, so none is lost to another's buffer
	o.In = bufio.NewReader(o.In)
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	rows, _, err := readSessions(ctx, SessionsArgs{Agent: bin})
	if err != nil {
		return err
	}
	members, err := listMembers(ctx, bin)
	if err != nil {
		return err
	}
	isMember := map[string]bool{}
	for _, s := range members {
		isMember[s] = true
	}
	var pick []row
	for _, r := range rows {
		if !r.ended() && !isMember[r.Session] {
			pick = append(pick, r)
		}
	}
	if len(pick) == 0 {
		return errors.New("no running Claude Code session here that is not an agent already; start one with claude, then c4 farm agents add")
	}
	home, _ := os.UserHomeDir()
	for i, r := range pick {
		dir := r.Cwd
		if home != "" && strings.HasPrefix(dir, home+string(filepath.Separator)) {
			dir = "~" + dir[len(home):]
		}
		o.printf("%3d  %s · %s · %s\n", i+1, r.label(), r.Word, dir)
	}
	o.printf("Which one? [1-%d] ", len(pick))
	line, _ := o.In.(*bufio.Reader).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(pick) {
		return errors.New("no session picked: nothing was changed")
	}
	r := pick[n-1]
	a.Session, a.Name = r.Session, r.Name
	if a.Name == "" {
		o.printf("Name it: ")
		line, _ := o.In.(*bufio.Reader).ReadString('\n')
		if a.Name = strings.TrimSpace(line); a.Name == "" {
			return errors.New("no name given: nothing was changed")
		}
	}
	return nil
}

// RemoveAgent is `c4 farm agents rm NAME`: what add added, taken out. The
// hooks leave the member's folder only when no other member runs there,
// since they serve every member in that folder.
func RemoveAgent(ctx context.Context, o Out, a AgentsArgs) error {
	if a.Name == "" {
		return errors.New("give the agent's name: c4 farm agents rm NAME")
	}
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	cd, err := a.configDir()
	if err != nil {
		return err
	}
	members, err := listMembers(ctx, bin)
	if err != nil {
		return err
	}
	session, ok := members[a.Name]
	if !ok {
		return fmt.Errorf("%s is not an agent here; c4 farm agents ls lists them", a.Name)
	}
	folder, err := a.memberFolder(transcriptOf(cd, session))
	if err != nil {
		return err
	}
	var others []string
	for name, s := range members {
		if name != a.Name && folderOf(transcriptOf(cd, s)) == folder {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	settings := localSettings(folder)
	b, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	stripped, removed, err := withoutHooks(b)
	if err != nil {
		return fmt.Errorf("%s: %w", settings, err)
	}
	switch {
	case len(removed) == 0:
		o.printf("hooks     none of c4-agent's in %s\n", settings)
	case len(others) > 0:
		o.printf("hooks     stay in %s: %s runs there too\n", settings, andList(others))
	default:
		o.printf("hooks     %s loses %s\n", settings, andList(removed))
	}
	if err := a.agree(o, "Remove "+a.Name+"?", a.Name+" is not removed"); err != nil {
		return err
	}
	if len(removed) > 0 && len(others) == 0 {
		if err := writeAtomic(settings, stripped); err != nil {
			return err
		}
	}
	_, err = agentCmd(ctx, bin, "unregister", "--name", a.Name)
	switch {
	case err != nil && strings.Contains(err.Error(), "unknown command"):
		o.printf("member    %s stays in c4-agent's list: this c4-agent cannot remove a member yet\n", a.Name)
	case err != nil:
		return err
	default:
		o.printf("removed   %s\n", a.Name)
	}
	return nil
}

// listMembers are c4-agent ls's members by name, with their sessions.
func listMembers(ctx context.Context, bin string) (map[string]string, error) {
	out, err := agentCmd(ctx, bin, "ls")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines[min(1, len(lines)):] {
		if f := strings.Fields(l); len(f) >= 3 {
			m[f[0]] = f[2]
		}
	}
	return m, nil
}

// writeAtomic replaces a file whole: a session reading it sees the old
// file or the new, never half of one. The file keeps its mode.
func writeAtomic(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// withHooks is a settings file with each of c4-agent's hook groups (the
// fragment c4-agent hooks prints) that is not there already, and the
// events added. Only the hooks are edited: every other byte stays as
// written.
func withHooks(settings, frag []byte) ([]byte, []string, error) {
	var want struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(frag, &want); err != nil || len(want.Hooks) == 0 {
		return nil, nil, errors.New("c4-agent hooks printed no hooks")
	}
	events := make([]string, 0, len(want.Hooks))
	for ev := range want.Hooks {
		events = append(events, ev)
	}
	sort.Strings(events)
	doc := settings
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = []byte("{}\n")
	}
	var have struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(doc, &have); err != nil {
		return nil, nil, errors.New("not a JSON object with a hooks object")
	}
	var added []string
	for _, ev := range events {
		if hasC4Hook(have.Hooks[ev]) {
			continue
		}
		var err error
		for _, g := range want.Hooks[ev] {
			if doc, err = addGroup(doc, ev, g); err != nil {
				return nil, nil, err
			}
		}
		added = append(added, ev)
	}
	return doc, added, nil
}

// withoutHooks is a settings file with c4-agent's hook groups taken out,
// and the events they were in; other bytes stay as written, but for the
// hooks, which are written again.
func withoutHooks(settings []byte) ([]byte, []string, error) {
	if len(bytes.TrimSpace(settings)) == 0 {
		return settings, nil, nil
	}
	var have struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &have); err != nil {
		return nil, nil, errors.New("not a JSON object with a hooks object")
	}
	var removed []string
	kept := map[string][]json.RawMessage{}
	for ev, groups := range have.Hooks {
		var k []json.RawMessage
		for _, raw := range groups {
			var g hookGroup
			json.Unmarshal(raw, &g)
			if hasC4Hook([]hookGroup{g}) {
				continue
			}
			k = append(k, raw)
		}
		if len(k) < len(groups) {
			removed = append(removed, ev)
		}
		if len(k) > 0 {
			kept[ev] = k
		}
	}
	if len(removed) == 0 {
		return settings, nil, nil
	}
	sort.Strings(removed)
	start, end, ok, err := memberSpan(settings, "hooks")
	if err != nil || !ok {
		return nil, nil, errors.New("hooks is not where it was read")
	}
	hb, err := marshalPlain(kept)
	if err != nil {
		return nil, nil, err
	}
	out := append(append(append([]byte{}, settings[:start]...), hb...), settings[end:]...)
	return out, removed, nil
}

type hookGroup struct {
	Hooks []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

func hasC4Hook(groups []hookGroup) bool {
	for _, g := range groups {
		for _, h := range g.Hooks {
			if isC4Hook(h.Command) {
				return true
			}
		}
	}
	return false
}

// marshalPlain is JSON without HTML escaping, so a command's 2>&1 stays
// as written.
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// addGroup puts a hook group at the end of the event's list in hooks,
// making the list or hooks itself when missing, and leaving every other
// byte of doc as it is.
func addGroup(doc []byte, event string, group json.RawMessage) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, group); err != nil {
		return nil, err
	}
	g := compact.Bytes()
	hs, he, ok, err := memberSpan(doc, "hooks")
	if err != nil {
		return nil, err
	}
	if !ok {
		v, _ := marshalPlain(map[string][]json.RawMessage{event: {g}})
		return insertMember(doc, 0, len(doc), "hooks", v)
	}
	hooks := doc[hs:he]
	es, ee, ok, err := memberSpan(hooks, event)
	if err != nil {
		return nil, errors.New("hooks is not an object")
	}
	var nh []byte
	if !ok {
		v, _ := marshalPlain([]json.RawMessage{g})
		if nh, err = insertMember(hooks, 0, len(hooks), event, v); err != nil {
			return nil, err
		}
	} else {
		arr := hooks[es:ee]
		na, err := appendElem(arr, g)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s is not a list", event)
		}
		nh = append(append(append([]byte{}, hooks[:es]...), na...), hooks[ee:]...)
	}
	return append(append(append([]byte{}, doc[:hs]...), nh...), doc[he:]...), nil
}

// memberSpan is where the value of an object's top-level key lies in doc,
// an object.
func memberSpan(doc []byte, key string) (start, end int, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return 0, 0, false, errors.New("not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		if k, _ := kt.(string); k == key {
			end := int(dec.InputOffset())
			return end - len(raw), end, true, nil
		}
	}
	return 0, 0, false, nil
}

// insertMember adds "key": value as the last member of the object in
// doc[start:end].
func insertMember(doc []byte, start, end int, key string, value []byte) ([]byte, error) {
	obj := doc[start:end]
	i := bytes.LastIndexByte(obj, '}')
	if i < 0 {
		return nil, errors.New("not a JSON object")
	}
	kb, _ := marshalPlain(key)
	member := append(append(kb, ": "...), value...)
	inner := bytes.TrimSpace(obj[bytes.IndexByte(obj, '{')+1 : i])
	var ins []byte
	if len(inner) == 0 {
		ins = member
	} else {
		ins = append([]byte(", "), member...)
	}
	// after the last member, before the whitespace that closes the object
	j := i
	for j > 0 && (obj[j-1] == ' ' || obj[j-1] == '\n' || obj[j-1] == '\t' || obj[j-1] == '\r') {
		j--
	}
	if len(inner) == 0 {
		j = bytes.IndexByte(obj, '{') + 1
	}
	out := append(append(append([]byte{}, doc[:start+j]...), ins...), doc[start+j:]...)
	return out, nil
}

// appendElem adds elem as the last element of the list arr.
func appendElem(arr, elem []byte) ([]byte, error) {
	i := bytes.LastIndexByte(arr, ']')
	open := bytes.IndexByte(arr, '[')
	if i < 0 || open < 0 {
		return nil, errors.New("not a list")
	}
	j := i
	for j > open+1 && (arr[j-1] == ' ' || arr[j-1] == '\n' || arr[j-1] == '\t' || arr[j-1] == '\r') {
		j--
	}
	var ins []byte
	if len(bytes.TrimSpace(arr[open+1:i])) == 0 {
		ins, j = elem, open+1
	} else {
		ins = append([]byte(", "), elem...)
	}
	return append(append(append([]byte{}, arr[:j]...), ins...), arr[j:]...), nil
}

// ListAgents is `c4 farm agents ls`: the members, whether each is busy,
// idle or stopped, and its last saved turn, as c4-agent ls gives them.
func ListAgents(ctx context.Context, o Out, a AgentsArgs) error {
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	out, err := agentCmd(ctx, bin, "ls")
	if err != nil {
		return err
	}
	if strings.Count(strings.TrimSpace(out), "\n") == 0 {
		o.printf("no agents yet; c4 farm agents add NAME adds a Claude Code session\n")
		return nil
	}
	o.printf("%s", out)
	return nil
}
