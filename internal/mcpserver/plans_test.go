package mcpserver

import (
	"strings"
	"testing"
)

// c4_submit_job carries the plans the submission's jobs name, as the
// agent sends them; a plan named and not sent is refused at its job, in
// the farm's words, before the farm is asked.
func TestSubmitSendsPlans(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	sub := map[string]any{"c4": "submit/1", "project": "show-a", "shot": "port",
		"jobs": map[string]any{"port": map[string]any{"tool": "agent", "plan": "port.plan.yaml"}}}

	_, out := call(t, cs, "c4_submit_job", map[string]any{"dry_run": true, "submission": sub})
	errs, _ := out["errors"].([]any)
	if out["ok"] == true || len(errs) != 1 || asMap(errs[0])["path"] != "jobs.port.plan" ||
		asMap(errs[0])["message"] != "port.plan.yaml was not sent with the submission" || !strings.Contains(asMap(errs[0])["fix"].(string), `plans under "port.plan.yaml"`) {
		t.Fatalf("a plan not sent: %v", out)
	}
	hud.mu.Lock()
	n := len(hud.posts)
	hud.mu.Unlock()
	if n != 0 {
		t.Fatalf("the farm was asked: %d posts", n)
	}

	plan := map[string]any{"version": 1, "blocks": []any{map[string]any{"name": "scan"}, map[string]any{"name": "port", "after": []any{"scan"}}}}
	if res, out := call(t, cs, "c4_submit_job", map[string]any{"dry_run": true, "submission": sub, "plans": map[string]any{"port.plan.yaml": plan}}); res.IsError {
		t.Fatalf("with its plan: %q %v", text(res), out)
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 1 || !strings.Contains(hud.posts[0], `"plans":{"port.plan.yaml":{`) || !strings.Contains(hud.posts[0], `"after":["scan"]`) {
		t.Fatalf("posts %v", hud.posts)
	}
}
