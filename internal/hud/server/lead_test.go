package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// leadFarm is a farm with dana's laptop in her own pool, its runner
// reporting as host, and people who are not its owners.
func leadFarm(t *testing.T, now time.Time) *Server {
	t.Helper()
	m := &model{at: now, byKey: map[string]*jobRec{}, nsProject: map[string]string{},
		nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1", Labels: map[string]string{"pool": "agent-dana"}}}},
		pools: []c4v1.Pool{{ObjectMeta: metav1.ObjectMeta{Name: "agent-dana"},
			Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "agent-dana"}, Owners: []string{"dana"}}}}}
	return &Server{Loc: time.UTC, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), m: m, DevViewers: map[string]*api.Viewer{
		"host":  {User: HostAccountPrefix + "laptop-1"},
		"dana":  {User: "dana", Projects: []string{}},
		"noor":  {User: "noor", Projects: []string{}},
		"admin": {User: "root", Projects: []string{}, All: true, Role: "farm-admin"}}}
}

func postObserved(t *testing.T, s *Server, sessions ...api.ObservedSession) {
	t.Helper()
	b, _ := json.Marshal(api.ObservedReport{Machine: "Dana's MacBook Pro", ObservedAt: time.Now(), Owners: []string{"dana"}, Sessions: sessions})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/laptop-1/observed?as=host", bytes.NewReader(b)))
	if w.Code != http.StatusOK {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}
}

// turnsOf lists turns first..last, each a minute long and ending a minute
// before the next, the last ending at end; the last takes state last.
func turnsOf(first, last int, end time.Time, state string) []api.ObservedTurn {
	var out []api.ObservedTurn
	for i := first; i <= last; i++ {
		e := end.Add(-time.Duration(last-i) * 2 * time.Minute)
		out = append(out, api.ObservedTurn{Index: i, Start: e.Add(-time.Minute), End: e, Tokens: 1000, Weighted: 1200, State: "done"})
	}
	out[len(out)-1].State = state
	return out
}

func leadJobs(t *testing.T, s *Server, as string) map[string]api.Job {
	t.Helper()
	var snap api.Snapshot
	if code := get(t, s, "/api/v1/snapshot?as="+as, &snap); code != http.StatusOK {
		t.Fatalf("snapshot as %s: %d", as, code)
	}
	out := map[string]api.Job{}
	for _, j := range snap.Jobs {
		if j.Observed {
			out[j.Name] = j
		}
	}
	return out
}

type leadDetail struct {
	api.Job
	Axis     map[string]any   `json:"axis"`
	Groups   []map[string]any `json:"groups"`
	UnitList []struct {
		I         int            `json:"i"`
		Pos       int            `json:"pos"`
		Group     string         `json:"group"`
		Label     string         `json:"label"`
		Name      string         `json:"name"`
		State     string         `json:"state"`
		Parent    *int           `json:"parent"`
		Generator bool           `json:"generator"`
		Generates map[string]any `json:"generates"`
		ETA       *time.Time     `json:"eta"`
	} `json:"unitList"`
}

// A session of 289 turns, its latest 200 listed, is a lead entry: a job of
// driver lead whose axis counts turns from the first listed (90), a turn
// generator group and a sparse per-row group of what each turn started,
// each worker in the row of the turn that started it (a nested one in its
// starter's row; one started before the listed turns in the first row).
// The 89 turns before the listed ones count as succeeded, so it reads 289
// turns, not 200. With no turn limit there are no turns to come and no
// ETA. The snapshot's own observed row leaves its turns to the entry.
func TestLeadEntryShape(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := leadFarm(t, now)
	started := now.Add(-10 * time.Hour)
	postObserved(t, s,
		api.ObservedSession{Session: "s-api", Name: "api-lead", Kind: "interactive", Running: true, Word: "working",
			StartedAt: &started, Account: "a1b2c3d4e5f6", Turns: turnsOf(90, 289, now.Add(time.Minute), "running"), TurnsTotal: 289},
		api.ObservedSession{Session: "w-tests", Name: "auth-tests", Kind: "subagent", Word: "done", Parent: "s-api", Turn: 95},
		api.ObservedSession{Session: "w-deep", Name: "fixture", Kind: "subagent", Word: "working", Parent: "w-tests", Turn: 3},
		api.ObservedSession{Session: "w-old", Name: "deps-bump", Kind: "subagent", Word: "done", Parent: "s-api", Turn: 12},
		api.ObservedSession{Session: "w-mate", Name: "reviewer", Kind: "teammate", Word: "waiting", WaitsOn: "person", Parent: "s-api", Turn: 289},
	)

	jobs := leadJobs(t, s, "dana")
	if len(jobs) != 1 {
		t.Fatalf("lead entries: %+v", jobs)
	}
	j := jobs["s-api"]
	if j.Namespace != "observed:laptop-1" || j.Driver != "lead" || j.Class != "lead" || j.Owner != "dana" || j.Label != "api-lead" ||
		j.Account != "a1b2c3d4e5f6" || j.TurnsTotal != 289 || j.Phase != "Running" || j.Rank != 0 || j.Order != "sequential" {
		t.Fatalf("entry %+v", j)
	}
	// 89 earlier and 199 listed turns done, one running; two workers done
	// of four, one working, one at its person's prompt
	u := j.Units
	if u.Total != 89+200+4 || u.Succeeded != 89+199+2 || u.Running != 2 || u.Needs != 1 || u.Planned != 0 || u.Unevaluated != 0 {
		t.Fatalf("counts %+v", u)
	}
	if j.UnitStates.Running != "199,201" || j.UnitStates.Needs != "203" {
		t.Fatalf("unit states %+v", j.UnitStates)
	}
	if j.Estimate.NoETA == "" || !j.Estimate.EtaP50.Equal(now) || j.Estimate.SlackSeconds != 0 || j.Estimate.Risk != "" {
		t.Fatalf("no limit, no ETA: %+v", j.Estimate)
	}

	var d leadDetail
	if code := get(t, s, "/api/v1/jobs/observed:laptop-1/s-api?as=dana", &d); code != http.StatusOK {
		t.Fatalf("its page: %d", code)
	}
	if d.Axis["kind"] != "turn" || d.Axis["start"] != float64(90) || d.Axis["step"] != float64(1) {
		t.Fatalf("axis %+v", d.Axis)
	}
	want := `[{"generator":true,"key":"turn","label":"turns","spans":["work"],"units":"0-199"},` +
		`{"key":"work","label":"what each turn started","parent":"turn","perBlock":true,"sparse":true,"units":"200-203"}]`
	if got, _ := json.Marshal(d.Groups); string(got) != want {
		t.Fatalf("groups\n got %s\nwant %s", got, want)
	}
	if len(d.UnitList) != 204 {
		t.Fatalf("%d units", len(d.UnitList))
	}
	first, last := d.UnitList[0], d.UnitList[199]
	if first.Label != "turn 90" || first.State != "succeeded" || !first.Generator || last.Label != "turn 289" || last.State != "running" {
		t.Fatalf("turns: %+v ... %+v", first, last)
	}
	pos := map[string]int{}
	for _, x := range d.UnitList[200:] {
		if x.Group != "work" || x.Parent == nil || *x.Parent != x.Pos || d.UnitList[x.Pos].Group != "turn" {
			t.Fatalf("worker %+v", x)
		}
		pos[x.Label] = x.Pos
	}
	if pos["auth-tests"] != 5 || pos["fixture"] != 5 || pos["deps-bump"] != 0 || pos["reviewer"] != 199 {
		t.Fatalf("worker rows %v", pos)
	}

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=dana", &snap)
	for _, o := range snap.Observed {
		if len(o.Turns) != 0 {
			t.Fatalf("row %s carries %d turns", o.Session, len(o.Turns))
		}
	}
}

// A turn's state is the queue's word for it: done turns succeeded, the
// turn in flight running, a turn that ended on a call with no result needs
// its person, or waits on its lead, or failed with its session. A session
// at its usage limit is queued until the reset.
func TestLeadEntryStates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := leadFarm(t, now)
	resets := now.Add(35 * time.Minute)
	limit, _ := json.Marshal(map[string]any{"kind": "session", "text": "limit reached", "resets": resets})
	postObserved(t, s,
		api.ObservedSession{Session: "s-run", Kind: "interactive", Running: true, Word: "working", Turns: turnsOf(1, 3, now, "running"), TurnsTotal: 3},
		api.ObservedSession{Session: "s-ask", Kind: "interactive", Running: true, Word: "needs you", Turns: turnsOf(1, 3, now, "waiting"), TurnsTotal: 3},
		api.ObservedSession{Session: "s-lead", Kind: "interactive", Running: true, Word: "idle", WaitsOn: "lead", Turns: turnsOf(1, 3, now, "waiting"), TurnsTotal: 3},
		api.ObservedSession{Session: "s-err", Kind: "interactive", Word: "failed", Turns: turnsOf(1, 3, now, "waiting"), TurnsTotal: 3},
		api.ObservedSession{Session: "s-limit", Kind: "background", Running: true, Word: "at its limit", Limit: limit,
			Turns: turnsOf(1, 3, now, "done"), TurnsTotal: 3},
	)
	jobs := leadJobs(t, s, "dana")
	for name, want := range map[string]struct {
		phase string
		last  string
	}{
		"s-run":   {"Running", "running"},
		"s-ask":   {"Running", "needs"},
		"s-lead":  {"Running", "waits"},
		"s-err":   {"Failed", "failed"},
		"s-limit": {"Queued", "succeeded"},
	} {
		j := jobs[name]
		var d leadDetail
		get(t, s, "/api/v1/jobs/observed:laptop-1/"+name+"?as=dana", &d)
		if j.Phase != want.phase || len(d.UnitList) != 3 || d.UnitList[0].State != "succeeded" || d.UnitList[2].State != want.last {
			t.Errorf("%s: phase %s, units %+v; want %s, last %s", name, j.Phase, d.UnitList, want.phase, want.last)
		}
	}
	if j := jobs["s-ask"]; j.Units.Needs != 1 || j.UnitStates.Needs != "2" || j.Units.Succeeded != 2 {
		t.Errorf("s-ask counts %+v %+v", j.Units, j.UnitStates)
	}
	if e := jobs["s-limit"].Estimate; e.NoETA == "" || !strings.Contains(e.Message, "usage limit to reset at") {
		t.Errorf("at its limit, no turn limit: %+v", e)
	}
}

// With a turn limit the turns to come are planned, the first saying how
// many come, and the ETA is the turns left at the measured time per turn;
// at a usage limit it starts from the reset.
func TestLeadEntryTurnLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := leadFarm(t, now)
	resets := now.Add(time.Hour)
	limit, _ := json.Marshal(map[string]any{"kind": "session", "text": "limit reached", "resets": resets})
	postObserved(t, s,
		api.ObservedSession{Session: "s-lane", Kind: "background", Running: true, Word: "idle", TurnLimit: 20,
			Turns: turnsOf(1, 10, now.Add(-time.Minute), "done"), TurnsTotal: 10},
		api.ObservedSession{Session: "s-held", Kind: "background", Running: true, Word: "at its limit", Limit: limit, TurnLimit: 20,
			Turns: turnsOf(1, 10, now.Add(-time.Minute), "done"), TurnsTotal: 10},
	)
	jobs := leadJobs(t, s, "dana")
	j := jobs["s-lane"]
	if j.Units.Planned != 10 || j.Units.Unevaluated != 10 || j.UnitStates.Planned != "10-19" || j.Units.Total != 20 {
		t.Fatalf("planned turns %+v %+v", j.Units, j.UnitStates)
	}
	// ten turns of a minute each left
	if e := j.Estimate; e.NoETA != "" || e.RemainingSeconds != 600 || !e.EtaP50.Equal(now.Add(10*time.Minute)) || !e.EtaP90.Equal(now.Add(700*time.Second)) {
		t.Fatalf("ETA %+v", e)
	}
	var d leadDetail
	get(t, s, "/api/v1/jobs/observed:laptop-1/s-lane?as=dana", &d)
	next := d.UnitList[10]
	if next.State != "planned" || next.Label != "turn 11" || next.Generates["low"] != float64(10) || next.Generates["from"] != "its turn limit" ||
		next.ETA == nil || !next.ETA.Equal(now.Add(time.Minute)) {
		t.Fatalf("the next turn %+v", next)
	}
	h := jobs["s-held"].Estimate
	if !h.EtaP50.Equal(resets.Add(10*time.Minute)) || !strings.Contains(h.Message, "reset at") || !strings.Contains(h.Message, "about 10 min of turns") {
		t.Fatalf("from the reset: %+v", h)
	}
}

// A lead entry has its session's visibility: the runner's owner sees it,
// in the snapshot and on its page; no one else does, a farm admin
// included, and its page answers 404 as for a job they may not see.
func TestLeadEntryOwnerOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := leadFarm(t, now)
	postObserved(t, s, api.ObservedSession{Session: "s-api", Name: "api-lead", Kind: "interactive", Running: true, Word: "working",
		Turns: turnsOf(1, 3, now, "running"), TurnsTotal: 3})
	if jobs := leadJobs(t, s, "dana"); len(jobs) != 1 {
		t.Fatalf("dana's entries %+v", jobs)
	}
	for _, as := range []string{"noor", "admin"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?as="+as, nil))
		if body := w.Body.String(); w.Code != http.StatusOK || strings.Contains(body, "s-api") || strings.Contains(body, `"observed"`) ||
			strings.Contains(body, `"lead"`) {
			t.Fatalf("%s sees dana's session: %d %s", as, w.Code, body)
		}
		if code := get(t, s, "/api/v1/jobs/observed:laptop-1/s-api?as="+as, nil); code != http.StatusNotFound {
			t.Fatalf("%s opens its page: %d", as, code)
		}
	}
	if code := get(t, s, "/api/v1/jobs/observed:laptop-1/s-none?as=dana", nil); code != http.StatusNotFound {
		t.Fatalf("a session not reported: %d", code)
	}
}
