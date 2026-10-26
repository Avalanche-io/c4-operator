package server

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// A lead's wait answers once the units it names settle: ended, with the
// result it acts on (its summary, the unit's own words, whole from the
// store, and its branch), or stopped on a question, with the question.
// With any, one settled unit ends it; without, all must. A window that
// passes first answers with the units as they stand; a unit the job lacks
// is refused.
func TestWaitForUnits(t *testing.T) {
	s := farm(t)
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	s.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	whole := "Relit the shot with the v3 plates.\nThe rim light is a stand-in."
	doc, err := st.PutBytes(context.Background(), []byte(whole))
	if err != nil {
		t.Fatal(err)
	}
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates["needs"] = "8"
	rec.job.Status.UnitStates["queued"] = "6-7"
	rec.job.Status.UnitSummaries = map[string]c4v1.WorkUnitSummary{"3": {Excerpt: "Relit the shot with the v3 plates.", Doc: doc.String(), Attempt: 1}}
	rec.job.Status.UnitBranches = map[string]c4v1.WorkUnitBranch{"3": {Repo: "/Users/sam/src/shots", Branch: "c4/sh040-light-v007/3", Base: "b0", Head: "h1", Commits: 2}}
	rec.job.Status.Needs = map[string]c4v1.WorkNeeds{"8": {Text: "which plate version is final", Why: "v2 and v3 both exist", At: metav1.Now()}}
	const job = "/api/v1/jobs/show-a/sh040-light-v007/wait?as=wrangler&within=0s"

	// one ended unit: its result
	var a api.WaitAnswer
	if code := get(t, s, job+"&unit=3", &a); code != 200 || !a.Done || len(a.Units) != 1 {
		t.Fatalf("an ended unit: %d %+v", code, a)
	}
	u := a.Units[0]
	if u.Settled != "ended" || u.State != "succeeded" || u.Summary == nil || u.Summary.By != "unit" || u.Summary.Text != whole ||
		u.Branch == nil || u.Branch.Branch != "c4/sh040-light-v007/3" || u.Branch.Commits != 2 || u.Job != "show-a/sh040-light-v007" {
		t.Fatalf("its result: %+v %+v %+v", u, u.Summary, u.Branch)
	}

	// a unit stopped on a question: the question
	a = api.WaitAnswer{}
	get(t, s, job+"&unit=8", &a)
	if !a.Done || a.Units[0].Settled != "asks" || a.Units[0].Question == nil || a.Units[0].Question.Text != "which plate version is final" {
		t.Fatalf("a question: %+v", a)
	}

	// any: one settled is enough; all: a running unit keeps it open
	a = api.WaitAnswer{}
	get(t, s, job+"&unit=4&unit=9&any=true", &a)
	if !a.Done || a.Units[0].Settled != "" || a.Units[1].Settled != "ended" || a.Units[1].State != "failed" {
		t.Fatalf("any: %+v", a)
	}
	a = api.WaitAnswer{}
	get(t, s, job+"&unit=4&unit=9", &a)
	if a.Done || len(a.Units) != 2 {
		t.Fatalf("all, one still running: %+v", a)
	}
	if a.Units[0].Summary != nil || a.Units[0].Ended != nil {
		t.Fatalf("a running unit has a result: %+v", a.Units[0])
	}

	// the window holds an unsettled wait, then answers as things stand
	start := time.Now()
	a = api.WaitAnswer{}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007/wait?as=wrangler&within=1500ms&unit=4", &a)
	if took := time.Since(start); a.Done || took < time.Second {
		t.Fatalf("held %s, done %v", took, a.Done)
	}

	// refused: no unit named, a unit the job lacks, a bad window
	if code := get(t, s, job, nil); code != 400 {
		t.Fatalf("no unit: %d", code)
	}
	if code := get(t, s, job+"&unit=nope", nil); code != 404 {
		t.Fatalf("no such unit: %d", code)
	}
	if code := get(t, s, "/api/v1/jobs/show-a/sh040-light-v007/wait?as=wrangler&within=soon&unit=3", nil); code != 400 {
		t.Fatalf("a bad window: %d", code)
	}
}
