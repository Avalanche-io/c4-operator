package server

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

func trackers(t *testing.T, s *Server, as string) map[string]api.Tracker {
	t.Helper()
	var out api.TrackerList
	if code := call(t, s, http.MethodGet, "/api/v1/trackers?as="+as, nil, &out); code != http.StatusOK {
		t.Fatalf("trackers as %s: %d", as, code)
	}
	m := map[string]api.Tracker{}
	for _, tr := range out.Trackers {
		m[tr.Namespace+"/"+tr.Name] = tr
	}
	return m
}

// A person sees the trackers in their projects, in words: what each
// connects to and turns into jobs, whether it is connected and why not,
// how many people's grants serve it, and their own.
func TestTrackersList(t *testing.T) {
	s := grantFarm(t)
	ctx := context.Background()
	now := time.Now()
	var gh c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "github"}, &gh); err != nil {
		t.Fatal(err)
	}
	gh.Spec.Scope = map[string]string{"repo": "petrelfinch/pipeline"}
	gh.Spec.Intake = []c4v1.TrackerIntake{
		{Match: c4v1.TrackerMatch{Type: "PullRequest"}, As: "job"},
		{Match: c4v1.TrackerMatch{Type: "Issue", Labels: []string{"render"}}, As: "job"},
		{Match: c4v1.TrackerMatch{Type: "Milestone"}, As: "milestone"},
	}
	gh.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "SyncFailed",
		Message: "401 Bad credentials", LastTransitionTime: metav1.NewTime(now)}}
	if err := s.Client.Update(ctx, &gh); err != nil {
		t.Fatal(err)
	}
	resynced := metav1.NewTime(now.Add(-time.Minute))
	stale := metav1.NewTime(now.Add(-2 * time.Hour))
	ready := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: metav1.NewTime(now)}}
	for _, tr := range []*c4v1.Tracker{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "saltmarsh-flow"},
			Spec: c4v1.TrackerSpec{Kind: "flow", Endpoint: "https://saltmarsh.shotgrid.autodesk.com", Scope: map[string]string{"project": "122"},
				Intake: []c4v1.TrackerIntake{{Match: c4v1.TrackerMatch{Type: "Task", Fields: map[string]string{"step.Step.short_name": "LGT"}},
					As: "job", For: &c4v1.TrackerFor{Assignee: true}}}},
			Status: c4v1.TrackerStatus{Conditions: ready, LastResync: &resynced}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "linear"},
			Spec:   c4v1.TrackerSpec{Kind: "linear", Scope: map[string]string{"team": "Pipeline"}},
			Status: c4v1.TrackerStatus{Conditions: ready, LastResync: &stale}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "github"}, Spec: c4v1.TrackerSpec{Kind: "github"}},
	} {
		if err := s.Client.Create(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	// maya and lee let the Flow tracker submit for them; lee lets GitHub
	// too, then takes it back.
	for _, as := range []string{"artist", "lee"} {
		var out api.GrantResponse
		if code := call(t, s, http.MethodPost, "/api/v1/grants?as="+as, api.GrantRequest{Project: "show-a", Tracker: "saltmarsh-flow"}, &out); code != http.StatusOK {
			t.Fatalf("grant as %s: %d %+v", as, code, out)
		}
	}
	var out api.GrantResponse
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=lee", api.GrantRequest{Project: "show-a", Tracker: "github"}, &out); code != http.StatusOK {
		t.Fatalf("grant: %d", code)
	}
	if code := call(t, s, http.MethodDelete, "/api/v1/grants/show-a/"+out.Grant.Name+"?as=lee", nil, &out); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}

	got := trackers(t, s, "artist")
	if len(got) != 3 {
		t.Fatalf("maya sees show-a's three trackers, not show-b's: %v", slices.Collect(maps.Keys(got)))
	}
	g := got["show-a/github"]
	if g.Project != "show-a" || g.Kind != "github" || g.Site != "github.com/petrelfinch/pipeline" ||
		!slices.Equal(g.Syncs, []string{"pull requests", "issues labeled render", "milestones, as due dates"}) {
		t.Fatalf("github: %+v", g)
	}
	if g.Ready || !strings.Contains(g.Why, "401 Bad credentials") || g.Granted != 0 || g.Mine != nil {
		t.Fatalf("github is not connected, and lee's revoked grant does not count: %+v", g)
	}
	f := got["show-a/saltmarsh-flow"]
	if f.Site != "Flow Production Tracking, Saltmarsh" ||
		!slices.Equal(f.Syncs, []string{"tasks assigned to a person, where step.Step.short_name is LGT"}) {
		t.Fatalf("flow, in words: %+v", f)
	}
	if !f.Ready || f.Why != "" || f.Granted != 2 || f.Mine == nil || f.Mine.Person != "maya" || !f.Mine.Active {
		t.Fatalf("flow is connected, with two grants, one maya's: %+v", f)
	}
	l := got["show-a/linear"]
	if l.Site != "Linear, Pipeline" || l.Ready || !strings.Contains(l.Why, "not synced since") {
		t.Fatalf("a sync two hours behind a 15-minute resync is not connected: %+v", l)
	}
	if lee := trackers(t, s, "lee")["show-a/github"]; lee.Mine == nil || lee.Mine.Active || lee.Mine.Revoked == nil {
		t.Fatalf("lee's own grant shows, revoked: %+v", lee.Mine)
	}
	if all := trackers(t, s, "wrangler"); len(all) != 4 || all["show-b/github"].Why != "it has not connected yet" {
		t.Fatalf("a farm-wide viewer sees show-b's too: %+v", all)
	}
}

func TestTrackerWords(t *testing.T) {
	for in, want := range map[string]string{"PullRequest": "pull requests", "Issue": "issues", "Task": "tasks",
		"Run": "runs", "Story": "stories", "Status": "statuses", "": "items"} {
		if got := entityPlural(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		spec c4v1.TrackerSpec
		want string
	}{
		{c4v1.TrackerSpec{Kind: "github", Endpoint: "https://ghe.studio.example/api/v3", Scope: map[string]string{"repo": "fx/pipe"}}, "ghe.studio.example/fx/pipe"},
		{c4v1.TrackerSpec{Kind: "jira", Endpoint: "https://saltmarsh.atlassian.net", Scope: map[string]string{"project": "SALT"}}, "Jira, saltmarsh.atlassian.net, SALT"},
		{c4v1.TrackerSpec{Kind: "mlflow", Endpoint: "http://mlflow.lab:5000", Scope: map[string]string{"experiment": "7"}}, "MLflow, mlflow.lab"},
	} {
		if got := trackerSite(&c.spec); got != c.want {
			t.Errorf("%s: %q, want %q", c.spec.Kind, got, c.want)
		}
	}
	syncs := trackerSyncs(&c4v1.TrackerSpec{Intake: []c4v1.TrackerIntake{
		{Match: c4v1.TrackerMatch{Type: "Issue", State: "all"}, For: &c4v1.TrackerFor{Person: "maya"}}}})
	if !slices.Equal(syncs, []string{"open and closed issues for maya"}) {
		t.Errorf("syncs: %q", syncs)
	}
}
