package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// An agent's act (PEOPLE-KEYS-2 5.2, K5): the MCP server signs with a key
// of its own, certified as agent/<name> under a person's grant, and the
// HUD takes the act only while that grant is in force, for the person who
// sent it. The agent's use of the grant is booked, which is what keeps it
// from lapsing (DESIGN 10's exception, P5).

// AgentGrants judges an agent's grant: the people's issuer, which books
// the grants in the farm book.
type AgentGrants interface {
	// AgentGrantFor is the grant a verified agent record was signed
	// under, if it is in force now for that agent and key.
	AgentGrantFor(ctx context.Context, v *provenance.Verified) (*people.AgentGrant, error)
	// UseGrant books an act the HUD took as the agent's use of its grant.
	UseGrant(ctx context.Context, v *provenance.Verified, bundle []byte) error
}

// citeAgentAct takes an agent's act whose subject, verb and params citeAct
// has matched: signed by the agent's own key, under a grant in force for
// the person who sent it, for that person; a decision only of a wait
// that names the agent; taken once.
func (s *Server) citeAgentAct(ctx context.Context, r *http.Request, v *api.Viewer, data []byte, vr *provenance.Verified, a *schema.Act, subject provenance.Subject, action string) (*citedAct, error) {
	p := a.Predicate
	agent := p.Chain.By.Agent
	switch h := claimedAgent(r); {
	case s.AgentGrants == nil:
		return nil, fmt.Errorf("%w is agent %s's, and this farm keeps no agents' grants (its people's issuer and farm book)", errAct, agent)
	case vr.Identity != provenance.AgentIdentity(agent):
		return nil, fmt.Errorf("%w names agent %s and is signed by %s, not by its key", errAct, agent, vr.Identity)
	case v == nil:
		return nil, fmt.Errorf("%w is agent %s's, and no one signed in sent it", errAct, agent)
	case v.Unit != nil:
		return nil, fmt.Errorf("%w is agent %s's, and unit %s sent it: an agent outside a job acts through the MCP server", errAct, agent, v.Unit.Unit)
	case h != "" && h != agent:
		return nil, fmt.Errorf("%w is agent %s's, and the request names agent %s", errAct, agent, h)
	}
	if action == "decide" {
		// An agent decides only its own wait; the handler has checked the
		// wait is the one the act names (handleDecide).
		var q struct {
			WaitsFor string `json:"waitsFor"`
		}
		_ = json.Unmarshal(p.Params, &q)
		if q.WaitsFor != "agent:"+agent {
			return nil, fmt.Errorf("%w: agent %s decides only a wait that names it, and the act says waitsFor %q", errAct, agent, q.WaitsFor)
		}
	}
	g, err := s.AgentGrants.AgentGrantFor(ctx, vr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAct, err)
	}
	switch {
	case g.Person != v.User && s.ownerFor(ctx, v) != g.Person:
		return nil, fmt.Errorf("%w is agent %s's under %s's grant, and %s sent it", errAct, agent, g.Person, v.User)
	case p.Chain.For.Party != g.Person:
		return nil, fmt.Errorf("%w is for %s, and agent %s's grant is %s's", errAct, p.Chain.For.Party, agent, g.Person)
	}
	taken, _, err := provenance.VerifyAct(data, s.trust(ctx).roots, time.Now(), s.actsSeen())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAct, err)
	}
	if err := s.AgentGrants.UseGrant(ctx, taken, data); err != nil {
		// The act is taken; a use not booked only brings the lapse nearer.
		s.Log.Error(err, "book an agent's use of its grant", "agent", agent, "grant", g.Grant)
	}
	return &citedAct{cite: &schema.ActCitation{Payload: provenance.PayloadHash(vr.Payload), Record: c4.Identify(bytes.NewReader(data)).String()},
		params: p.Params, grade: p.Chain.By.Grade, on: subject, bundle: data, agent: agent, grant: g.Grant}, nil
}

// agentOnRecord names the agent whose own act a control record carries
// out, graded as the act is (signed): no longer the client's word.
func (a *citedAct) agentOnRecord(pred *controlPredicate) {
	if a != nil && a.agent != "" {
		pred.Agent, pred.AgentGrade = a.agent, a.grade
	}
}

// actAgent is the agent a request's act names, read without verifying it:
// whom a decision is checked as before citeAct verifies the act, which
// refuses it if it isn't that agent's.
func actAgent(r *http.Request) string {
	data, err := base64.StdEncoding.DecodeString(r.Header.Get(ActHeader))
	if err != nil || len(data) == 0 {
		return ""
	}
	var b provenance.Bundle
	if json.Unmarshal(data, &b) != nil {
		return ""
	}
	payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		return ""
	}
	a, errs := schema.ParseAct(payload)
	if errs != nil {
		return ""
	}
	return a.Predicate.Chain.By.Agent
}
