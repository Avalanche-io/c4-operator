package farmcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
)

// The agent's sandbox (PEOPLE-KEYS-2 P6, K5 item 7): an integrated agent is
// denied the farm's directory on this machine (~/.c4/<farm>/: its keys and
// its installer token), so the agent reaches the farm only through its
// tools, whose server, outside the sandbox, signs as the agent with its
// own key. Claude Code's own settings carry it: Read and Edit deny rules,
// which its file tools and the shell commands it recognises obey, and the
// sandbox's denyRead, which every command obeys once the sandbox is on.
// c4 records each entry it adds and takes out only those.

// SandboxRecord is what c4 added to Claude Code's user settings, by entry:
// "deny:<rule>", "denyRead:<path>", "enabled", "noEscape".
type SandboxRecord map[string]bool

// claudeSettings is Claude Code's user settings file: in CLAUDE_CONFIG_DIR
// when it is set, else ~/.claude.
func claudeSettings() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// farmDirRule is a farm's directory as Claude Code's rules name it:
// home-relative, all within.
func farmDirRule(farm string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(KeysFor(farm).Dir)
	rel, err := filepath.Rel(home, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("the farm's directory %s is not in your home folder", dir)
	}
	return "~/" + filepath.ToSlash(rel) + "/**", nil
}

// SandboxOn denies the agent farm's directory in Claude Code's user
// settings, and, with enable, turns its sandbox on with no way around it,
// so every command the agent runs is held to the deny. It answers what it
// added, to record.
func SandboxOn(farm string, enable bool) (SandboxRecord, error) {
	rule, err := farmDirRule(farm)
	if err != nil {
		return nil, err
	}
	added := SandboxRecord{}
	err = editSettings(func(s map[string]any) {
		perms := object(s, "permissions")
		deny := jsonStrings(perms["deny"])
		for _, r := range []string{"Read(" + rule + ")", "Edit(" + rule + ")"} {
			if !slices.Contains(deny, r) {
				deny = append(deny, r)
				added["deny:"+r] = true
			}
		}
		perms["deny"] = deny
		sb := object(s, "sandbox")
		fs := object(sb, "filesystem")
		read := jsonStrings(fs["denyRead"])
		if !slices.Contains(read, rule) {
			read = append(read, rule)
			added["denyRead:"+rule] = true
		}
		fs["denyRead"] = read
		if !enable {
			return
		}
		if sb["enabled"] != true {
			sb["enabled"] = true
			added["enabled"] = true
		}
		if v, ok := sb["allowUnsandboxedCommands"]; !ok || v != false {
			sb["allowUnsandboxedCommands"] = false
			added["noEscape"] = true
		}
	})
	return added, err
}

// SandboxOff takes out of Claude Code's user settings what c4 added, and
// nothing else: a rule or setting the person had is theirs.
func SandboxOff(added SandboxRecord) error {
	if len(added) == 0 {
		return nil
	}
	return editSettings(func(s map[string]any) {
		perms := object(s, "permissions")
		perms["deny"] = slices.DeleteFunc(jsonStrings(perms["deny"]), func(r string) bool { return added["deny:"+r] })
		sb := object(s, "sandbox")
		fs := object(sb, "filesystem")
		fs["denyRead"] = slices.DeleteFunc(jsonStrings(fs["denyRead"]), func(p string) bool { return added["denyRead:"+p] })
		if added["enabled"] {
			delete(sb, "enabled")
		}
		if added["noEscape"] {
			delete(sb, "allowUnsandboxedCommands")
		}
		tidy(s)
	})
}

// editSettings changes Claude Code's user settings in place: everything
// else in them kept, written whole, and read back as JSON before it counts.
// A settings file that isn't JSON is left alone, and said.
func editSettings(change func(map[string]any)) error {
	p := claudeSettings()
	// a settings file that is a link (one config folder pointing at
	// another's) is changed where it points, and stays a link
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	s := map[string]any{}
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("Claude Code's settings %s don't read as JSON (%v): left as they are; fix them, then c4 farm integrate on", tildePath(p), err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	change(s)
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := keystore.WriteFile(p, append(out, '\n')); err != nil {
		return err
	}
	var check map[string]any
	if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &check) != nil {
		return fmt.Errorf("Claude Code's settings %s were written and don't read back", tildePath(p))
	}
	return nil
}

// object is m[k] as an object, made if it isn't one.
func object(m map[string]any, k string) map[string]any {
	if o, ok := m[k].(map[string]any); ok {
		return o
	}
	o := map[string]any{}
	m[k] = o
	return o
}

// strings is a JSON list of strings, its other members dropped.
func jsonStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	if l, ok := v.([]string); ok {
		out = append(out, l...)
	}
	return out
}

// tidy drops what SandboxOff emptied: a list or object c4 left empty.
func tidy(s map[string]any) {
	for _, k := range []string{"permissions", "sandbox"} {
		o, ok := s[k].(map[string]any)
		if !ok {
			continue
		}
		if fs, ok := o["filesystem"].(map[string]any); ok {
			if len(jsonStrings(fs["denyRead"])) == 0 {
				delete(fs, "denyRead")
			}
			if len(fs) == 0 {
				delete(o, "filesystem")
			}
		}
		if len(jsonStrings(o["deny"])) == 0 {
			delete(o, "deny")
		}
		if len(o) == 0 {
			delete(s, k)
		}
	}
}

// SandboxQuestion asks whether to turn Claude Code's sandbox on, saying
// plainly what it does beyond the farm's agent.
const SandboxQuestion = "Also turn on Claude Code's sandbox? It confines Bash in every Claude Code session you run, not only the farm's agent: " +
	"writes to the project folder, the network to an allowlist, and a command outside that fails rather than asks. " +
	"With it on, scripts the agent runs can't read C4's key directory either."

// sandboxRecordKey is how an entry c4 added is kept in the integration's
// record, so integrate off takes out exactly that.
const sandboxRecordKey = "sandbox:"

// sandboxOn denies the agent the farm's directory, always, and turns
// Claude Code's sandbox on only with the person's own yes (DESIGN 10:
// consent is never inferred, so no terminal is no).
func (in *Integration) sandboxOn(o Out) {
	enable := in.AskSandbox != nil && in.AskSandbox(SandboxQuestion)
	added, err := SandboxOn(in.SandboxFarm, enable)
	if err != nil {
		o.printf("sandbox   C4's key directory is not denied to the agent: %v\n", err)
		return
	}
	for k := range added {
		in.Record[sandboxRecordKey+k] = true
	}
	s, _ := sandboxState()
	switch {
	case s.enabled && !s.escape:
		o.printf("sandbox   C4's key directory (%s) is denied to the agent, its scripts included: Claude Code's sandbox is on\n", tildePath(filepath.Dir(KeysFor(in.SandboxFarm).Dir)))
	default:
		o.printf("sandbox   C4's key directory is denied to Claude's own tools, but not to scripts the agent runs; the agent's key is protected from scripts only with Claude Code's sandbox on\n")
	}
}

// sandboxOff takes out what sandboxOn added, and nothing else.
func (in *Integration) sandboxOff(o Out) {
	added := SandboxRecord{}
	for k, v := range in.Record {
		if rest, ok := strings.CutPrefix(k, sandboxRecordKey); ok && v {
			added[rest] = true
		}
	}
	if len(added) == 0 {
		return
	}
	if err := SandboxOff(added); err != nil {
		o.printf("sandbox   what c4 added to Claude Code's settings is still there: %v\n", err)
		return
	}
	for k := range added {
		delete(in.Record, sandboxRecordKey+k)
	}
	o.printf("sandbox   what c4 added to Claude Code's settings is taken out\n")
}

// sandboxState is whether Claude Code's sandbox is on in the person's user
// settings, and whether commands can still run outside it.
func sandboxState() (struct{ enabled, escape bool }, error) {
	var out struct{ enabled, escape bool }
	b, err := os.ReadFile(claudeSettings())
	if err != nil {
		return out, err
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		return out, err
	}
	sb, _ := s["sandbox"].(map[string]any)
	out.enabled = sb["enabled"] == true
	out.escape = sb["allowUnsandboxedCommands"] != false
	return out, nil
}
