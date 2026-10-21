package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Who may give the controls that answer for a person (DESIGN.md section
// 10.6, Waits): deciding a wait, supplying what a unit stopped for, and
// withdrawing its question. Patch rights on the job (authorize) come first
// and still hold; this is the rule on top, read through one place under the
// policy keys a farm's policy sets (decide.by, decide.byUnits, supply.by,
// withdraw.by). Until a policy names them, these defaults hold.

// Entries of a control's by list.
const (
	// ByWaitsPerson: whom the wait names. A person: wait takes that
	// person, a party: wait a member of the party, an agent: wait that
	// agent. A wait that names no one (a review that named no reviewer)
	// takes any person who may change the job.
	ByWaitsPerson = "waits-person"
	// ByJob: anyone who may change the job.
	ByJob = "job"
	// ByRole is a prefix: role:<role> is anyone with that role
	// (api.Roles).
	ByRole = "role:"
)

// ControlRule is who may give one control.
type ControlRule struct {
	// By lists who may: ByWaitsPerson, ByJob or role:<role>; any one
	// suffices.
	By []string
	// Units: whether a unit, acting through its tool's c4 grant, may.
	// None may by default: a question for a person is answered by a
	// person (or, later, by the job's answerer).
	Units bool
	// Agents: whether an agent (a client through MCP) may, besides on a
	// wait that names it.
	Agents bool
}

// ControlRules are the farm's defaults, by control.
var ControlRules = map[string]ControlRule{
	// decide.by, decide.byUnits: a person's wait is that person's alone,
	// unless the farm and the project let someone stand in (standIn).
	"decide": {By: []string{ByWaitsPerson}},
	// supply.by
	"supply": {By: []string{ByJob}, Agents: true},
	// withdraw.by
	"withdraw": {By: []string{ByJob}, Agents: true},
}

// Decider is who gives a control: a person signed in, an agent acting for
// one through MCP, a unit through its tool's grant, or the person a
// tracker maps (a relayed decision).
type Decider struct {
	User string
	Role string
	// Parties are the outside parties the person speaks for: their
	// c4:party:<name> or c4-party-<name> groups, or the party a tracker
	// maps its user to.
	Parties []string
	// Agent names the agent, when the control comes through MCP.
	Agent string
	Unit  bool
}

// deciderOf is who makes request r as viewer v.
func deciderOf(r *http.Request, v *api.Viewer) Decider {
	d := Decider{User: v.User, Role: v.Role, Parties: v.Parties, Unit: v.Unit != nil}
	if via(r) == "mcp" {
		d.Agent = r.Header.Get("X-C4-Agent")
		if d.Agent == "" {
			d.Agent = "mcp"
		}
	}
	return d
}

// MayControl says whether who may give control on a unit that waits for
// server (person:<id>, party:<name>, agent:<name>, or "" for a question
// of no one in particular).
func MayControl(control, server string, who Decider) error {
	rule, ok := ControlRules[control]
	if !ok {
		return fmt.Errorf("%s is not a control a person gives", control)
	}
	kind, name, _ := strings.Cut(server, ":")
	switch {
	case who.Unit && !rule.Units:
		return fmt.Errorf("%s is a unit; only a person may %s this (%s.byUnits is off)", who.User, control, control)
	case who.Agent != "" && kind == "agent" && name == who.Agent:
		return nil
	case who.Agent != "" && !rule.Agents:
		return fmt.Errorf("agent %s may not %s a wait that does not name it; this waits for %s", who.Agent, control, waitsWhom(server))
	}
	for _, by := range rule.By {
		switch {
		case by == ByJob:
			return nil
		case by == ByWaitsPerson && kind == "person" && (name == "" || who.User == name):
			return nil
		case by == ByWaitsPerson && kind == "party" && slices.Contains(who.Parties, name):
			return nil
		case by == ByWaitsPerson && kind == "unit":
			// A unit answers its wait; a person may still decide it for
			// the job.
			return nil
		case strings.HasPrefix(by, ByRole) && who.Role != "" && who.Role == strings.TrimPrefix(by, ByRole):
			return nil
		}
	}
	return fmt.Errorf("this waits for %s; %s may not %s it (%s.by: %s)", waitsWhom(server), who.User, control, control, strings.Join(rule.By, ", "))
}

// waitsWhom names a wait's server in the farm's words.
func waitsWhom(server string) string {
	kind, name, _ := strings.Cut(server, ":")
	switch {
	case kind == "person" && name != "":
		return name + "'s decision"
	case kind == "party":
		return "a member of " + name
	case kind == "agent":
		return "agent " + name
	case kind == "unit":
		return "unit " + name + "'s answer"
	}
	return "a person's decision"
}

// PartiesOf are the outside parties a person speaks for, from their
// groups: c4:party:<name> or c4-party-<name>.
func PartiesOf(groups []string) []string {
	var out []string
	for _, g := range groups {
		p, ok := strings.CutPrefix(g, "c4:party:")
		if !ok {
			p, ok = strings.CutPrefix(g, "c4-party-")
		}
		if ok && p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Standing in: deciding a person's wait for them. decide.standIn is a farm
// security key: the farm's value (Server.StandIn, deny by default) is the
// limit, and only under a farm at allow may a project choose, allow or
// deny, in its spec.work.policy (deny when it says nothing). Where stand-in
// is allowed, decide.standInBy (role:farm-admin by default; a project may
// name its own) may decide, and the record names both: for the person, by
// whoever stood in.
const (
	KeyStandIn   = "decide.standIn"
	KeyStandInBy = "decide.standInBy"
)

// standInDefaultBy is decide.standInBy's default.
var standInDefaultBy = []string{ByRole + "farm-admin"}

// standIn is whether project lets someone stand in for a person, who may,
// and where that came from (farm or project), for the record.
func (s *Server) standIn(project string) (bool, []string, string) {
	if s.StandIn != "allow" {
		return false, nil, "farm"
	}
	var values map[string]string
	if m := s.current(); m != nil {
		for _, p := range m.projects {
			if p.Name == project && p.Spec.Work != nil && p.Spec.Work.Policy != nil {
				values = p.Spec.Work.Policy.Values
			}
		}
	}
	if strings.TrimSpace(values[KeyStandIn]) != "allow" {
		return false, nil, "project " + project
	}
	by := standInDefaultBy
	if v := strings.TrimSpace(values[KeyStandInBy]); v != "" {
		by = nil
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				by = append(by, b)
			}
		}
	}
	return true, by, "project " + project
}

// mayDecide applies decide.by, then stand-in: it returns the person:<id>
// the decider stands in for ("" when they decide as themselves).
func (s *Server) mayDecide(project, server string, who Decider) (string, error) {
	err := MayControl("decide", server, who)
	if err == nil {
		return "", nil
	}
	kind, name, _ := strings.Cut(server, ":")
	if who.Unit || who.Agent != "" || kind != "person" || name == "" {
		return "", err
	}
	// A single-person farm (a local farm: SinglePerson names its
	// installer) is one person in every role: the installer decides a
	// wait addressed to any of them, recorded for that role.
	if one := s.singlePerson(context.Background()); one != "" && who.User == one {
		if slices.Contains(api.Roles, name) {
			return "role:" + name, nil
		}
		return server, nil
	}
	ok, by, from := s.standIn(project)
	if !ok {
		return "", fmt.Errorf("%v; no one stands in for a person here (%s: deny, from the %s)", err, KeyStandIn, from)
	}
	for _, b := range by {
		if r, isRole := strings.CutPrefix(b, ByRole); (isRole && who.Role != "" && who.Role == r) || (!isRole && b == who.User) {
			return server, nil
		}
	}
	return "", fmt.Errorf("%v; only %s may stand in for a person here (%s)", err, strings.Join(by, ", "), KeyStandInBy)
}
