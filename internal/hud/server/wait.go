package server

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A lead waits for its workers the way it waits for any background work: a
// background command (c4 farm wait) that exits when a worker ends or stops
// on a question. The command long-polls this route, which looks at the
// HUD's own picture of the farm each second and answers as soon as the
// units it names settle, or when its window ends, so nothing polls the
// farm itself and a proxy's timeout is never reached.

// waitWithin is how long a wait holds its request by default, inside the
// timeouts proxies commonly put in front of the HUD; maxWaitWithin bounds
// what a client may ask for.
const (
	waitWithin    = 25 * time.Second
	maxWaitWithin = 5 * time.Minute
)

// endedStates are a unit's final states; a unit in needs stopped on a
// question.
var endedStates = map[string]bool{"succeeded": true, "failed": true, "superseded": true, "accepted": true}

// handleWait is GET /jobs/{ns}/{name}/wait?unit=U[&unit=U...][&any=true]
// [&within=25s]: it answers when every unit named (or, with any, one of
// them) has ended, at once when any of them stops on a question (it
// waits on its answerer), or when within has passed, with each unit as it
// stands. A unit is named by its name or its index.
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	m, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	q := r.URL.Query()
	refs := q["unit"]
	if len(refs) == 0 {
		fail(w, http.StatusBadRequest, "name a unit to wait for: ?unit=<name or index>")
		return
	}
	anyOne := q.Get("any") == "true" || q.Get("any") == "1"
	within := waitWithin
	if v := q.Get("within"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			fail(w, http.StatusBadRequest, "within %q is not a duration (25s, 2m)", v)
			return
		}
		within = min(d, maxWaitWithin)
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	deadline := time.Now().Add(within)
	for {
		idx, bad := unitRefs(rec, refs)
		if bad != "" {
			fail(w, http.StatusNotFound, "no unit %s in job %s/%s", bad, ns, name)
			return
		}
		ans := waitAnswer(m, rec, idx, anyOne)
		if ans.Done || !time.Now().Before(deadline) {
			for k := range ans.Units {
				s.fillResult(r.Context(), m, rec, &ans.Units[k])
			}
			writeJSON(w, http.StatusOK, ans)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
		if m = s.current(); m == nil {
			continue
		}
		next, err := s.lookup(r.Context(), m, ns, name)
		if err != nil {
			continue // the next look may read it
		}
		if next == nil {
			fail(w, http.StatusNotFound, "job %s/%s is gone", ns, name)
			return
		}
		rec = next
	}
}

// unitRefs resolves units named by name, index or #index; bad is the first
// that names none.
func unitRefs(rec *jobRec, refs []string) (idx []int, bad string) {
	if rec.plan == nil {
		return nil, refs[0]
	}
	units := rec.plan.Units
	for _, ref := range refs {
		i, ok := -1, false
		for k := range units {
			if units[k].Name != "" && units[k].Name == ref {
				i, ok = k, true
				break
			}
		}
		if !ok {
			if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil && n >= 0 && n < len(units) {
				i, ok = n, true
			}
		}
		if !ok {
			return nil, ref
		}
		idx = append(idx, i)
	}
	return idx, ""
}

// waitAnswer is each named unit's state and whether the wait is over: at
// once when one stops on a question (it waits on its answerer); else, with
// anyOne, when one has ended, and without, when all have.
func waitAnswer(m *model, rec *jobRec, idx []int, anyOne bool) api.WaitAnswer {
	states := unitStateMap(&rec.job.Status, len(rec.plan.Units))
	var ans api.WaitAnswer
	asked, some, all := false, false, true
	for _, i := range idx {
		u := rec.plan.Units[i]
		res := api.UnitResult{Job: rec.job.Namespace + "/" + rec.job.Name, Unit: i, Name: u.Name, Label: u.Label, State: states[i]}
		switch {
		case endedStates[res.State]:
			res.Settled = "ended"
		case res.State == "needs":
			res.Settled = "asks"
			asked = true
		}
		some, all = some || res.Settled != "", all && res.Settled != ""
		ans.Units = append(ans.Units, res)
	}
	ans.Done = asked || (anyOne && some) || (!anyOne && all)
	return ans
}

// fillResult adds to a settled unit what a lead acts on: its summary (the
// whole of it, from the store), its branch, its run time and how it ended,
// or the question it stopped on.
func (s *Server) fillResult(ctx context.Context, m *model, rec *jobRec, res *api.UnitResult) {
	if res.Settled == "" {
		return
	}
	key := strconv.Itoa(res.Unit)
	st := &rec.job.Status
	if res.Unit < len(st.UnitMillis) && st.UnitMillis[res.Unit] > 0 {
		res.DurationSeconds = math.Round(float64(st.UnitMillis[res.Unit]) / 1000)
	}
	if b, ok := st.UnitBranches[key]; ok {
		res.Branch = &api.UnitBranch{Unit: res.Unit, Name: res.Name, Repo: b.Repo, Branch: b.Branch, Base: b.Base, Head: b.Head, Commits: int(b.Commits)}
	}
	if sm, ok := st.UnitSummaries[key]; ok {
		out := &api.UnitSummary{Excerpt: sm.Excerpt, By: "unit", Attempt: sm.Attempt, Doc: sm.Doc, Error: sm.Error}
		if text, err := s.summaryText(ctx, rec.project, sm); err == nil {
			out.Text = text
		} else if sm.Error == "" {
			out.TextError = err.Error()
		}
		res.Summary = out
	}
	if n, ok := st.Needs[key]; ok && res.Settled == "asks" {
		res.Question = &api.UnitQuestion{Kind: "needs", Text: n.Text, Why: n.Why, At: n.At.Time}
	}
	if p := unitPod(m, rec, res.Unit); p != nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
		res.Ended = &api.UnitEnded{Phase: string(p.Status.Phase), Reason: p.Status.Reason, Message: p.Status.Message}
	}
}

// unitPod is a unit's current pod in the HUD's picture, or nil.
func unitPod(m *model, rec *jobRec, i int) *corev1.Pod {
	var pod *corev1.Pod
	for k := range m.pods {
		p := &m.pods[k]
		if p.Namespace == rec.job.Namespace && p.Labels[c4v1.LabelJob] == rec.job.Name && p.Labels[c4v1.LabelUnit] == strconv.Itoa(i) {
			pod = p
		}
	}
	return pod
}
