package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// A lead puts workers on the farm with one call. The first makes the
// lead's job through the submission's own path: a graph job named for the
// person and the session, its lead's session on it, answered by its lead,
// its workers its table, each with its brief and the commit it starts
// from. The same call again adds nothing; a later call adds its workers to
// the same job (here by the path a call takes when the job was made by
// one the HUD has not seen yet), signed once. A call with no session, or
// a base that is not a commit, is refused, and makes nothing.
func TestLeadWorkers(t *testing.T) {
	s := submissionFarm(t)
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	agentToolIn(t, s)
	base := strings.Repeat("0a1b", 10)
	turn := 7
	first := api.LeadWorkersRequest{Project: "show-a", Root: "/Users/dev/textkit", Key: "call-1", Turn: &turn, Workers: []api.LeadWorker{
		{Name: "review-design", Description: "Design critic", Prompt: "Review docs/designs/parser.md as a design critic.", Base: base},
		{Description: "Naive reader", Prompt: "Read docs/designs/parser.md as someone new to it.", Base: base, Model: "claude-haiku-5-5"}}}

	code, msg, body := post(t, s, "/api/v1/lead/workers", "artist", first, leadHeader("session-1"))
	var out api.LeadWorkersResponse
	_ = json.Unmarshal(body, &out)
	name := leadJobName("maya", "session-1", 1)
	if code != http.StatusCreated || !out.Created || out.Job != "show-a/"+name || len(out.Units) != 2 || out.Units[0].Name != "review-design" ||
		out.Units[1].Index == nil || *out.Units[1].Index != 1 || !strings.HasPrefix(out.Units[1].Name, "naive-reader-") {
		t.Fatalf("the first call: %d %s %+v", code, msg, out)
	}
	j := liveJob(t, s, name)
	if j.Annotations[c4v1.AnnotationLeadSession] != "session-1" || j.Annotations[c4v1.AnnotationOwner] != "maya" ||
		j.Spec.Answers == nil || j.Spec.Answers.By != c4v1.AnswersByLead || j.Spec.Class != "graph" {
		t.Fatalf("the lead's job: %v %+v", j.Annotations, j.Spec.Answers)
	}
	st, _ := s.Stores(ctx, "show-a")
	id, _ := c4.Parse(j.Spec.Params[class.TableParam])
	r, err := st.Open(id)
	if err != nil {
		t.Fatalf("the table: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	r.Close()
	var doc class.Doc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil || len(doc.Units) != 2 {
		t.Fatalf("the table: %v %s", err, buf.Bytes())
	}
	u := doc.Units[0]
	if u.Env["C4_BLOCK_BASE"] != base || u.Env["C4_BLOCK_PROMPT"] != first.Workers[0].Prompt || u.Label != "Design critic" ||
		u.Group != "turn 7" || u.Planned || doc.Units[1].Env["C4_BLOCK_MODEL"] != "claude-haiku-5-5" {
		t.Fatalf("a worker: %+v", u)
	}

	// the same call again: nothing more
	code, msg, body = post(t, s, "/api/v1/lead/workers", "artist", first, leadHeader("session-1"))
	out = api.LeadWorkersResponse{}
	_ = json.Unmarshal(body, &out)
	if code != http.StatusOK || out.Job != "show-a/"+name || !strings.Contains(out.Message, "made already") {
		t.Fatalf("the call again: %d %s %+v", code, msg, out)
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("jobs: %v %d", err, len(list.Items))
	}

	// a later call: its workers are added to the same job
	next := api.LeadWorkersRequest{Project: "show-a", Root: "/Users/dev/textkit", Key: "call-2", Workers: []api.LeadWorker{
		{Name: "implement", Description: "Implement the parser", Prompt: "Implement docs/designs/parser.md.", Base: base}}}
	code, msg, body = post(t, s, "/api/v1/lead/workers", "artist", next, leadHeader("session-1"))
	out = api.LeadWorkersResponse{}
	_ = json.Unmarshal(body, &out)
	if code >= 300 || out.Created || out.Job != "show-a/"+name || len(out.Units) != 1 || out.Units[0].Name != "implement" {
		t.Fatalf("a later call: %d %s %+v", code, msg, out)
	}
	j = liveJob(t, s, name)
	if !strings.Contains(j.Annotations[c4v1.AnnotationAppend], `"implement"`) || !strings.Contains(j.Annotations[c4v1.AnnotationAppend], `"call-2"`) {
		t.Fatalf("the append: %s", j.Annotations[c4v1.AnnotationAppend])
	}
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("a later call made a job: %v %d", err, len(list.Items))
	}

	// refused, making nothing
	for _, c := range []struct {
		name    string
		req     api.LeadWorkersRequest
		headers map[string]string
		code    int
		want    string
	}{
		{"no session", next, nil, http.StatusBadRequest, "names none in its agent header"},
		{"no key", api.LeadWorkersRequest{Project: "show-a", Workers: next.Workers}, leadHeader("session-1"), http.StatusBadRequest, "key:"},
		{"no workers", api.LeadWorkersRequest{Project: "show-a", Key: "k"}, leadHeader("session-1"), http.StatusBadRequest, "at least one worker"},
		{"a ref as base", api.LeadWorkersRequest{Project: "show-a", Key: "k", Workers: []api.LeadWorker{{Description: "x", Prompt: "y", Base: "main"}}},
			leadHeader("session-2"), http.StatusUnprocessableEntity, "is not a full commit ID"},
	} {
		if code, msg, _ := post(t, s, "/api/v1/lead/workers", "artist", c.req, c.headers); code != c.code || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %d %q", c.name, code, msg)
		}
	}
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("a refused call made a job: %v %d", err, len(list.Items))
	}
}
