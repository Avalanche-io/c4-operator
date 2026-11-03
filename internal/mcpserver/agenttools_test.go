package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// toolActs is the act each tool performs at the HUD, by the verb its
// control record names; "" for a tool that reads, or plans a grant the
// person makes. Every tool is listed, so a new one is classified here
// before it ships.
var toolActs = map[string]string{
	"c4_acknowledge_units": "acknowledge", "c4_agent": "append", "c4_avoid_node": "blacklist",
	"c4_boost_job": "boost", "c4_close_lead": "close", "c4_decide": "decide", "c4_disable_node": "disable",
	"c4_hold_job": "hold", "c4_move_to_next_session": "deadline", "c4_plan": "plan", "c4_publish_tool": "publish",
	"c4_requeue_units": "requeue", "c4_run_bundle": "bundle", "c4_set_deadline": "deadline", "c4_steer": "steer",
	"c4_stop_bundle": "bundle", "c4_submit_job": "submit", "c4_supply": "supply", "c4_withdraw_needs": "withdraw-needs",
	// Reads: what only a person may do, each says how the person does it.
	"c4_cache_tiers": "", "c4_capacity_and_cost": "", "c4_deadline_options": "", "c4_dependency_graph": "",
	"c4_describe_tool": "", "c4_diff_views": "", "c4_farm_status": "", "c4_get_capacity_forecast": "", "c4_get_job": "",
	"c4_get_unit": "", "c4_grant": "", "c4_list_bundles": "", "c4_list_grants": "", "c4_list_held_writes": "",
	"c4_list_jobs": "", "c4_list_nodes": "", "c4_list_people_map": "", "c4_list_tools": "", "c4_list_trackers": "",
	"c4_needs_me":  "",
	"c4_read_book": "", "c4_suggest_deadline": "", "c4_trace_file": "", "c4_verify_custody": "",
}

// The agent's toolset holds no act only a person may do: none that
// provenance.ActNeeds marks a person's or the person's touch (grant,
// revoke, approving paid capacity, a held write's answer and the rest),
// which the person does themselves. A decision is the agent's
// only for a wait addressed to it, which the server signs and the HUD
// holds it to (agentkey.go, agentacts.go).
func TestToolsetHoldsNoPersonsAct(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range Tools(nil) {
		name := tl.Def.Name
		seen[name] = true
		act, ok := toolActs[name]
		if !ok {
			t.Errorf("tool %s is not classified: name the act it performs in toolActs", name)
			continue
		}
		if act == "" {
			if tl.Def.Annotations == nil || !tl.Def.Annotations.ReadOnlyHint {
				t.Errorf("tool %s performs no act, and isn't marked read-only", name)
			}
			continue
		}
		params := json.RawMessage(`{}`)
		if act == "decide" {
			params = json.RawMessage(`{"waitsFor": "agent:claude-code"}`)
		}
		if need := provenance.ActNeeds(act, params, time.Now()); need.Person || need.Touch {
			t.Errorf("tool %s performs %s, which is a person's (%+v): take it out: the person does it", name, act, need)
		}
	}
	for name := range toolActs {
		if !seen[name] {
			t.Errorf("toolActs names %s, which isn't a tool", name)
		}
	}
	// A person-only verb is caught: the test isn't vacuous.
	for _, act := range []string{"revoke", "approve-capacity", "confirm-write", "grant"} {
		if need := provenance.ActNeeds(act, json.RawMessage(`{}`), time.Now()); !need.Person && !need.Touch {
			t.Errorf("ActNeeds says %s needs neither a person nor a touch", act)
		}
	}
	for _, gone := range []string{"c4_approve_capacity", "c4_confirm_write", "c4_revoke_grant", "c4_plan_"} {
		if strings.Contains(Instructions, gone) {
			t.Errorf("the server's instructions name %s, which is no tool", gone)
		}
	}
}

// What only a person may do has no tool: each waiting item a read tool
// returns says how the person acts on it, and the tools send nothing. A
// capacity request waiting for approval is approved in the HUD with the
// person's touch; one already provisioned waits for no one. A held write
// is answered in the HUD; a grant in force is revoked by the person.
func TestReadToolsSayHowAPersonActs(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lt.Tools {
		if strings.HasPrefix(tl.Name, "c4_plan_") || tl.Name == "c4_revoke_grant" || tl.Name == "c4_approve_capacity" || tl.Name == "c4_confirm_write" {
			t.Errorf("%s is a tool: what only a person may do has none", tl.Name)
		}
	}
	res, out := call(t, cs, "c4_capacity_and_cost", map[string]any{})
	reqs, _ := out["requests"].([]any)
	if res.IsError || len(reqs) != 2 {
		t.Fatalf("capacity: %v %q", out, text(res))
	}
	waiting, provisioned := reqs[0].(map[string]any), reqs[1].(map[string]any)
	if how, _ := waiting["howAPersonActs"].(string); !strings.Contains(how, "approve it in the HUD, with your touch: Capacity, pool burst-gpu: http") {
		t.Errorf("a waiting request: %v", waiting)
	}
	if _, ok := provisioned["howAPersonActs"]; ok {
		t.Errorf("a provisioned request waits for no one: %v", provisioned)
	}
	res, out = call(t, cs, "c4_list_held_writes", map[string]any{})
	held, _ := out["held"].([]any)
	if res.IsError || len(held) != 1 || !strings.Contains(held[0].(map[string]any)["howAPersonActs"].(string), "answer it in the HUD: Held writes, show-a/gh-issue-7 comment: http") {
		t.Errorf("held: %v %q", out, text(res))
	}
	res, out = call(t, cs, "c4_list_grants", map[string]any{})
	grants, _ := out["grants"].([]any)
	if res.IsError || len(grants) != 1 || grants[0].(map[string]any)["howAPersonActs"] != "your person runs: c4 farm revoke show-a/github-ab12, or revokes it in the HUD" {
		t.Errorf("grants: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	for _, p := range hud.posts {
		if !strings.HasPrefix(p, "GET ") {
			t.Errorf("a read tool sent %s", p)
		}
	}
}
