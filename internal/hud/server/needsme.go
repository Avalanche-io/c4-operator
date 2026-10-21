package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// handleNeedsMe is GET /needs-me: only what this viewer may act on now
// (api.NeedsMe), the one-person view and MCP's c4_needs_me.
func (s *Server) handleNeedsMe(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	writeJSON(w, http.StatusOK, s.needsMe(r, v, s.snapshot(m, v)))
}

// needsMe lists what waits for v in snap, by the rules the controls apply
// (patch rights on the job, then whomay.go), so no listed button is
// refused:
//   - a unit's open question goes to its job's owner (escalation.to's
//     default), or, for a job with no owner, to anyone who may change it;
//   - a wait goes to whom it names (decide.by), and on a single-person
//     farm to its installer, whatever role it names; standing in for
//     someone is not something that needs them;
//   - a burst goes to whoever may approve the request.
func (s *Server) needsMe(r *http.Request, v *api.Viewer, snap *api.Snapshot) *api.NeedsMe {
	out := &api.NeedsMe{Now: snap.Now, Viewer: v, Items: []api.NeedsMeItem{}}
	ctx := r.Context()
	who := deciderOf(r, v)
	single := s.SinglePerson != "" && v.User == s.SinglePerson
	mayChange := map[string]bool{}
	may := func(j *api.Job) bool {
		k := j.Namespace + "/" + j.Name
		ok, seen := mayChange[k]
		if !seen {
			ok = s.authorize(ctx, r, v, j.Namespace, "patch", "workjobs", j.Name) == nil
			mayChange[k] = ok
		}
		return ok
	}
	for i := range snap.Jobs {
		j := &snap.Jobs[i]
		key := j.Namespace + "/" + j.Name
		base := "/api/v1/jobs/" + key
		for _, n := range j.Needs {
			if n.Supplied != "" || n.Withdrawn != nil || (n.State != "" && n.State != "raised" && n.State != "pressing") {
				continue
			}
			if j.Owner != "" && j.Owner != v.User && !single {
				continue
			}
			if !may(j) || MayControl("supply", "", who) != nil {
				continue
			}
			unit := n.Unit
			name := n.Name
			if name == "" {
				name = "unit " + strconv.Itoa(unit)
			}
			state, severity := "raised", "warning"
			if n.State == "pressing" {
				state, severity = "pressing", "urgent"
			}
			since := n.RaisedAt
			if since.IsZero() {
				since = n.At
			}
			out.Items = append(out.Items, api.NeedsMeItem{ID: "question:" + key + "/" + strconv.Itoa(unit), Kind: "question",
				Job: key, Project: j.Project, Unit: &unit, UnitName: n.Name, Title: name + " in " + j.Name + " asks you",
				Ask: n.Text, Why: n.Why, State: state, Severity: severity, Since: since, AnswerBy: n.AnswerBy, PressingWhy: n.PressingWhy,
				Href: "job/" + key, Actions: []api.NeedsMeAction{
					{Act: "Supply", Action: "supply", Path: base + "/supply", Body: map[string]any{"unit": unit}},
					{Act: "Withdraw", Action: "withdraw-needs", Path: base + "/needs/" + strconv.Itoa(unit) + "/withdraw"},
				}})
		}
		for _, wt := range j.Waits {
			if wt.Do != "" {
				continue
			}
			kind, named, _ := strings.Cut(wt.WaitsFor, ":")
			if kind != "person" && kind != "party" && kind != "agent" {
				continue
			}
			mine := MayControl("decide", wt.WaitsFor, who) == nil
			if kind == "person" && named == "" {
				// A wait that names no one goes where its job's questions go.
				mine = mine && (j.Owner == "" || j.Owner == v.User)
			}
			if !mine && !(single && kind == "person") {
				continue
			}
			if !may(j) {
				continue
			}
			unit := wt.Unit
			name := wt.Name
			if name == "" {
				name = "unit " + strconv.Itoa(unit)
			}
			ref := any(unit)
			if wt.Name != "" {
				ref = wt.Name
			}
			choices := wt.Choices
			switch {
			case wt.Escalated:
				choices = []string{"accept", "rework"}
			case len(choices) == 0:
				choices = []string{"approve"}
			}
			var acts []api.NeedsMeAction
			for _, c := range choices {
				acts = append(acts, api.NeedsMeAction{Act: strings.ToUpper(c[:1]) + c[1:], Action: "decide", Path: base + "/decide",
					Body: map[string]any{"unit": ref, "do": c}})
			}
			title := name + " in " + j.Name + " waits for your decision"
			if kind == "party" {
				title = name + " in " + j.Name + " waits for " + named + "'s decision"
			}
			it := api.NeedsMeItem{ID: "decision:" + key + "/" + strconv.Itoa(unit), Kind: "decision", Job: key, Project: j.Project,
				Unit: &unit, UnitName: wt.Name, Title: title, Ask: wt.Ask, State: "open", Severity: "warning", Choices: wt.Choices,
				Href: "job/" + key, Actions: acts}
			if wt.Since != nil {
				it.Since = *wt.Since
			}
			out.Items = append(out.Items, it)
		}
	}
	// An observed session that needs its person: no farm control answers
	// it, only the session itself, in Claude Code.
	for _, o := range snap.Observed {
		if o.Word != "needs you" {
			continue
		}
		name := o.Name
		if name == "" {
			name = "a session"
		}
		on := o.Machine
		if on == "" {
			on = o.Node
		}
		it := api.NeedsMeItem{ID: "session:" + o.Node + "/" + o.Session, Kind: "session", Title: name + " on " + on + " needs you",
			Ask: o.NeedsYou, State: "open", Severity: "warning", Actions: []api.NeedsMeAction{}, Open: "claude --resume " + o.Session}
		if o.LastActive != nil {
			it.Since = *o.LastActive
		}
		out.Items = append(out.Items, it)
	}
	for _, c := range snap.Capacity {
		if c.State != "PendingApproval" {
			continue
		}
		if s.authorize(ctx, r, v, "", "patch", "capacityrequests", "") != nil {
			continue
		}
		out.Items = append(out.Items, api.NeedsMeItem{ID: "burst:" + c.Pool, Kind: "burst",
			Title: strconv.Itoa(c.Slots) + " slot" + plural(c.Slots) + " in " + c.Pool + " wait for your approval",
			Ask:   c.Reason, State: "pending", Severity: "warning", Since: snap.Now, Href: "farm",
			Actions: []api.NeedsMeAction{{Act: "Approve", Action: "approve", Path: "/api/v1/capacity/" + c.Pool + "/approve"}}})
	}
	sort.SliceStable(out.Items, func(a, b int) bool {
		x, y := out.Items[a], out.Items[b]
		if (x.Severity == "urgent") != (y.Severity == "urgent") {
			return x.Severity == "urgent"
		}
		switch {
		case x.AnswerBy != nil && y.AnswerBy != nil && !x.AnswerBy.Equal(*y.AnswerBy):
			return x.AnswerBy.Before(*y.AnswerBy)
		case (x.AnswerBy != nil) != (y.AnswerBy != nil):
			return x.AnswerBy != nil
		}
		return x.Since.Before(y.Since)
	})
	return out
}
