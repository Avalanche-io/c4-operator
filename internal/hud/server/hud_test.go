package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// farm is a small farm of two projects: show-a renders a shot (four
// frames done, two running, four waiting) and has a job the queue has not
// estimated yet; show-b renders on the same nodes.
func farm(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	ago := func(s int) *metav1.Time { return &metav1.Time{Time: now.Add(-time.Duration(s) * time.Second)} }
	render := c4v1.WorkJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh040-light-v007", Labels: map[string]string{LabelShot: "sh040"},
			CreationTimestamp: metav1.Time{Time: now.Add(-time.Hour)}},
		Spec: c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1001-1010", "chunk": "1"}},
		Status: c4v1.WorkJobStatus{Phase: "Running", Order: "binary",
			Units:      c4v1.UnitCounts{Total: 10, Succeeded: 4, Running: 2, Queued: 3, Failed: 1},
			UnitStates: map[string]string{"succeeded": "0-3", "running": "4-5", "queued": "6-8", "failed": "9"},
			UnitMillis: []int64{100000, 120000, 110000, 130000, 0, 0, 0, 0, 0, 40000},
			Started:    ago(1800),
			Estimate:   &c4v1.WorkEstimate{RemainingWork: 700, RemainingWorkP90: 900, ETA50: ago(-600), ETA90: ago(-900), Feasible: true},
			Decisions: []c4v1.WorkDecision{{Unit: 5, At: *ago(300), Kind: "yield", Reason: "sh020 misses Fri 18:00 at current capacity",
				For: "show-a/sh020", Record: "c4decision"}}},
	}
	fresh := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh050-comp-v001", CreationTimestamp: metav1.Time{Time: now}},
		Spec:   c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-48", "chunk": "4"}},
		Status: c4v1.WorkJobStatus{Units: c4v1.UnitCounts{Total: 12, Succeeded: 2, Running: 1, Queued: 9}}}
	other := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "ep104-crowd", CreationTimestamp: metav1.Time{Time: now}},
		Spec:   c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-4"}},
		Status: c4v1.WorkJobStatus{Phase: "Running", Started: ago(600), Units: c4v1.UnitCounts{Total: 4, Running: 1, Queued: 3}}}

	pod := func(ns, job string, unit, startedAgo int, node string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: job + "-u" + string(rune('0'+unit)),
			Labels: map[string]string{c4v1.LabelJob: job, c4v1.LabelUnit: string(rune('0' + unit)), c4v1.LabelPool: "cpu"}},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "u", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"c4.avalanche.io/slots": resource.MustParse("1")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: ago(startedAgo)}}
	}
	node := func(name, zone, region string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"c4.avalanche.io/pool": "cpu",
			corev1.LabelTopologyZone: zone, corev1.LabelTopologyRegion: region}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	}
	m := &model{at: now, byKey: map[string]*jobRec{}, nsProject: map[string]string{"show-a": "show-a", "show-b": "show-b"},
		projects: []c4v1.Project{
			{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-a"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "show-b"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-b"}, Root: "/mnt/shows/b"}}},
		pools: []c4v1.Pool{{ObjectMeta: metav1.ObjectMeta{Name: "cpu"},
			Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"c4.avalanche.io/pool": "cpu"}, SlotsPerNode: 4}}},
		nodes: []corev1.Node{node("w1", "burbank-r1", "burbank"), node("w2", "us-west-2a", "us-west-2")},
		pods: []corev1.Pod{pod("show-a", render.Name, 4, 50, "w1"), pod("show-a", render.Name, 5, 200, "w2"),
			pod("show-a", fresh.Name, 2, 30, "w1"), pod("show-b", other.Name, 0, 60, "w2")},
		crs: []c4v1.CapacityRequest{
			{ObjectMeta: metav1.ObjectMeta{Name: "spot-a", CreationTimestamp: *ago(400)},
				Spec: c4v1.CapacityRequestSpec{Pool: "spot", Slots: 8, Reason: "sh040 misses", Jobs: []string{"show-a/sh040-light-v007"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "spot-b", CreationTimestamp: *ago(350)},
				Spec: c4v1.CapacityRequestSpec{Pool: "spot", Slots: 4, Reason: "ep104 misses", Jobs: []string{"show-b/ep104-crowd"}}}},
	}
	for _, j := range []c4v1.WorkJob{render, fresh, other} {
		d, err := class.Lookup(j.Spec.Class)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := d.Plan(&j)
		if err != nil {
			t.Fatal(err)
		}
		rec := &jobRec{job: j, key: j.Namespace + "/" + j.Name, project: j.Namespace, plan: plan, driver: d}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}

	ev := func(ns, kind, name, reason, msg, comp string, at int, anno map[string]string) runtime.Object {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-" + reason, Annotations: anno},
			InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: ns}, Reason: reason, Message: msg,
			Source: corev1.EventSource{Component: comp, Host: "w1"}, LastTimestamp: *ago(at)}
	}
	kube := kfake.NewSimpleClientset(
		ev("show-a", "WorkJob", render.Name, "Nudged", render.Name+": held (by maya in the HUD)", "c4-hud", 700,
			map[string]string{annoActor: "agent/wrangler-bot for maya", annoBookKind: "hold", annoRecord: "c4control"}),
		ev("show-b", "WorkJob", other.Name, "Nudged", other.Name+": boost 1.5 (by jo in the HUD)", "c4-hud", 500,
			map[string]string{annoActor: "jo", annoBookKind: "boost"}),
		ev("default", "Node", "w2", "NodeNotReady", "Node w2 status is now: NodeNotReady", "kubelet", 900, nil),
		ev("c4-system", "CapacityRequest", "spot-b", "Approved", "spot: 4 slots approved for show-b/ep104-crowd", "c4-hud", 200,
			map[string]string{annoActor: "jo", annoBookKind: "capacity-approved"}),
	)
	return &Server{Kube: kube, Namespace: "c4-system", Loc: time.UTC, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), m: m,
		Sites:          []api.Site{{Name: "burbank", Label: "Burbank", Region: "burbank", Kind: "onprem", Localities: []string{"burbank-r1"}}},
		DevViewersOpen: true,
		DevViewers: map[string]*api.Viewer{
			"wrangler": {User: "dana", Projects: []string{"show-a", "show-b"}, All: true},
			"artist":   {User: "maya", Projects: []string{"show-a"}}}}
}

func get(t *testing.T, s *Server, path string, v any) int {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if v != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func TestWorkInSnapshot(t *testing.T) {
	s := farm(t)
	var snap api.Snapshot
	if code := get(t, s, "/api/v1/snapshot?as=wrangler", &snap); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	jobs := map[string]api.Job{}
	for _, j := range snap.Jobs {
		jobs[j.Name] = j
		w := j.Work
		if w.DoneSlotSeconds+w.RunningSlotSeconds+w.RemainingSlotSeconds <= 0 {
			t.Errorf("%s has no work: %+v", j.Name, w)
		}
	}
	w := jobs["sh040-light-v007"].Work
	if w.Basis != api.BasisEstimate || w.DoneSlotSeconds != 460 || w.RunningSlotSeconds != 250 ||
		w.RemainingSlotSeconds != 700 || w.P90RemainingSlotSeconds != 900 {
		t.Errorf("estimated work %+v", w)
	}
	// The mean measured unit is 115 s: 65 s left of the unit 50 s in, and
	// a quarter of its elapsed time for the unit 200 s in.
	if w.RunningRemainingSlotSeconds != 115 {
		t.Errorf("running remainder %v, want 115", w.RunningRemainingSlotSeconds)
	}
	if jobs["sh040-light-v007"].Shot != "sh040" {
		t.Errorf("shot %q", jobs["sh040-light-v007"].Shot)
	}
	u := jobs["sh050-comp-v001"].Work
	if u.Basis != api.BasisUnits || u.DoneSlotSeconds != 2 || u.RunningSlotSeconds != 1 || u.RunningRemainingSlotSeconds != 0 ||
		u.RemainingSlotSeconds != 9 || u.P90RemainingSlotSeconds != 9 {
		t.Errorf("unit-based work %+v", u)
	}
}

func TestUnitWorkInDetail(t *testing.T) {
	s := farm(t)
	var d map[string]any
	if code := get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=artist", &d); code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if d["shot"] != "sh040" {
		t.Errorf("shot %v", d["shot"])
	}
	w := d["work"].(map[string]any)
	if w["runningRemainingSlotSeconds"].(float64) <= 0 || w["runningRemainingSlotSeconds"].(float64) > w["remainingSlotSeconds"].(float64) {
		t.Errorf("detail work %v", w)
	}
	for _, x := range d["unitList"].([]any) {
		u := x.(map[string]any)
		uw := u["work"].(map[string]any)
		spent, exp := uw["spent"].(float64), uw["expected"].(float64)
		switch u["state"] {
		case "succeeded":
			if spent <= 0 || spent != exp {
				t.Errorf("unit %v: %v", u["i"], uw)
			}
		case "running":
			if spent <= 0 || exp <= spent {
				t.Errorf("unit %v: %v", u["i"], uw)
			}
		case "queued":
			if spent != 0 || exp <= 0 {
				t.Errorf("unit %v: %v", u["i"], uw)
			}
		case "failed":
			if spent != 40 {
				t.Errorf("unit %v: %v", u["i"], uw)
			}
		}
		// The queue plans every unit: nothing was revealed by a generator,
		// and per-unit reads and finish times are not exported.
		for _, k := range []string{"parent", "reads", "eta"} {
			if _, ok := u[k]; ok {
				t.Errorf("unit %v has %s", u["i"], k)
			}
		}
	}
	for _, g := range d["groups"].([]any) {
		if _, ok := g.(map[string]any)["parent"]; ok {
			t.Errorf("group %v has a parent", g)
		}
	}
	if b := d["axis"].(map[string]any)["batch"]; b != 24.0 {
		t.Errorf("axis.batch %v, want 24", b)
	}
	get(t, s, "/api/v1/jobs/show-a/sh050-comp-v001?as=artist", &d)
	if b := d["axis"].(map[string]any)["batch"]; b != 4.0 {
		t.Errorf("a chunked job's axis.batch %v, want its chunk, 4", b)
	}
	var g map[string]any
	get(t, s, "/api/v1/graph?as=artist", &g)
	if n := g["nodes"].([]any)[0].(map[string]any); n["shot"] != "sh040" {
		t.Errorf("graph node %v", n)
	}
}

func TestBookScope(t *testing.T) {
	s := farm(t)
	var all api.Book
	if code := get(t, s, "/api/v1/book?as=wrangler", &all); code != 200 {
		t.Fatalf("book: %d", code)
	}
	kinds := map[string]api.BookEntry{}
	for i, e := range all.Entries {
		kinds[e.Kind] = e
		if i > 0 && (e.At.Before(all.Entries[i-1].At) || e.Seq <= all.Entries[i-1].Seq) {
			t.Errorf("entry %d out of order", i)
		}
	}
	for _, k := range []string{"hold", "boost", "node-not-ready", "capacity-requested", "capacity-approved", "yield", "started"} {
		if _, ok := kinds[k]; !ok {
			t.Errorf("no %s entry in %+v", k, all.Entries)
		}
	}
	if h := kinds["hold"]; h.Actor != "agent/wrangler-bot for maya" || h.Signer != api.SignerHUD || h.Record != "c4control" {
		t.Errorf("hold %+v", h)
	}
	if n := kinds["node-not-ready"]; n.Signer != api.SignerNode || n.Object.Node != "w2" {
		t.Errorf("node event %+v", n)
	}
	if y := kinds["yield"]; y.Signer != api.SignerQueue || y.Record != "c4decision" || y.Object.Unit == nil || *y.Object.Unit != 5 {
		t.Errorf("decision %+v", y)
	}

	var art api.Book
	get(t, s, "/api/v1/book?as=artist", &art)
	nodeSeen := false
	for _, e := range art.Entries {
		if strings.HasPrefix(e.Object.Job, "show-b/") || strings.Contains(e.Message, "ep104") {
			t.Errorf("the artist sees another project's entry: %+v", e)
		}
		nodeSeen = nodeSeen || e.Object.Type == "node"
	}
	if !nodeSeen || len(art.Entries) >= len(all.Entries) {
		t.Errorf("artist book %+v", art.Entries)
	}
	if code := get(t, s, "/api/v1/book?job=show-b/ep104-crowd&as=artist", nil); code != 404 {
		t.Errorf("another project's job: %d, want 404", code)
	}
	var one api.Book
	get(t, s, "/api/v1/book?job=show-a/sh040-light-v007&limit=2&as=artist", &one)
	if len(one.Entries) != 2 {
		t.Fatalf("%d entries", len(one.Entries))
	}
	for _, e := range one.Entries {
		if e.Object.Job != "show-a/sh040-light-v007" {
			t.Errorf("job book has %+v", e)
		}
	}
}

func TestZonesAndSites(t *testing.T) {
	s := farm(t)
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=artist", &snap)
	got := map[string][2]string{}
	for _, n := range snap.Nodes {
		got[n.Name] = [2]string{n.Zone, n.Region}
	}
	if got["w1"] != [2]string{"burbank-r1", "burbank"} || got["w2"] != [2]string{"us-west-2a", "us-west-2"} {
		t.Errorf("zones and regions %v", got)
	}
	if len(snap.Sites) != 1 || snap.Sites[0].Region != "burbank" {
		t.Errorf("sites %+v", snap.Sites)
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "sites.json")
	os.WriteFile(good, []byte(`[{"name":"burbank","region":"burbank","lat":34.18,"lon":-118.31,"kind":"onprem","localities":["rack r1"],"tz":"America/Los_Angeles"},
		{"name":"aws-us-west-2","region":"us-west-2","lat":45.8,"lon":-119.7,"kind":"cloud"}]`), 0o600)
	sites, err := LoadSites(good)
	if err != nil || len(sites) != 2 || sites[1].Label != "aws-us-west-2" || sites[1].Localities == nil || sites[0].TZ != "America/Los_Angeles" {
		t.Fatalf("LoadSites: %+v %v", sites, err)
	}
	badTZ := filepath.Join(dir, "badtz.json")
	os.WriteFile(badTZ, []byte(`[{"name":"x","region":"x","kind":"onprem","tz":"Pacific/Atlantis"}]`), 0o600)
	if _, err := LoadSites(badTZ); err == nil {
		t.Error("a site in no real time zone loaded")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`[{"name":"x","region":"x","kind":"moon"}]`), 0o600)
	if _, err := LoadSites(bad); err == nil {
		t.Error("a site of kind moon loaded")
	}
}

func TestKebab(t *testing.T) {
	for in, want := range map[string]string{"NodeNotReady": "node-not-ready", "Nudged": "nudged", "OOMKilling": "oom-killing", "": ""} {
		if got := kebab(in); got != want {
			t.Errorf("kebab(%q) = %q, want %q", in, got, want)
		}
	}
}

// The HUD API writes every time in UTC; the browser shows local time.
func TestWireTimesAreUTC(t *testing.T) {
	s := farm(t)
	s.Loc, _ = time.LoadLocation("America/Los_Angeles")
	offset := regexp.MustCompile(`"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?[+-]\d\d:\d\d"`)
	for _, path := range []string{"/api/v1/snapshot?as=wrangler", "/api/v1/book?as=wrangler"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if m := offset.FindString(w.Body.String()); m != "" {
			t.Errorf("%s writes a time with an offset: %s", path, m)
		}
	}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	for _, j := range snap.Jobs {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+j.Namespace+"/"+j.Name+"?as=wrangler", nil))
		if m := offset.FindString(w.Body.String()); m != "" {
			t.Errorf("job %s/%s writes a time with an offset: %s", j.Namespace, j.Name, m)
		}
	}
}

func TestAgentSummaryInDetail(t *testing.T) {
	s := farm(t)
	now := s.m.at
	j := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "research",
		Labels: map[string]string{"c4.avalanche.io/agent-bundle": "research"}, CreationTimestamp: metav1.Time{Time: now.Add(-3 * time.Hour)}},
		Spec:   c4v1.WorkJobSpec{Class: "agent", Params: map[string]string{"members": "provenance,plan-jobs", "bundle": "research"}},
		Status: c4v1.WorkJobStatus{Phase: "Running", Units: c4v1.UnitCounts{Total: 2, Running: 1, Queued: 1}}}
	d, err := class.Lookup("agent")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(&j)
	if err != nil {
		t.Fatal(err)
	}
	rec := &jobRec{job: j, key: "show-a/research", project: "show-a", plan: plan, driver: d}
	s.m.jobs = append(s.m.jobs, rec)
	s.m.byKey[rec.key] = rec
	from, to := metav1.NewTime(now.Add(-2*time.Hour)), metav1.NewTime(now.Add(-time.Hour))
	start, vnow := metav1.NewTime(now.Add(-3*time.Hour)), metav1.NewTime(now.Add(-time.Hour))
	s.m.bundles = []c4v1.AgentBundle{{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "research"},
		Spec: c4v1.AgentBundleSpec{MaxRunning: 1, Members: []c4v1.AgentMemberSpec{{Name: "provenance"}, {Name: "plan-jobs", Kind: "task"}}},
		Status: c4v1.AgentBundleStatus{Phase: "Running", Members: []c4v1.AgentMemberStatus{
			{Name: "provenance", State: "running", Node: "c4-host-laptop-1", Epoch: "3"},
			{Name: "plan-jobs", State: "parked", Checkpoint: "s#7:c4abc"}},
			Clock: c4v1.AgentClock{Start: &start, VirtualNow: &vnow, DriftSeconds: 3600, Suspensions: []c4v1.AgentSuspension{{From: from, To: to}}}}}}
	var got map[string]any
	if code := get(t, s, "/api/v1/jobs/show-a/research?as=artist", &got); code != 200 {
		t.Fatalf("detail: %d", code)
	}
	cs := got["classSummary"].(map[string]any)
	if cs["bundle"] != "research" || cs["cap"] != 1.0 || cs["running"] != 1.0 {
		t.Fatalf("summary %v", cs)
	}
	ms := cs["members"].([]any)
	m0, m1 := ms[0].(map[string]any), ms[1].(map[string]any)
	if m0["state"] != "running" || m0["epoch"] != "3" || m0["kind"] != "resident" {
		t.Fatalf("member 0 %v", m0)
	}
	cp, _ := m1["checkpoint"].(map[string]any)
	if m1["state"] != "suspended" || cp["session"] != "s" || cp["seq"] != 7.0 || cp["id"] != "c4abc" || m1["kind"] != "task" {
		t.Fatalf("member 1 %v", m1)
	}
	c := cs["clock"].(map[string]any)
	if c["driftSeconds"] != 3600.0 || !strings.HasSuffix(c["virtualNow"].(string), "Z") {
		t.Fatalf("clock %v", c)
	}
	if sus := cs["suspended"].([]any); len(sus) != 1 || sus[0].(map[string]any)["to"] == nil {
		t.Fatalf("suspended %v", sus)
	}
	// A person's suspension: the parked member is held, not suspended.
	s.m.bundles[0].Spec.Suspend = true
	get(t, s, "/api/v1/jobs/show-a/research?as=artist", &got)
	if st := got["classSummary"].(map[string]any)["members"].([]any)[1].(map[string]any)["state"]; st != "held" {
		t.Fatalf("held member state %v", st)
	}
}

func TestCappedNodeInSnapshot(t *testing.T) {
	s := farm(t)
	s.m.nodes[0].Labels[c4v1.LabelSlotLimit] = "1"
	s.m.nodes[0].Annotations = map[string]string{c4v1.AnnotationCapacityReason: "battery 21% and falling on a 45 W adapter"}
	var snap map[string]any
	if code := get(t, s, "/api/v1/snapshot?as=wrangler", &snap); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	for _, x := range snap["nodes"].([]any) {
		n := x.(map[string]any)
		switch n["name"] {
		case "w1":
			if n["capped"] != true || n["slots"] != 1.0 || n["shapeSlots"] != 4.0 || n["capacityReason"] == "" {
				t.Fatalf("capped node %v", n)
			}
		case "w2":
			if _, ok := n["capped"]; ok {
				t.Fatalf("uncapped node %v", n)
			}
		}
	}
}

// A job the scheduler projects no ETA for (here, a suspended bundle with no
// deadline) reports the snapshot's time and the reason, never the zero time.
func TestNoETAInSnapshot(t *testing.T) {
	s := farm(t)
	now := s.m.at
	j := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "research",
		Labels: map[string]string{"c4.avalanche.io/agent-bundle": "research"}, CreationTimestamp: metav1.Time{Time: now.Add(-3 * time.Hour)}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Params: map[string]string{"members": "provenance", "bundle": "research"}},
		Status: c4v1.WorkJobStatus{Phase: "Running", Units: c4v1.UnitCounts{Total: 1, Queued: 1},
			Estimate: &c4v1.WorkEstimate{Feasible: true}}}
	d, err := class.Lookup("agent")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(&j)
	if err != nil {
		t.Fatal(err)
	}
	rec := &jobRec{job: j, key: "show-a/research", project: "show-a", plan: plan, driver: d}
	s.m.jobs = append(s.m.jobs, rec)
	s.m.byKey[rec.key] = rec
	s.m.bundles = []c4v1.AgentBundle{{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "research"},
		Status: c4v1.AgentBundleStatus{Phase: "Suspended"}}}
	var snap map[string]any
	if code := get(t, s, "/api/v1/snapshot?as=wrangler", &snap); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	for _, x := range snap["jobs"].([]any) {
		jj := x.(map[string]any)
		if jj["name"] != "research" {
			continue
		}
		e := jj["estimate"].(map[string]any)
		if e["noEta"] != "suspended: it resumes from its checkpoints when capacity returns" {
			t.Fatalf("noEta %v", e["noEta"])
		}
		if at, _ := time.Parse(time.RFC3339, e["etaP50"].(string)); !at.Equal(now) {
			t.Fatalf("etaP50 %v, want the snapshot's time %v", e["etaP50"], now)
		}
		return
	}
	t.Fatal("research not in the snapshot")
}

// A project's trees mount at its root: /show unless it sets one.
func TestProjectRootInSnapshot(t *testing.T) {
	s := farm(t)
	var snap api.Snapshot
	if code := get(t, s, "/api/v1/snapshot?as=wrangler", &snap); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	roots := map[string]string{}
	for _, p := range snap.Projects {
		roots[p.Name] = p.Root
	}
	if roots["show-a"] != "/show" || roots["show-b"] != "/mnt/shows/b" {
		t.Fatalf("roots %v", roots)
	}
}
