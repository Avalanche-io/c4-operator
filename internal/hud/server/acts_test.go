package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// A request may carry its act, signed by the person's own device key. The
// control record cites it, on the job by UID and its spec before the
// change, with the act's params and grade. An act that was taken already,
// or is for another action, other params, another person or a spec that
// has changed, is refused; a request with no act is taken as before.
func TestControlRecordCitesTheSignedAct(t *testing.T) {
	s, decide := decideFarm(t)
	now := time.Now()
	f := provtest.NewFarm(t, now)
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}}, now.Add(time.Hour)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	spec := queue.SpecDigest(&rec.job)
	dana, noor := f.Person(t, "dana", false), f.Person(t, "noor", false)
	on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, spec)
	params := map[string]any{"do": "approve", "unit": "ruling"}
	act := func(k *provtest.Key, a provtest.Act) string {
		if a.Subject.Name == "" {
			a.Subject = on
		}
		if a.Act == "" {
			a.Act = "decide"
		}
		if a.Params == nil {
			a.Params = params
		}
		a.SignedAt = time.Now()
		return base64.StdEncoding.EncodeToString(f.SignAct(t, k, a))
	}
	signed := act(dana, provtest.Act{})
	code, p := decide(decideRequest("wrangler", map[string]string{ActHeader: signed}))
	if code != http.StatusOK {
		t.Fatalf("a signed act: %d", code)
	}
	b, _ := base64.StdEncoding.DecodeString(signed)
	var bundle provenance.Bundle
	json.Unmarshal(b, &bundle)
	payload, _ := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if p.Act == nil || p.Act.Payload != provenance.PayloadHash(payload) || !strings.HasPrefix(p.Act.Record, "c4") || p.Custody != CustodySigned {
		t.Fatalf("the control record cites %+v, custody %q", p.Act, p.Custody)
	}
	var got map[string]any
	if json.Unmarshal(p.Params, &got) != nil || got["do"] != "approve" || got["unit"] != "ruling" {
		t.Errorf("the control record's params: %s", p.Params)
	}
	stale := on
	stale.Digest = map[string]string{"sha256": strings.Repeat("0", 64)}
	// Until K5 and K6 check their callers, only a person's own act is
	// taken: a relay's key would otherwise grade the record relayed
	// around relayed()'s checks, whoever sent it.
	relay := f.Relay(t, "c4-system", "importer", "flow", "dana")
	agent := f.Agent(t, "assistant", "")
	unit := f.Unit(t, "show-a", "sh040-light-v007", "8", provtest.JobUID, 1)
	for name, h := range map[string]string{
		"a relay's act":            act(relay, provtest.Act{}),
		"an agent's act":           act(agent, provtest.Act{}),
		"a unit's act":             act(unit, provtest.Act{}),
		"the same act again":       signed,
		"an act for another verb":  act(dana, provtest.Act{Act: "steer"}),
		"an act with other params": act(dana, provtest.Act{Params: map[string]any{"do": "reject", "unit": "ruling"}}),
		"noor's act, dana sends":   act(noor, provtest.Act{}),
		"an act on an older spec":  act(dana, provtest.Act{Subject: stale}),
		"not base64":               "not an act",
	} {
		if code, _ := decide(decideRequest("wrangler", map[string]string{ActHeader: h})); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	if code, p := decide(decideRequest("wrangler", nil)); code != http.StatusOK || p.Act != nil {
		t.Errorf("a request with no act: %d, act %+v", code, p.Act)
	}
}

// Each job's summary names its UID and its spec's digest, the subject a
// client signs an act on.
func TestJobSummaryNamesWhatAnActSigns(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	sum := s.jobSummary(s.m, rec)
	if sum.UID != provtest.JobUID || sum.SpecDigest != queue.SpecDigest(&rec.job) || len(sum.SpecDigest) != 64 {
		t.Errorf("summary uid %q, specDigest %q", sum.UID, sum.SpecDigest)
	}
}

// A unit's act is taken only from that unit (PEOPLE-KEYS 5.1, 10.4 item
// 18): sent with its own pod's token, signed by exactly its attempt's key,
// for its job's owner. The same unit's earlier attempt, another unit, a
// person sending a unit's act, and an act for someone else are refused;
// a unit's request with no act is taken as before.
func TestUnitActFromItsOwnToken(t *testing.T) {
	s, call := decideFarm(t)
	now := time.Now()
	f := provtest.NewFarm(t, now)
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}}, now.Add(time.Hour)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	if rec.job.Annotations == nil {
		rec.job.Annotations = map[string]string{}
	}
	rec.job.Annotations[c4v1.AnnotationOwner] = "dana"
	spec := queue.SpecDigest(&rec.job)
	on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, spec)
	unitViewer := func(unit string) *api.Viewer {
		return &api.Viewer{User: "unit:show-a/sh040-light-v007/" + unit, Projects: []string{"show-a"},
			Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", JobUID: provtest.JobUID, Unit: unit, Grant: "job", Attempt: 2}}
	}
	s.DevViewers["unit3"], s.DevViewers["unit4"] = unitViewer("3"), unitViewer("4")
	body := `{"unit": "8", "text": "use the v3 plates"}`
	params := map[string]any{"unit": "8", "text": "use the v3 plates"}
	act := func(k *provtest.Key, a provtest.Act) string {
		a.Subject, a.Act, a.Params, a.SignedAt = on, "steer", params, time.Now()
		return base64.StdEncoding.EncodeToString(f.SignAct(t, k, a))
	}
	steer := func(as, h string) (int, controlPredicate) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/steer?as="+as, strings.NewReader(body))
		if h != "" {
			r.Header.Set(ActHeader, h)
		}
		return call(r)
	}
	three := f.Unit(t, "show-a", "sh040-light-v007", "3", provtest.JobUID, 2)
	if code, p := steer("unit3", act(three, provtest.Act{})); code != http.StatusOK || p.Act == nil || p.Custody != CustodySigned {
		t.Fatalf("a unit's own act: %d, act %+v, custody %q", code, p.Act, p.Custody)
	}
	earlier := f.Unit(t, "show-a", "sh040-light-v007", "3", provtest.JobUID, 1)
	four := f.Unit(t, "show-a", "sh040-light-v007", "4", provtest.JobUID, 2)
	for name, c := range map[string]struct{ as, h, why string }{
		"its earlier attempt's key":        {"unit3", act(earlier, provtest.Act{}), "/1, and this request comes from"},
		"another unit's act":               {"unit3", act(four, provtest.Act{}), "sh040-light-v007/4@"},
		"a person sends a unit's act":      {"wrangler", act(three, provtest.Act{}), "this request is no unit's"},
		"an act for someone but the owner": {"unit3", act(three, provtest.Act{For: "noor"}), "acts for its job's owner, dana"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/steer?as="+c.as, strings.NewReader(body))
		r.Header.Set(ActHeader, c.h)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), c.why) {
			t.Errorf("%s: %d %s, want 403 with %q", name, w.Code, w.Body.String(), c.why)
		}
	}
	if code, p := steer("unit3", ""); code != http.StatusOK || p.Act != nil {
		t.Errorf("a unit's request with no act: %d, act %+v", code, p.Act)
	}

	// another job of the same owner: the unit's act names job B, sent from
	// job A's unit, to A's route and to B's; neither is taken
	b := s.m.byKey["show-a/sh050-comp-v001"]
	b.job.UID = "9c1d2e7f-0000-4000-8000-00000000000b"
	if b.job.Annotations == nil {
		b.job.Annotations = map[string]string{}
	}
	b.job.Annotations[c4v1.AnnotationOwner] = "dana"
	onB := provenance.ActSubject("workjob", "show-a", "sh050-comp-v001", string(b.job.UID), queue.SpecDigest(&b.job))
	actOnB := func() string {
		return base64.StdEncoding.EncodeToString(f.SignAct(t, three, provtest.Act{Subject: onB, Act: "steer", Params: params, SignedAt: time.Now()}))
	}
	for job, why := range map[string]string{
		"sh040-light-v007": "or it is another job",                                 // A's route: the act is on another job
		"sh050-comp-v001":  "may act only on its own job, show-a/sh040-light-v007", // B's route: a unit controls only its own job
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/"+job+"/steer?as=unit3", strings.NewReader(body))
		r.Header.Set(ActHeader, actOnB())
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), why) {
			t.Errorf("job A's unit acting on job B through %s's route: %d %s, want 403 with %q", job, w.Code, w.Body.String(), why)
		}
	}
}
