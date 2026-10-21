package server

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// A host runner reports its machine's Claude Code sessions as its own node
// only. They are shown to its owners (its --owner users, several or one)
// who also own a pool its node is in, and to no one else: not another
// pool owner, not another person, not a farm admin, not as a count. A
// session that needs its person is in their needs-me list, answered in
// Claude Code, not by a farm control. A report that is not renewed goes.
func TestObservedSessions(t *testing.T) {
	now := time.Now().UTC()
	node := func(name, pool string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": pool}}}
	}
	m := &model{at: now, byKey: map[string]*jobRec{}, nsProject: map[string]string{},
		nodes: []corev1.Node{node("laptop-1", "agent-dana"), node("laptop-2", "shared"), node("studio-1", "studio")},
		pools: []c4v1.Pool{
			{ObjectMeta: metav1.ObjectMeta{Name: "agent-dana"}, Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "agent-dana"},
				Owners: []string{"dana"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "shared"}, Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "shared"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "studio"}, Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "studio"},
				Owners: []string{"dana", "lee", "sam"}}}}}
	host := func(n string) *api.Viewer { return &api.Viewer{User: HostAccountPrefix + n} }
	s := &Server{Loc: time.UTC, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), m: m, DevViewers: map[string]*api.Viewer{
		"host1": host("laptop-1"), "host2": host("laptop-2"), "host3": host("studio-1"),
		"dana":  {User: "dana", Projects: []string{}},
		"sam":   {User: "sam", Projects: []string{}},
		"lee":   {User: "lee", Projects: []string{}},
		"maya":  {User: "maya", Projects: []string{}},
		"admin": {User: "root", Projects: []string{}, All: true, Role: "farm-admin"}}}
	post := func(as, node string, rep api.ObservedReport) (int, string) {
		b, _ := json.Marshal(rep)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+node+"/observed?as="+as, bytes.NewReader(b)))
		return w.Code, w.Body.String()
	}
	active := now.Add(-2 * time.Minute)
	rep := api.ObservedReport{Machine: "Dana's MacBook Pro", ObservedAt: now, Owners: []string{"dana"}, Sessions: []api.ObservedSession{
		{Session: "s-1", Name: "refactor", Kind: "background", Running: true, Word: "needs you",
			NeedsYou: "Should I drop the old API or keep a shim?", Cwd: "/Users/dana/src/app", LastActive: &active},
		{Session: "s-2", Kind: "interactive", Word: "working", OnFarm: true}}}

	if code, out := post("host1", "laptop-1", rep); code != http.StatusOK || !strings.Contains(out, `"shownTo":["dana"]`) {
		t.Fatalf("its own node: %d %s", code, out)
	}
	for _, c := range []struct{ as, node string }{{"host1", "laptop-2"}, {"dana", "laptop-1"}, {"admin", "laptop-1"}} {
		if code, out := post(c.as, c.node, rep); code != http.StatusForbidden {
			t.Errorf("%s posting for %s: %d %s, want 403", c.as, c.node, code, out)
		}
	}
	// A node in no pool its owners own shows its rows to no one, and says
	// so; so does a runner that names no owner.
	if code, out := post("host2", "laptop-2", rep); code != http.StatusOK || !strings.Contains(out, `"shownTo":[]`) ||
		!strings.Contains(out, "no pool its owners own") {
		t.Fatalf("a node no one owns: %d %s", code, out)
	}
	if code, out := post("host3", "studio-1", api.ObservedReport{Machine: "studio-1", Sessions: rep.Sessions}); code != http.StatusOK ||
		!strings.Contains(out, `"shownTo":[]`) || !strings.Contains(out, "--owner") {
		t.Fatalf("a runner with no owner: %d %s", code, out)
	}
	// A shared machine: two owners both see its rows; a third owner of the
	// pool, not the runner's, does not.
	shared := api.ObservedReport{Machine: "studio-1", ObservedAt: now, Owners: []string{"sam", "dana"},
		Sessions: []api.ObservedSession{{Session: "s-9", Kind: "background", Word: "working"}}}
	if code, out := post("host3", "studio-1", shared); code != http.StatusOK || !strings.Contains(out, `"shownTo":["dana","sam"]`) {
		t.Fatalf("two owners: %d %s", code, out)
	}
	for as, want := range map[string]int{"sam": 1, "lee": 0} {
		var snap api.Snapshot
		get(t, s, "/api/v1/snapshot?as="+as, &snap)
		if len(snap.Observed) != want {
			t.Errorf("%s sees %d sessions, want %d: %+v", as, len(snap.Observed), want, snap.Observed)
		}
	}
	many := api.ObservedReport{Machine: "x"}
	for i := range observedMax + 1 {
		many.Sessions = append(many.Sessions, api.ObservedSession{Session: fmt.Sprint(i), Word: "idle"})
	}
	if code, _ := post("host1", "laptop-1", many); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d sessions taken: %d", len(many.Sessions), code)
	}

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=dana", &snap)
	if len(snap.Observed) != 3 {
		t.Fatalf("dana sees %d sessions: %+v", len(snap.Observed), snap.Observed)
	}
	if o := snap.Observed[0]; o.Node != "laptop-1" || o.Machine != "Dana's MacBook Pro" || len(o.Owners) != 1 || o.PostedAt.IsZero() ||
		o.NeedsYou == "" || o.Cwd == "" {
		t.Fatalf("row %+v", o)
	}
	for _, as := range []string{"maya", "admin", "lee"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?as="+as, nil))
		if strings.Contains(w.Body.String(), `"observed"`) || strings.Contains(w.Body.String(), "drop the old API") {
			t.Fatalf("%s sees dana's sessions: %s", as, w.Body.String())
		}
	}

	var mine api.NeedsMe
	get(t, s, "/api/v1/needs-me?as=dana", &mine)
	if len(mine.Items) != 1 {
		t.Fatalf("dana's needs-me: %+v", mine.Items)
	}
	if it := mine.Items[0]; it.Kind != "session" || it.ID != "session:laptop-1/s-1" || len(it.Actions) != 0 ||
		it.Open != "claude --resume s-1" || it.Ask != "Should I drop the old API or keep a shim?" ||
		it.Title != "refactor on Dana's MacBook Pro needs you" {
		t.Fatalf("the session's item %+v", it)
	}
	var hers api.NeedsMe
	get(t, s, "/api/v1/needs-me?as=maya", &hers)
	if len(hers.Items) != 0 {
		t.Fatalf("maya is asked about dana's session: %+v", hers.Items)
	}

	// A runner that stops reporting leaves nothing behind.
	s.obsMu.Lock()
	r := s.observed["laptop-1"]
	r.postedAt = now.Add(-observedFor - time.Minute)
	s.observed["laptop-1"] = r
	s.obsMu.Unlock()
	snap = api.Snapshot{}
	get(t, s, "/api/v1/snapshot?as=dana", &snap)
	if len(snap.Observed) != 1 || snap.Observed[0].Node != "studio-1" {
		t.Fatalf("a stale report is still shown: %+v", snap.Observed)
	}
}

// A machine's own sessions in use (working, needing their person, at a
// usage limit) are counted on its node and pool as used outside the queue,
// for anyone who sees the node, as a bare count. They are never slots: the
// pool's slots and use, which the queue plans on, do not move.
func TestUsedOutsideTheQueue(t *testing.T) {
	m := &model{at: time.Now().UTC(), byKey: map[string]*jobRec{}, nsProject: map[string]string{},
		nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1", Labels: map[string]string{"pool": "agent-dana"}}}},
		pools: []c4v1.Pool{{ObjectMeta: metav1.ObjectMeta{Name: "agent-dana"},
			Spec:   c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "agent-dana"}, Owners: []string{"dana"}, SlotsPerNode: 4},
			Status: c4v1.PoolStatus{Slots: 4, Used: 1}}}}
	s := &Server{Loc: time.UTC, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), m: m, DevViewers: map[string]*api.Viewer{
		"host": {User: HostAccountPrefix + "laptop-1"},
		"maya": {User: "maya", Projects: []string{}}}}
	look := func() (api.Pool, api.Node) {
		var snap api.Snapshot
		get(t, s, "/api/v1/snapshot?as=maya", &snap)
		if len(snap.Pools) != 1 || len(snap.Nodes) != 1 {
			t.Fatalf("pools %+v nodes %+v", snap.Pools, snap.Nodes)
		}
		return snap.Pools[0], snap.Nodes[0]
	}
	poolBefore, nodeBefore := look()

	rep := api.ObservedReport{Machine: "Dana's MacBook Pro", ObservedAt: time.Now(), Owners: []string{"dana"}, Sessions: []api.ObservedSession{
		{Session: "a", Word: "working"}, {Session: "b", Word: "needs you"}, {Session: "c", Word: "at its limit"}, {Session: "d", Word: "idle"}}}
	b, _ := json.Marshal(rep)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/laptop-1/observed?as=host", bytes.NewReader(b)))
	if w.Code != http.StatusOK {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}

	pool, node := look()
	if node.UsedOutside != 3 || pool.UsedOutside != 3 {
		t.Fatalf("used outside: node %d, pool %d, want 3", node.UsedOutside, pool.UsedOutside)
	}
	if pool.Slots != poolBefore.Slots || pool.Used != poolBefore.Used || node.Slots != nodeBefore.Slots || node.Used != nodeBefore.Used {
		t.Fatalf("the sessions moved slots: pool %d/%d (was %d/%d), node %d/%d (was %d/%d)", pool.Used, pool.Slots,
			poolBefore.Used, poolBefore.Slots, node.Used, node.Slots, nodeBefore.Used, nodeBefore.Slots)
	}
}
