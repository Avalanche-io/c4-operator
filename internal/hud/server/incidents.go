package server

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Incidents: alerting by root cause (proposals/hud-proficiency 6, after
// FAA AC 25.1322-1 ¶8d and ¶10c). One item per cause, everything it caused
// folded in, with an ordered checklist that leads to the action:
//
//   - failures after their retries, by cause across jobs (the message's
//     cause with its numbers blurred);
//   - a decision or an escalated review that holds up a job, one per job;
//   - anything else a job needs a person for, one per job;
//   - machines that stopped reporting in the last hour, by rack, with the
//     units they interrupted (the farm handles these: the units ran again
//     without spending a retry), and spot machines taken back.
//
// The rule is the one c4-hud's incidents.js applied on the client before
// the server sent them (c4-hud-intro-to-c4-sched.md).

// incidentWindow is how far back machines that stopped reporting count.
const incidentWindow = time.Hour

// rackLabels name a node's rack, first match; else the node's name less a
// trailing number.
var rackLabels = []string{"topology.kubernetes.io/rack", "c4.avalanche.io/rack"}

var trailingNumber = regexp.MustCompile(`-\d+$`)

func rackOf(n *corev1.Node, name string) string {
	if n != nil {
		for _, l := range rackLabels {
			if r := n.Labels[l]; r != "" {
				return r
			}
		}
	}
	return trailingNumber.ReplaceAllString(name, "")
}

// noun is what a class's units are called in a title.
func noun(class string, n int) string {
	w := map[string]string{"render": "frame", "ci": "shard", "ml": "step"}[class]
	if w == "" {
		w = "unit"
	}
	if n != 1 {
		w += "s"
	}
	return w
}

var digits = regexp.MustCompile(`\d+`)

func incidents(m *model, v *api.Viewer, jobs []api.Job) []api.Incident {
	out := []api.Incident{}
	key := func(j *api.Job) string { return j.Namespace + "/" + j.Name }

	// What jobs need a person for, grouped by cause.
	type group struct {
		reason, cause string
		jobs          []*api.Job
	}
	var order []string
	groups := map[string]*group{}
	for i := range jobs {
		j := &jobs[i]
		np := j.NeedsPerson
		if np == nil {
			continue
		}
		cause, k := np.Message, np.Reason+"|"+key(j)
		if np.Reason == "failed" {
			if _, after, ok := strings.Cut(np.Message, ":"); ok && strings.TrimSpace(after) != "" {
				cause = strings.TrimSpace(after)
			} else {
				cause = "failed after its retries"
			}
			blurred := digits.ReplaceAllString(cause, "#")
			k = "failed|" + blurred[:min(len(blurred), 80)]
		}
		g := groups[k]
		if g == nil {
			g = &group{reason: np.Reason, cause: cause}
			groups[k] = g
			order = append(order, k)
		}
		g.jobs = append(g.jobs, j)
	}
	for _, k := range order {
		g := groups[k]
		first := g.jobs[0]
		since := first.NeedsPerson.Since
		var keys []string
		for _, j := range g.jobs {
			keys = append(keys, key(j))
			if j.NeedsPerson.Since.Before(since) {
				since = j.NeedsPerson.Since
			}
		}
		switch g.reason {
		case "failed":
			n, taken := 0, true
			for _, j := range g.jobs {
				n += j.Units.Failed
				if !hasAck(j, since, "acknowledge", "assign") {
					taken = false
				}
			}
			it := api.Incident{ID: "failed:" + key(first), NeedsYou: true, Count: n, CountOf: noun(first.Class, n), Jobs: keys, Since: since,
				State: "open", Detail: g.cause}
			if taken {
				it.State = "acknowledged"
			}
			on := first.Name
			if len(g.jobs) > 1 {
				on = fmt.Sprintf("%d jobs", len(g.jobs))
			}
			it.Title = fmt.Sprintf("%d %s on %s failed after their retries", n, noun(first.Class, n), on)
			if len(g.jobs) == 1 {
				// The controls act on one job.
				it.Checklist = []api.IncidentStep{
					{Label: "Read why in one failed " + noun(first.Class, 1) + "'s log", Act: "Open", Href: "job/" + key(first)},
					{Label: "If a machine is at fault, avoid it for this job, then requeue", Act: "Requeue failed", Action: "requeue", Job: key(first), Units: first.UnitStates.Failed},
					{Label: "Or, if they are acceptable as they are", Act: "Accept them", Action: "accept", Job: key(first), Units: first.UnitStates.Failed,
						Done: hasAck(first, since, "accept")},
				}
			} else {
				it.Checklist = []api.IncidentStep{
					{Label: "Read why in one failed " + noun(first.Class, 1) + "'s log", Act: "Open", Href: "job/" + key(first)},
					{Label: fmt.Sprintf("The same cause on %d jobs: requeue or accept each from its job", len(g.jobs)), Act: "Open the next", Href: "job/" + key(g.jobs[1])},
				}
			}
			out = append(out, it)
		case "decision", "review":
			var w *api.Wait
			for i := range first.Waits {
				if x := &first.Waits[i]; x.NeedsYou && x.Do == "" {
					w = x
					break
				}
			}
			whose, mine := "a person", false
			detail := g.cause
			if w != nil {
				detail = w.Ask
				if detail == "" {
					detail = g.cause
				}
				id := strings.TrimPrefix(w.WaitsFor, "person:")
				switch {
				case strings.HasPrefix(w.WaitsFor, "party:"):
					whose, mine = strings.TrimPrefix(w.WaitsFor, "party:"), true
				case v != nil && id == v.User:
					whose, mine = "you", true
				case id != "":
					whose = id
				}
			}
			title := first.Name + " waits for " + map[bool]string{true: "your", false: whose + "'s"}[whose == "you"] + " decision"
			if g.reason == "review" {
				title = "A check sent " + first.Name + " to " + whose
			}
			out = append(out, api.Incident{ID: g.reason + ":" + key(first), NeedsYou: mine, Title: title, Detail: detail, Count: 1, CountOf: "decision",
				Jobs: keys, Since: since, State: "open", Checklist: []api.IncidentStep{
					{Label: "See what it holds up", Act: "Open", Href: "job/" + key(first)},
					{Label: "Decide: approve, rework or accept", Act: "Decide", Href: "job/" + key(first)},
				}})
		case "needsinformation":
			// A unit's brief lacked information only a person could give:
			// a work-stopping error, fixed by supplying it.
			var open []api.Needs
			for _, n := range first.Needs {
				if n.Supplied == "" {
					open = append(open, n)
				}
			}
			detail := g.cause
			units := ""
			if len(open) > 0 {
				detail = open[0].Text
				if open[0].Why != "" {
					detail += " (" + open[0].Why + ")"
				}
				units = strconv.Itoa(open[0].Unit)
			}
			out = append(out, api.Incident{ID: g.reason + ":" + key(first), NeedsYou: true, Title: first.Name + " needs information its brief lacked",
				Detail: detail, Count: max(len(open), 1), CountOf: "question", Jobs: keys, Since: since, State: "open", Checklist: []api.IncidentStep{
					{Label: "See what it was doing", Act: "Open", Href: "job/" + key(first)},
					{Label: "Supply the information; the unit resumes where it stopped", Act: "Supply", Action: "supply", Job: key(first), Units: units},
				}})
		default:
			out = append(out, api.Incident{ID: g.reason + ":" + key(first), NeedsYou: true, Title: first.Name + " needs you", Detail: g.cause,
				Count: 1, Jobs: keys, Since: since, State: "open", Checklist: []api.IncidentStep{{Label: "Open the job", Act: "Open", Href: "job/" + key(first)}}})
		}
	}

	// The farm's own trouble in the last hour, by rack: the machines that
	// stopped reporting and the units they held are one item, not dozens.
	type rack struct {
		nodes map[string]bool
		units int
		jobs  map[string]bool
		since time.Time
		spot  bool
	}
	racks := map[string]*rack{}
	var rackOrder []string
	at := func(r string, t time.Time) *rack {
		g := racks[r]
		if g == nil {
			g = &rack{nodes: map[string]bool{}, jobs: map[string]bool{}, since: t}
			racks[r] = g
			rackOrder = append(rackOrder, r)
		}
		if t.Before(g.since) {
			g.since = t
		}
		return g
	}
	nodes := map[string]*corev1.Node{}
	for i := range m.nodes {
		n := &m.nodes[i]
		nodes[n.Name] = n
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue && m.at.Sub(c.LastTransitionTime.Time) <= incidentWindow {
				at(rackOf(n, n.Name), c.LastTransitionTime.Time).nodes[n.Name] = true
			}
		}
	}
	visible := map[string]bool{}
	for i := range jobs {
		visible[key(&jobs[i])] = true
	}
	for _, rec := range m.jobs {
		if !visible[rec.key] {
			continue
		}
		for _, x := range rec.job.Status.Interruptions {
			if x.Node == "" || m.at.Sub(x.At.Time) > incidentWindow {
				continue
			}
			g := at(rackOf(nodes[x.Node], x.Node), x.At.Time)
			g.units++
			g.jobs[rec.key] = true
			if strings.Contains(strings.ToLower(x.Reason), "spot") || strings.Contains(x.Reason, "Preempt") {
				g.spot = true
			}
		}
	}
	for _, r := range rackOrder {
		g := racks[r]
		if len(g.nodes) < 2 && g.units < 5 {
			continue
		}
		var js []string
		for k := range g.jobs {
			js = append(js, k)
		}
		sort.Strings(js)
		it := api.Incident{ID: "rack:" + r, Count: g.units, CountOf: "units moved", Jobs: append([]string{}, js...), Since: g.since, State: "open",
			Detail: fmt.Sprintf("%d unit%s on %d job%s were interrupted and queued again on other machines, without spending a retry. Nothing to do now.",
				g.units, map[bool]string{true: "", false: "s"}[g.units == 1], len(js), map[bool]string{true: "", false: "s"}[len(js) == 1]),
			Checklist: []api.IncidentStep{}}
		switch {
		case len(g.nodes) > 0:
			it.Title = fmt.Sprintf("%d machine%s in %s stopped reporting", len(g.nodes), map[bool]string{true: "", false: "s"}[len(g.nodes) == 1], r)
			it.Checklist = []api.IncidentStep{{Label: "If they are not back within the hour, tell facilities", Act: "Open Pools", Href: "pools"}}
		case g.spot:
			it.Title = "The cloud took back spot machines in " + r
		default:
			it.Title = "Work was interrupted on machines in " + r
		}
		out = append(out, it)
	}
	// What a person must do (the viewer's or another's) before what the
	// farm handles; then most work first.
	handled := func(it api.Incident) bool { return strings.HasPrefix(it.ID, "rack:") }
	sort.SliceStable(out, func(a, b int) bool {
		if handled(out[a]) != handled(out[b]) {
			return !handled(out[a])
		}
		return out[a].Count > out[b].Count
	})
	return out
}

// hasAck reports whether the job has an ack of one of kinds since t.
func hasAck(j *api.Job, t time.Time, kinds ...string) bool {
	for _, a := range j.Acks {
		for _, k := range kinds {
			if a.Kind == k && !a.At.Before(t) {
				return true
			}
		}
	}
	return false
}
