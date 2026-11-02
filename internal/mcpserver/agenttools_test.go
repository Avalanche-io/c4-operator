package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// toolActs is the act each tool performs at the HUD, by the verb its
// control record names; "" for a tool that reads, or only plans what the
// person does. Every tool is listed, so a new one is classified here
// before it ships.
var toolActs = map[string]string{
	"c4_acknowledge_units": "acknowledge", "c4_agent": "append", "c4_avoid_node": "blacklist",
	"c4_boost_job": "boost", "c4_close_lead": "close", "c4_decide": "decide", "c4_disable_node": "disable",
	"c4_hold_job": "hold", "c4_move_to_next_session": "deadline", "c4_plan": "plan", "c4_publish_tool": "publish",
	"c4_requeue_units": "requeue", "c4_run_bundle": "bundle", "c4_set_deadline": "deadline", "c4_steer": "steer",
	"c4_stop_bundle": "bundle", "c4_submit_job": "submit", "c4_supply": "supply", "c4_withdraw_needs": "withdraw-needs",
	// Reads, and plans of what the person does.
	"c4_cache_tiers": "", "c4_capacity_and_cost": "", "c4_deadline_options": "", "c4_dependency_graph": "",
	"c4_describe_tool": "", "c4_diff_views": "", "c4_farm_status": "", "c4_get_capacity_forecast": "", "c4_get_job": "",
	"c4_get_unit": "", "c4_grant": "", "c4_list_bundles": "", "c4_list_grants": "", "c4_list_held_writes": "",
	"c4_list_jobs": "", "c4_list_nodes": "", "c4_list_people_map": "", "c4_list_tools": "", "c4_list_trackers": "",
	"c4_needs_me": "", "c4_plan_approve_capacity": "", "c4_plan_confirm_write": "", "c4_plan_revoke": "",
	"c4_read_book": "", "c4_suggest_deadline": "", "c4_trace_file": "", "c4_verify_custody": "",
}

// The agent's toolset holds no act only a person may do: none that
// provenance.ActNeeds marks a person's or the person's touch (grant,
// revoke, approving paid capacity, a held write's answer and the rest),
// which the agent plans and the person does. A decision is the agent's
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
			t.Errorf("tool %s performs %s, which is a person's (%+v): make it a plan", name, act, need)
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
	if strings.Contains(Instructions, "c4_approve_capacity") || strings.Contains(Instructions, "c4_confirm_write") {
		t.Error("the server's instructions name a tool that is now a plan")
	}
}

// Approving paid capacity is planned for the person, never sent.
func TestCapacityApprovalOnlyPlanned(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_plan_approve_capacity", map[string]any{"pool": "burst-gpu"})
	if res.IsError || out["performed"] != false || !strings.Contains(out["where"].(string), "pool burst-gpu") {
		t.Fatalf("plan: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 0 {
		t.Errorf("the plan sent %v", hud.posts)
	}
}
