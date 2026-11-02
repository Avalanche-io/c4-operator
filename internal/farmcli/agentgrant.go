package farmcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// An agent's grant (PEOPLE-KEYS-2 5.2, K5): a touched c4 farm integrate
// makes the agent's own key on this machine (the keystore's
// workload-<agent>), and the person grants it, a confirmed grant act on
// device:<its ID>. The people's issuer certifies the key as agent/<name>
// only while that grant is in force; the MCP server, the one signer for
// the agent, then signs the agent's acts with it. The grant ends when the
// integration is removed (a revoke on grant:<ID>) or after 90 days with no
// act, and nothing renews it silently: a touched integrate makes a fresh
// one.

// ErrNoTouchKey means granting needs the person's touch key, and this
// machine has none at the farm.
var ErrNoTouchKey = errors.New("granting an agent its key takes your touch, and this machine has no touch key at the farm: c4 farm touch-key makes one, then c4 farm integrate on")

// AgentGrant is a grant made, as the CLI tells it.
type AgentGrant struct {
	Grant, Agent, Device string
	Lapses               time.Time
}

// GrantAgent grants agent its own key on this machine, as this machine's
// person: the key made if it isn't yet, the grant act signed with the
// device key and the touch key, and booked. It answers the grant's ID.
func GrantAgent(ctx context.Context, c *Client, st keystore.Store, agent string) (*AgentGrant, error) {
	dev, err := LoadCert(st, keystore.Device)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		return nil, errors.New("this machine's key is not enrolled at the farm: c4 farm up or c4 farm login enrols it")
	}
	tc, err := LoadCert(st, keystore.Touch)
	if err != nil {
		return nil, err
	}
	if tc == nil || !st.Has(keystore.Touch) {
		return nil, ErrNoTouchKey
	}
	cfg, err := PeopleConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	ak, err := st.Key(keystore.Workload(agent))
	if err != nil {
		return nil, fmt.Errorf("the agent's key: %w", err)
	}
	id := deviceID(ak)
	params, err := json.Marshal(map[string]string{"agent": agent, "device": id})
	if err != nil {
		return nil, err
	}
	act, err := personAct(st, dev, tc, provenance.PersonAct{Farm: cfg.Farm, Subject: provenance.Subject{Name: "device:" + id, Digest: map[string]string{"c4": id}},
		Act: people.ActGrant, Params: params, Confirmed: true})
	if err != nil {
		return nil, err
	}
	var b people.Booked
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/acts", json.RawMessage(act), &b); err != nil {
		return nil, fmt.Errorf("granting %s its key: %w", agent, err)
	}
	g := &AgentGrant{Grant: b.Record, Agent: agent, Device: id}
	if v, err := AgentGrants(ctx, c, ""); err == nil {
		for _, x := range v {
			if x.Grant == b.Record {
				g.Lapses, _ = time.Parse(time.RFC3339, x.Lapses)
			}
		}
	}
	return g, nil
}

// AgentGrants are a person's agents' grants, in force or ended: the
// caller's own when person is "".
func AgentGrants(ctx context.Context, c *Client, person string) ([]people.GrantView, error) {
	p := "/v1/people/agents"
	if person != "" {
		p += "?person=" + url.QueryEscape(person)
	}
	var out struct {
		Grants []people.GrantView `json:"grants"`
	}
	if err := c.Issuer(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	return out.Grants, nil
}

// RevokeAgent ends agent's grants in force for this machine's key of it,
// as the person removing the integration: a revoke on grant:<ID>, signed
// with the device key (taking authority away needs no touch). It answers
// how many it ended.
func RevokeAgent(ctx context.Context, c *Client, st keystore.Store, agent string) (int, error) {
	dev, err := LoadCert(st, keystore.Device)
	if err != nil || dev == nil {
		return 0, err
	}
	if !st.Has(keystore.Workload(agent)) {
		return 0, nil
	}
	ak, err := st.Key(keystore.Workload(agent))
	if err != nil {
		return 0, err
	}
	id := deviceID(ak)
	cfg, err := PeopleConfig(ctx, c)
	if err != nil {
		return 0, err
	}
	gs, err := AgentGrants(ctx, c, strings.TrimPrefix(dev.Identity, provenance.IdentityBase+"user/"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range gs {
		if !g.InForce || g.Agent != agent || g.Device != id {
			continue
		}
		act, err := personAct(st, dev, nil, provenance.PersonAct{Farm: cfg.Farm, Subject: provenance.Subject{Name: "grant:" + g.Grant, Digest: map[string]string{"c4": g.Grant}},
			Act: people.ActRevoke, Params: json.RawMessage(`{}`)})
		if err != nil {
			return n, err
		}
		if err := c.Issuer(ctx, http.MethodPost, "/v1/people/acts", json.RawMessage(act), nil); err != nil {
			return n, fmt.Errorf("ending %s's grant: %w", agent, err)
		}
		n++
	}
	return n, nil
}

// personAct signs a's act as this machine's person: their device key, and
// with tc their touch key too.
func personAct(st keystore.Store, dev, tc *people.Certified, a provenance.PersonAct) ([]byte, error) {
	key, err := st.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	a.Person = strings.TrimPrefix(dev.Identity, provenance.IdentityBase+"user/")
	a.SignedAt = time.Now().UTC()
	var touch keystore.Key
	var touchCert *provenance.DeviceCert
	if a.Confirmed {
		if touch, err = st.Key(keystore.Touch); err != nil {
			return nil, err
		}
		d := deviceCert(tc)
		touchCert = &d
	}
	return provenance.SignPersonAct(a, key, deviceCert(dev), touch, touchCert)
}

// AgentHook is an Integration's Agent for farm c with this machine's keys
// st: on, a grant of the agent's own key, which takes the person's touch,
// so only when they are present; off, its revoke. A farm with no people's
// issuer, or none that takes agent grants, says nothing.
func AgentHook(c *Client, st keystore.Store, present bool) func(context.Context, Out, bool) {
	return func(ctx context.Context, o Out, on bool) {
		if _, err := PeopleConfig(ctx, c); err != nil {
			return
		}
		if !on {
			n, err := RevokeAgent(ctx, c, st, AgentName)
			switch {
			case err != nil:
				o.printf("agent     %s's grant was not ended: %v (c4 farm integrate off tries again)\n", AgentName, err)
			case n > 0:
				o.printf("agent     %s's grant of its own key is ended\n", AgentName)
			}
			return
		}
		if !present {
			o.printf("agent     %s acts on your sign-in until you run c4 farm integrate on yourself, at a terminal: its own key's grant takes your touch\n", AgentName)
			return
		}
		g, err := GrantAgent(ctx, c, st, AgentName)
		switch {
		case errors.Is(err, ErrNoTouchKey):
			o.printf("agent     %v\n", err)
		case err != nil:
			o.printf("agent     %s has no key of its own yet: %v (c4 farm integrate on tries again)\n", AgentName, err)
		default:
			until := ""
			if !g.Lapses.IsZero() {
				until = ", until " + o.Clock.At(g.Lapses) + " if it makes no act"
			}
			o.printf("agent     %s signs as itself with its own key (%s), granted by your touch%s; c4 farm integrate off ends it\n", AgentName, ShortDevice(g.Device), until)
		}
	}
}
