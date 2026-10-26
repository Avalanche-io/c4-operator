package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// appendFarm is a farm whose HUD signs into a store, with dana's running
// graph job noor-lead in show-a, and a unit of it as a viewer.
func appendFarm(t *testing.T, edit func(j *c4v1.WorkJob)) (*Server, *storeurl.Sharded, *x509.CertPool) {
	t.Helper()
	s := farm(t)
	ctx := context.Background()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	s.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	j := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "noor-lead", ResourceVersion: "1",
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana"}},
		Spec:   c4v1.WorkJobSpec{Class: "graph", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "u", Image: "lead"}}}}},
		Status: c4v1.WorkJobStatus{Phase: "Running", UnitStates: map[string]string{"running": "0"}}}
	if edit != nil {
		edit(&j)
	}
	d, err := class.Lookup("graph")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(&j)
	if err != nil {
		t.Fatal(err)
	}
	rec := &jobRec{job: j, key: "show-a/noor-lead", project: "show-a", plan: plan, driver: d}
	s.m.jobs = append(s.m.jobs, rec)
	s.m.byKey[rec.key] = rec
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	live := j.DeepCopy()
	live.ResourceVersion = ""
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, s.m.byKey["show-a/sh040-light-v007"].job.DeepCopy()).Build()
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/noor-lead/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "noor-lead", Unit: "0", Grant: "job"}}
	return s, st, roots
}

func appendPost(s *Server, job, as, body string, headers map[string]string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/"+job+"/units?as="+as, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func liveAppends(t *testing.T, s *Server) (*c4v1.WorkJob, []c4v1.WorkAppendRequest) {
	t.Helper()
	var j c4v1.WorkJob
	if err := s.Client.Get(context.Background(), client.ObjectKey{Namespace: "show-a", Name: "noor-lead"}, &j); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkAppendRequest
	if raw := j.Annotations[c4v1.AnnotationAppend]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
			t.Fatal(err)
		}
	}
	return &j, reqs
}

func readStored(t *testing.T, st *storeurl.Sharded, id string) []byte {
	t.Helper()
	cid, err := c4.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.Open(cid)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const worker1 = `{"key": "k1", "turn": 3, "units": [{"name": "worker-1", "env": {"C4_TASK": "fix the parser"}, "work": 600}]}`

// The job's owner, through their MCP client, adds a unit: the document is
// stored as a generator's would be, the request is signed (action append,
// the agent graded claimed) and listed for the queue, and the same key
// again answers the same without listing it twice; once the queue has
// added the units the answer carries their indexes, and a new key lists
// a new request.
func TestAppendUnits(t *testing.T) {
	s, st, roots := appendFarm(t, nil)
	mcp := map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": "claude"}
	code, body := appendPost(s, "noor-lead", "wrangler", worker1, mcp)
	if code != http.StatusCreated {
		t.Fatalf("append: %d %s", code, body)
	}
	var out api.AppendUnitsResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "pending" || len(out.Units) != 1 || out.Units[0].Name != "worker-1" || out.Units[0].Index != nil || out.Record == "" || out.Document == "" {
		t.Fatalf("answer %+v", out)
	}
	j, reqs := liveAppends(t, s)
	if len(reqs) != 1 || reqs[0].Key != "k1" || reqs[0].Document != out.Document || reqs[0].Record != out.Record ||
		reqs[0].Actor != "agent/claude for dana" || reqs[0].Turn == nil || *reqs[0].Turn != 3 || reqs[0].Token == "" {
		t.Fatalf("requests %+v", reqs)
	}
	if !strings.Contains(j.Annotations[c4v1.AnnotationControlRecords], out.Record) {
		t.Fatalf("control records %q lack %s", j.Annotations[c4v1.AnnotationControlRecords], out.Record)
	}
	// The stored document is an expansion document the queue reads, the
	// turn carried onto its unit.
	var doc class.Doc
	if err := class.Strict(readStored(t, st, out.Document), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.Units) != 1 || doc.Units[0].Group != "turn 3" || doc.Units[0].Params["turn"] != "3" ||
		doc.Units[0].Env["C4_TASK"] != "fix the parser" || string(doc.Dependents) != "[]" {
		t.Fatalf("document %+v", doc)
	}
	// The signed record.
	cid, _ := c4.Parse(out.Record)
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := provenance.VerifyBundle(data, roots)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		controlPredicate
		Change struct {
			Request map[string]any `json:"request"`
		} `json:"change"`
	}
	if err := json.Unmarshal(rv.Predicate, &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != "append" || p.Target != "workjob:show-a/noor-lead" || p.Actor != "dana" || p.Via != "mcp" ||
		p.Agent != "claude" || p.AgentGrade != CustodyClaimed {
		t.Fatalf("record %+v", p.controlPredicate)
	}
	rq := p.Change.Request
	if rq["key"] != "k1" || rq["document"] != out.Document || rq["turn"] != float64(3) || rq["token"] != reqs[0].Token {
		t.Fatalf("record's request %v", rq)
	}
	if units, _ := rq["units"].([]any); len(units) != 1 || units[0] != "worker-1" {
		t.Fatalf("record's units %v", rq["units"])
	}

	// The same key again: the same answer, nothing more listed.
	code, body = appendPost(s, "noor-lead", "wrangler", worker1, mcp)
	var again api.AppendUnitsResponse
	json.Unmarshal([]byte(body), &again)
	if code != http.StatusOK || again.Record != out.Record || again.Document != out.Document || again.State != "pending" || len(again.Units) != 1 {
		t.Fatalf("again: %d %s", code, body)
	}
	if _, reqs := liveAppends(t, s); len(reqs) != 1 {
		t.Fatalf("again listed %d requests", len(reqs))
	}

	// The queue adds it as unit 1; the same key reads the index, and a new
	// key lists a new request while the applied one leaves the list.
	j.Status.Appends = []c4v1.WorkAppend{{Key: "k1", Document: out.Document, First: 1, Units: 1, Control: out.Record, At: metav1.Now()}}
	if err := s.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	code, body = appendPost(s, "noor-lead", "wrangler", worker1, mcp)
	json.Unmarshal([]byte(body), &again)
	if code != http.StatusOK || again.State != "added" || len(again.Units) != 1 || again.Units[0].Index == nil || *again.Units[0].Index != 1 || again.Record != out.Record {
		t.Fatalf("after the queue: %d %s", code, body)
	}
	code, body = appendPost(s, "noor-lead", "wrangler", `{"key": "k2", "units": [{"name": "worker-2", "after": ["worker-1"]}]}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("a second key: %d %s", code, body)
	}
	if _, reqs := liveAppends(t, s); len(reqs) != 1 || reqs[0].Key != "k2" || reqs[0].Actor != "dana" || reqs[0].Turn != nil {
		t.Fatalf("after a second key: %+v", reqs)
	}
	// A refusal the queue recorded reads as one.
	j, _ = liveAppends(t, s)
	j.Status.Appends = append(j.Status.Appends, c4v1.WorkAppend{Key: "k2", Error: "unit worker-2 runs after worker-1, which is not in the job"})
	if err := s.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	code, body = appendPost(s, "noor-lead", "wrangler", `{"key": "k2", "units": [{"name": "worker-2"}]}`, nil)
	json.Unmarshal([]byte(body), &again)
	if code != http.StatusOK || again.State != "refused" || !strings.Contains(again.Error, "not in the job") {
		t.Fatalf("refused: %d %s", code, body)
	}
}

// Who may and what: a unit never adds units (agent units in the lead's
// words), nor anyone but the job's owner; a finished job takes none, a
// job that is not a graph takes none, and units the queue would refuse
// are refused here.
func TestAppendUnitsRefused(t *testing.T) {
	s, _, _ := appendFarm(t, nil)
	block := `{"key": "b", "units": [{"name": "noor-block", "env": {"C4_AGENT_BLOCK": "1"}}]}`
	for _, c := range []struct {
		name, job, as, body string
		code                int
		says                string
	}{
		{"a unit's agent units", "noor-lead", "unit", block, http.StatusForbidden, c4v1.LeadOnlyMessage},
		{"a unit's units", "noor-lead", "unit", worker1, http.StatusForbidden, "a unit cannot add units to a job: its lead does"},
		{"not the owner", "noor-lead", "artist", worker1, http.StatusForbidden, "noor-lead is dana's job: only dana"},
		{"a render job", "sh040-light-v007", "wrangler", worker1, http.StatusUnprocessableEntity, "sh040-light-v007 is a render job, whose units its class makes: only a graph job takes units while it runs"},
		{"no key", "noor-lead", "wrangler", `{"units": [{"name": "w"}]}`, http.StatusBadRequest, "key: an idempotency key"},
		{"no units", "noor-lead", "wrangler", `{"key": "k", "units": []}`, http.StatusBadRequest, "at least one unit"},
		{"a field no unit has", "noor-lead", "wrangler", `{"key": "k", "units": [{"name": "w", "colour": "red"}]}`, http.StatusBadRequest, "colour"},
		{"a name the job has", "noor-lead", "wrangler", `{"key": "k", "units": [{"name": "plan"}]}`, http.StatusUnprocessableEntity, "unit plan is already in the job"},
		{"after a unit it lacks", "noor-lead", "wrangler", `{"key": "k", "units": [{"name": "w", "after": ["nothing"]}]}`, http.StatusUnprocessableEntity, "not in the job"},
	} {
		code, body := appendPost(s, c.job, c.as, c.body, nil)
		if code != c.code || !strings.Contains(body, c.says) {
			t.Errorf("%s: %d %s, want %d saying %q", c.name, code, body, c.code, c.says)
		}
		if c.code == http.StatusForbidden && c.says == c4v1.LeadOnlyMessage && !strings.Contains(body, c4v1.LeadOnlyFix) {
			t.Errorf("%s: no fix: %s", c.name, body)
		}
	}
	if _, reqs := liveAppends(t, s); len(reqs) != 0 {
		t.Fatalf("a refusal listed %+v", reqs)
	}

	// A finished job.
	s, _, _ = appendFarm(t, func(j *c4v1.WorkJob) { j.Status.Phase = "Succeeded" })
	if code, body := appendPost(s, "noor-lead", "wrangler", worker1, nil); code != http.StatusConflict || !strings.Contains(body, "noor-lead has finished (succeeded): a finished job takes no more units") {
		t.Fatalf("a finished job: %d %s", code, body)
	}
	// A unit's job grows no agent units, even at its owner's word.
	s, _, _ = appendFarm(t, func(j *c4v1.WorkJob) { j.Annotations[c4v1.AnnotationSubmittedByUnit] = "show-a/plan/0" })
	if code, body := appendPost(s, "noor-lead", "wrangler", block, nil); code != http.StatusForbidden || !strings.Contains(body, c4v1.LeadOnlyFix) {
		t.Fatalf("agent units on a unit's job: %d %s", code, body)
	}
	if code, body := appendPost(s, "noor-lead", "wrangler", worker1, nil); code != http.StatusCreated {
		t.Fatalf("plain units on a unit's job: %d %s", code, body)
	}
}
