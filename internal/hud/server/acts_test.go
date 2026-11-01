package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

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
	for name, h := range map[string]string{
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
