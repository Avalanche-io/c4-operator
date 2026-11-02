package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/schema"
)

// fakeGrants stands in for the people's issuer: the grants in force, by
// ID, and the acts booked as their use.
type fakeGrants struct {
	inForce map[string]*people.AgentGrant
	used    int
}

func (f *fakeGrants) AgentGrantFor(_ context.Context, v *provenance.Verified) (*people.AgentGrant, error) {
	g := f.inForce[v.Grant]
	if g == nil || provenance.AgentIdentity(g.Agent) != v.Identity {
		return nil, people.ErrNoGrant
	}
	return g, nil
}

func (f *fakeGrants) UseGrant(context.Context, *provenance.Verified, []byte) error {
	f.used++
	return nil
}

// agentDecide is a farm whose job's unit "ruling" waits for whom the test
// says, the grants in force, and the agent keys the test makes; dana is
// the person the wrangler viewer is.
type agentDecide struct {
	t      *testing.T
	s      *Server
	f      *provtest.Farm
	grants *fakeGrants
	on     provenance.Subject
	decide func(*http.Request) (int, controlPredicate)
}

func newAgentDecide(t *testing.T, waitsFor string) *agentDecide {
	s, decide := decideFarm(t)
	now := time.Now()
	f := provtest.NewFarm(t, now)
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}}, now.Add(time.Hour)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	rec.job.Status.Waits[0].Server = waitsFor
	g := &fakeGrants{inForce: map[string]*people.AgentGrant{}}
	s.AgentGrants = g
	return &agentDecide{t: t, s: s, f: f, grants: g, decide: decide,
		on: provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, queue.SpecDigest(&rec.job))}
}

// agent is a key of agent name's under a grant for person, in force
// unless off.
func (a *agentDecide) agent(name, person string, off bool) *provtest.Key {
	grant := provtest.ID(person + "'s grant to " + name + " " + time.Now().String())
	k := a.f.Agent(a.t, name, grant)
	if !off {
		a.grants.inForce[grant] = &people.AgentGrant{Grant: grant, Agent: name, Person: person}
	}
	return k
}

// act signs a decision of body as k, for party.
func (a *agentDecide) act(k *provtest.Key, party, body string) string {
	var params map[string]any
	if err := json.Unmarshal([]byte(body), &params); err != nil {
		a.t.Fatal(err)
	}
	g := schema.GradeSigned
	agent := strings.TrimPrefix(k.Identity, provenance.IdentityBase+"agent/")
	return base64.StdEncoding.EncodeToString(a.f.SignAct(a.t, k, provtest.Act{Subject: a.on, Act: "decide", Params: params, SignedAt: time.Now(),
		Chain: &schema.ActChain{By: schema.Actor{Agent: agent, Grade: g}, For: schema.For{Party: party, Grade: g},
			Under: []schema.Delegation{{Grant: k.Grant, Kind: "sign", From: party, To: "agent " + agent}}}}))
}

func agentRequest(body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as=wrangler", strings.NewReader(body))
	r.Header.Set("X-C4-Client", "mcp")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

const ownWait = `{"unit": "ruling", "do": "approve", "waitsFor": "agent:assistant"}`

// The HUD takes an agent's own act under a grant in force for the person
// who sent it: the control record cites it, names the agent graded
// signed, not claimed, and the act is booked as the agent's use of its
// grant. An agent with no grant in force, under another person's grant,
// acting for someone else than its grant's person, or under another
// agent's name, is refused.
func TestAgentActTakenUnderItsGrant(t *testing.T) {
	a := newAgentDecide(t, "agent:assistant")
	k := a.agent("assistant", "dana", false)
	signed := a.act(k, "dana", ownWait)
	code, p := a.decide(agentRequest(ownWait, map[string]string{ActHeader: signed, "X-C4-Agent": "assistant"}))
	if code != http.StatusOK {
		t.Fatalf("the agent's act under its grant: %d", code)
	}
	if code, _ := a.decide(agentRequest(ownWait, map[string]string{ActHeader: signed})); code != http.StatusForbidden {
		t.Errorf("the same act again: %d", code)
	}
	if p.Act == nil || p.Custody != CustodySigned || p.Agent != "assistant" || p.AgentGrade != CustodySigned {
		t.Errorf("the record cites %+v, custody %q, agent %q graded %q", p.Act, p.Custody, p.Agent, p.AgentGrade)
	}
	if a.grants.used != 1 {
		t.Errorf("the act was booked as the grant's use %d times", a.grants.used)
	}
	lapsed := a.agent("assistant", "dana", true)
	noors := a.agent("assistant", "noor", false)
	other := a.agent("helper", "dana", false)
	for name, c := range map[string]struct {
		act     string
		headers map[string]string
	}{
		"no grant in force":           {a.act(lapsed, "dana", ownWait), nil},
		"another person's grant":      {a.act(noors, "noor", ownWait), nil},
		"for someone else":            {a.act(k, "noor", ownWait), nil},
		"another agent's name":        {a.act(k, "dana", ownWait), map[string]string{"X-C4-Agent": "helper"}},
		"another agent's act":         {a.act(other, "dana", ownWait), nil},
		"an agent's act on no header": {a.act(k, "dana", ownWait), nil},
	} {
		h := map[string]string{ActHeader: c.act}
		for k, v := range c.headers {
			h[k] = v
		}
		code, _ := a.decide(agentRequest(ownWait, h))
		want := http.StatusForbidden
		if name == "an agent's act on no header" {
			// The act names the agent; the header isn't needed.
			want = http.StatusOK
		}
		if code != want {
			t.Errorf("%s: %d, want %d", name, code, want)
		}
	}
	// Without the people's issuer, no agent's act is taken.
	a.s.AgentGrants = nil
	if code, _ := a.decide(agentRequest(ownWait, map[string]string{ActHeader: a.act(k, "dana", ownWait)})); code != http.StatusForbidden {
		t.Errorf("no issuer: %d", code)
	}
}

// A decision that says waitsFor is checked against the job's actual wait
// before its act is taken: a mismatch is refused, and the act is still
// unspent, so the same act on the wait it names is taken. An agent's act
// decides only a wait naming that agent, and never a person's.
func TestDecideWaitsForCheckedBeforeTheAct(t *testing.T) {
	a := newAgentDecide(t, "person:dana")
	k := a.agent("assistant", "dana", false)
	act := a.act(k, "dana", ownWait)
	if code, _ := a.decide(agentRequest(ownWait, map[string]string{ActHeader: act})); code != http.StatusConflict {
		t.Fatalf("an agent's decision for agent:assistant on dana's wait: %d, want 409", code)
	}
	if a.grants.used != 0 {
		t.Fatal("the refused act was taken")
	}
	if code, _ := a.decide(agentRequest(ownWait, nil)); code != http.StatusConflict {
		t.Errorf("a request with no act that says the wrong waitsFor: %d, want 409", code)
	}
	// The agent's act on dana's wait without saying whose: a person's
	// decision, which no agent makes.
	plain := `{"unit": "ruling", "do": "approve"}`
	if code, _ := a.decide(agentRequest(plain, map[string]string{ActHeader: a.act(k, "dana", plain)})); code != http.StatusForbidden {
		t.Errorf("an agent's act on a person's wait: %d, want 403", code)
	}
	// Another agent's wait.
	b := newAgentDecide(t, "agent:helper")
	kb := b.agent("assistant", "dana", false)
	helper := `{"unit": "ruling", "do": "approve", "waitsFor": "agent:helper"}`
	if code, _ := b.decide(agentRequest(helper, map[string]string{ActHeader: b.act(kb, "dana", helper)})); code != http.StatusForbidden {
		t.Errorf("assistant deciding helper's wait: %d, want 403", code)
	}
	// The same act, unspent, once the wait is the one it names.
	a.s.m.byKey["show-a/sh040-light-v007"].job.Status.Waits[0].Server = "agent:assistant"
	if code, _ := a.decide(agentRequest(ownWait, map[string]string{ActHeader: act})); code != http.StatusOK || a.grants.used != 1 {
		t.Errorf("the act refused for its wait, then on the wait it names: %d", code)
	}
}

// An agent's decision is taken only for a wait naming that agent, whatever
// the request around it: the act itself is checked, not only the wait.
func TestAgentDecidesOnlyItsOwnWait(t *testing.T) {
	a := newAgentDecide(t, "agent:helper")
	k := a.agent("assistant", "dana", false)
	helper := `{"unit": "ruling", "do": "approve", "waitsFor": "agent:helper"}`
	r := agentRequest(helper, map[string]string{ActHeader: a.act(k, "dana", helper)})
	v := &api.Viewer{User: "dana"}
	if _, err := a.s.citeAct(r.Context(), r, v, a.on, "decide"); err == nil || !strings.Contains(err.Error(), "decides only a wait that names it") {
		t.Errorf("assistant's act deciding helper's wait: %v", err)
	}
	if a.grants.used != 0 {
		t.Error("the refused act was booked as the grant's use")
	}
}
