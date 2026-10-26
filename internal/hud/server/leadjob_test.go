package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// post sends body as viewer as with headers, and returns the status and
// the error the HUD answered, if any.
func post(t *testing.T, s *Server, path, as string, body any, headers map[string]string) (int, string, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path+"?as="+as, bytes.NewReader(raw))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var e struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, e.Error, w.Body.Bytes()
}

// leadHeader is the header of the lead's own MCP client, for session.
func leadHeader(session string) map[string]string {
	return map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": session}
}

// A lead's job is made from the lead's own session: the agent header
// names the session the request does. Only such a job says its lead
// answers it, in the farm's words; a job with no lead is refused that.
func TestSubmitLeadJob(t *testing.T) {
	s := submitFarm(t)
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{Units: 1, Feasible: true}, nil
	}
	graph := func(answers *c4v1.WorkAnswers, class string) json.RawMessage {
		b, _ := json.Marshal(c4v1.WorkJobSpec{Class: class, Answers: answers,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-agent"}}}}})
		return b
	}
	byLead := &c4v1.WorkAnswers{By: c4v1.AnswersByLead}
	for _, tc := range []struct {
		name    string
		req     api.SubmitRequest
		headers map[string]string
		code    int
		want    string
	}{
		{"no lead", api.SubmitRequest{Project: "show-a", Name: "noor-graph", Spec: graph(byLead, "graph")}, leadHeader("session-1"),
			http.StatusBadRequest, c4v1.NoLeadMessage},
		{"no header", api.SubmitRequest{Project: "show-a", Name: "session-1", Spec: graph(byLead, "graph"), LeadSession: "session-1"}, nil,
			http.StatusForbidden, "leadSession session-1: the request names no session in its agent header (X-C4-Agent), so it is not that lead's; make the job from the lead's own session"},
		{"another session", api.SubmitRequest{Project: "show-a", Name: "session-1", Spec: graph(byLead, "graph"), LeadSession: "session-1"}, leadHeader("session-2"),
			http.StatusForbidden, "leadSession session-1: the agent header (X-C4-Agent) names session session-2; a lead's job is made from that lead's own session"},
		{"not a graph", api.SubmitRequest{Project: "show-a", Name: "session-1", Spec: graph(nil, "render"), LeadSession: "session-1"}, leadHeader("session-1"),
			http.StatusBadRequest, "a lead's job is a graph job, which takes its lead's workers as they come: spec.class graph, not render"},
	} {
		if code, msg, _ := post(t, s, "/api/v1/jobs", "artist", tc.req, tc.headers); code != tc.code || msg != tc.want {
			t.Errorf("%s: %d %q", tc.name, code, msg)
		}
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a refused request made a job: %v %d", err, len(list.Items))
	}

	req := api.SubmitRequest{Project: "show-a", Name: "session-1", Spec: graph(byLead, "graph"), LeadSession: "session-1"}
	if code, msg, _ := post(t, s, "/api/v1/jobs", "artist", req, leadHeader("session-1")); code != http.StatusCreated {
		t.Fatalf("a lead's job: %d %s", code, msg)
	}
	var j c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "session-1"}, &j); err != nil {
		t.Fatal(err)
	}
	if j.Annotations[c4v1.AnnotationLeadSession] != "session-1" || j.Annotations[c4v1.AnnotationOwner] != "maya" ||
		j.Spec.Answers == nil || j.Spec.Answers.By != c4v1.AnswersByLead {
		t.Fatalf("job %v %+v", j.Annotations, j.Spec.Answers)
	}
}

// leadJobFarm is appendFarm's job made a lead's, for session-1 of dana's,
// answered by its lead.
func leadJobFarm(t *testing.T, edit func(j *c4v1.WorkJob)) *Server {
	t.Helper()
	s, _, _ := appendFarm(t, func(j *c4v1.WorkJob) {
		j.Annotations[c4v1.AnnotationLeadSession] = "session-1"
		j.Spec.Answers = &c4v1.WorkAnswers{By: c4v1.AnswersByLead}
		if edit != nil {
			edit(j)
		}
	})
	return s
}

func liveJob(t *testing.T, s *Server, name string) *c4v1.WorkJob {
	t.Helper()
	var j c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: name}, &j); err != nil {
		t.Fatal(err)
	}
	return &j
}

// A lead's job is closed by its person or by that lead, signed and
// listed for the queue; another person, another session, or a job with
// no lead is refused. Once closing it takes no more workers, and closing
// it again answers as it stands.
func TestCloseLeadJob(t *testing.T) {
	s := leadJobFarm(t, nil)
	for _, tc := range []struct {
		name, job, as string
		headers       map[string]string
		code          int
		want          string
	}{
		{"another person", "noor-lead", "artist", nil, http.StatusForbidden,
			"noor-lead is dana's job, made for their lead (session session-1): only dana, or that lead, closes it"},
		{"another session", "noor-lead", "wrangler", leadHeader("session-2"), http.StatusForbidden,
			"noor-lead is the job of lead session session-1, and the agent header (X-C4-Agent) names session session-2: only that lead, or dana with no agent header, closes it"},
		{"no lead", "sh040-light-v007", "wrangler", nil, http.StatusUnprocessableEntity,
			"sh040-light-v007 is not a lead's job: it finishes once its units have, and is never closed"},
	} {
		if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/"+tc.job+"/close", tc.as, api.CloseRequest{}, tc.headers); code != tc.code || msg != tc.want {
			t.Errorf("%s: %d %q", tc.name, code, msg)
		}
	}
	if j := liveJob(t, s, "noor-lead"); j.Annotations[c4v1.AnnotationClose] != "" {
		t.Fatalf("a refused close was listed: %s", j.Annotations[c4v1.AnnotationClose])
	}

	code, msg, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/close", "wrangler", api.CloseRequest{Why: "the feature is done"}, leadHeader("session-1"))
	var out api.CloseResponse
	if err := json.Unmarshal(raw, &out); err != nil || code != http.StatusOK || out.State != "closing" || out.Record == "" {
		t.Fatalf("close: %d %s %+v", code, msg, out)
	}
	var reqs []c4v1.WorkCloseRequest
	j := liveJob(t, s, "noor-lead")
	if err := json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationClose]), &reqs); err != nil || len(reqs) != 1 ||
		reqs[0].Why != "the feature is done" || reqs[0].Actor != "agent/session-1 for dana" || reqs[0].Record != out.Record {
		t.Fatalf("request %v %+v", err, reqs)
	}
	if !strings.Contains(j.Annotations[c4v1.AnnotationControlRecords], out.Record) {
		t.Fatalf("control records %q", j.Annotations[c4v1.AnnotationControlRecords])
	}
	// Again, by the person directly: as it stands, nothing listed twice.
	if code, _, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/close", "wrangler", api.CloseRequest{}, nil); code != http.StatusOK || !strings.Contains(string(raw), `"state":"closing"`) {
		t.Fatalf("closing again: %d %s", code, raw)
	}
	if j := liveJob(t, s, "noor-lead"); j.Annotations[c4v1.AnnotationClose] == "" || strings.Count(j.Annotations[c4v1.AnnotationClose], "token") != 1 {
		t.Fatalf("closing again listed another: %s", j.Annotations[c4v1.AnnotationClose])
	}
	// A closing job takes no more workers.
	if code, body := appendPost(s, "noor-lead", "wrangler", worker1, leadHeader("session-1")); code != http.StatusConflict ||
		!strings.Contains(body, "noor-lead is closing (agent/session-1 for dana asked): a closed lead's job takes no more workers; make a new job for them") {
		t.Fatalf("append while closing: %d %s", code, body)
	}

	// Once the queue has closed it, its closing is the answer.
	s = leadJobFarm(t, func(j *c4v1.WorkJob) {
		j.Status.Lead = &c4v1.WorkLead{Session: "session-1", State: c4v1.LeadClosed,
			Closed: &c4v1.WorkClose{At: metav1.Now(), By: "c4", Why: "idle 24h, no worker since 2026-10-25T09:00:00Z"}}
	})
	if code, body := appendPost(s, "noor-lead", "wrangler", worker1, leadHeader("session-1")); code != http.StatusConflict ||
		!strings.Contains(body, "noor-lead is closed (idle 24h, no worker since 2026-10-25T09:00:00Z): a closed lead's job takes no more workers; make a new job for them") {
		t.Fatalf("append after close: %d %s", code, body)
	}
	if code, _, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/close", "wrangler", api.CloseRequest{}, nil); code != http.StatusOK || !strings.Contains(string(raw), `"state":"closed"`) {
		t.Fatalf("closing a closed job: %d %s", code, raw)
	}
	var detail api.Job
	if code := get(t, s, "/api/v1/jobs/show-a/noor-lead?as=wrangler", &detail); code != http.StatusOK || detail.Lead == nil ||
		detail.Lead.State != c4v1.LeadClosed || detail.Lead.Closed == nil || detail.Lead.Closed.By != "c4" {
		t.Fatalf("detail: %d %+v", code, detail.Lead)
	}
}

// A question for a lead (for: lead:<session>) is answered by that lead,
// through its person's client with the agent header naming its session,
// or by the person directly; a header naming another session is refused,
// saying whose question it is.
func TestSupplyLeadQuestion(t *testing.T) {
	asking := func(j *c4v1.WorkJob) {
		j.Status.UnitStates = map[string]string{"needs": "0"}
		j.Status.Needs = map[string]c4v1.WorkNeeds{"0": {Text: "which branch to start from", At: metav1.Now(), State: "raised", For: "lead:session-1"}}
	}
	supply := map[string]any{"unit": 0, "text": "main"}
	s := leadJobFarm(t, asking)
	if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/noor-lead/supply", "wrangler", supply, leadHeader("session-2")); code != http.StatusForbidden ||
		msg != `unit "0"'s question is for the lead of session session-1, and the agent header (X-C4-Agent) names session session-2: only that lead, or dana with no agent header, answers it` {
		t.Fatalf("another session: %d %q", code, msg)
	}
	if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/noor-lead/needs/0/withdraw", "wrangler", map[string]string{"why": "moot"}, leadHeader("session-2")); code != http.StatusForbidden ||
		!strings.Contains(msg, "the agent header (X-C4-Agent) names session session-2") {
		t.Fatalf("another session withdrawing: %d %q", code, msg)
	}
	var detail api.Job
	if code := get(t, s, "/api/v1/jobs/show-a/noor-lead?as=wrangler", &detail); code != http.StatusOK || len(detail.Needs) != 1 || detail.Needs[0].For != "lead:session-1" {
		t.Fatalf("detail: %d %+v", code, detail.Needs)
	}
	for name, headers := range map[string]map[string]string{"the lead": leadHeader("session-1"), "the person": nil} {
		s := leadJobFarm(t, asking)
		if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/noor-lead/supply", "wrangler", supply, headers); code != http.StatusOK {
			t.Errorf("%s: %d %q", name, code, msg)
		}
	}
}

// A lead's job grows by its own lead, or its person directly: a header
// naming another session adds nothing, and says whose job it is.
func TestAppendToLeadJob(t *testing.T) {
	s := leadJobFarm(t, nil)
	code, body := appendPost(s, "noor-lead", "wrangler", worker1, leadHeader("session-2"))
	if code != http.StatusForbidden || !strings.Contains(body,
		"noor-lead is the job of lead session session-1, and the agent header (X-C4-Agent) names session session-2: only that lead, or dana with no agent header, adds workers to it") {
		t.Fatalf("another session: %d %s", code, body)
	}
	if _, reqs := liveAppends(t, s); len(reqs) != 0 {
		t.Fatalf("a refused append was listed: %+v", reqs)
	}
	if code, body := appendPost(s, "noor-lead", "wrangler", worker1, leadHeader("session-1")); code != http.StatusCreated {
		t.Fatalf("its lead: %d %s", code, body)
	}
	if code, body := appendPost(s, "noor-lead", "wrangler", strings.NewReplacer(`"k1"`, `"k2"`, "worker-1", "worker-2").Replace(worker1), nil); code != http.StatusCreated {
		t.Fatalf("its person: %d %s", code, body)
	}
}

// On a one-person farm a role's sign-in is that person acting in the
// role (ownerFor): it adds workers to, and closes, the person's lead's
// job, as the person would.
func TestARoleActsForItsPersonsLead(t *testing.T) {
	s := leadJobFarm(t, nil)
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "dana", To: "laptop-1/*", OnePerson: true, For: []string{"task/v1"}, Start: metav1.Now(), By: "dana"}}
	if err := s.Client.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	s.DevViewersOpen, s.InstallerToken = false, []byte("test-token")
	s.DevViewers["artist"] = &api.Viewer{User: "artist", Projects: []string{"show-a"}}
	h := leadHeader("session-1")
	h[InstallerHeader] = "test-token"
	if code, msg, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/close", "artist", api.CloseRequest{Why: "done"}, h); code != http.StatusOK || !strings.Contains(string(raw), `"state":"closing"`) {
		t.Fatalf("the role's close: %d %q %s", code, msg, raw)
	}
}
