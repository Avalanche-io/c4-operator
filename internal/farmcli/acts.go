package farmcli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Signed acts from the CLI (PEOPLE-KEYS-2 2, K4): a request that is a
// person's act carries it, signed with this machine's device key, and with
// the touch key too where the farm's list says the act takes a touch, in
// X-C4-Act. The act is on the subject the HUD serves for it (api.ActOn)
// and its params are the request's body, byte for byte.
//
// Only a person signs: at a terminal, never inside an agent and never
// under --yes. Anywhere else the request goes as before, unsigned, until
// the agent's own key (K5); K8 then refuses it. A machine whose key isn't
// enrolled at the farm, or a HUD from before signed acts, sends it
// unsigned too.

// ActHeader carries a request's signed act, as the HUD reads it.
const ActHeader = "X-C4-Act"

// Acts signs a person's acts at a farm with this machine's keys.
type Acts struct {
	// Store is the farm's keys on this machine.
	Store keystore.Store
	// Present says a person is here to sign: at a terminal, not inside an
	// agent, not under --yes.
	Present bool
	// As is the role a local farm's view acts in, if any.
	As string
	// Say tells the person why an act went unsigned, once a kind.
	Say func(format string, a ...any)
	// Pinned is the farm's owner key as up pinned it, on a farm up made:
	// nothing is signed for a farm whose index books another.
	Pinned string

	needs map[string]api.ActNeed
	farm  string
	read  bool
	bad   error
	said  map[string]bool
}

// errNotSigned means the act goes unsigned, as before signed acts.
var errNotSigned = errors.New("not signed")

// header is the act's bundle, base64, for X-C4-Act; errNotSigned when it
// goes unsigned.
func (a *Acts) header(ctx context.Context, c *Client, act string, on *api.ActOn, body []byte) (string, error) {
	if a == nil || !a.Present || on == nil || on.Name == "" {
		return "", errNotSigned
	}
	cert, err := LoadCert(a.Store, keystore.Device)
	if err != nil {
		return "", err
	}
	if cert == nil {
		return "", errNotSigned
	}
	if !a.read {
		a.read = true
		var n api.ActNeeds
		if err := c.Do(ctx, http.MethodGet, "/acts/needs", nil, &n); err == nil {
			a.needs = n.Acts
		}
		// the farm the act is made for is the one this machine's key was
		// certified at, from its own chain, never the HUD's word
		if a.farm, err = certFarm(cert); err != nil {
			return "", err
		}
		if a.Pinned != "" {
			cfg, err := PeopleConfig(ctx, c)
			if err == nil {
				err = CheckOwner(ctx, c, a.Store, cfg, a.Pinned)
			}
			if err != nil {
				a.bad = err
			}
		}
	}
	if a.bad != nil {
		return "", a.bad
	}
	if a.needs == nil || a.farm == "" {
		// a HUD from before signed acts, or one with no people's issuer
		return "", errNotSigned
	}
	need := a.needs[act]
	person := strings.TrimPrefix(cert.Identity, provenance.IdentityBase+"user/")
	pa := provenance.PersonAct{Farm: a.farm, Subject: provenance.Subject{Name: on.Name, Digest: on.Digest}, Act: act,
		Params: json.RawMessage(body), Person: person, As: a.As, SignedAt: time.Now().UTC()}
	key, err := a.Store.Key(keystore.Device)
	if err != nil {
		return "", err
	}
	var touch keystore.Key
	var touchCert *provenance.DeviceCert
	if need.Touch {
		tc, err := LoadCert(a.Store, keystore.Touch)
		if err != nil {
			return "", err
		}
		if tc == nil || !a.Store.Has(keystore.Touch) {
			// until a touch key is enrolled the act goes as before; K8
			// refuses it then, and c4 farm touch-key enrols one
			a.once(act, "%s takes your touch, and this machine has no touch key at the farm: it goes unsigned for now (c4 farm touch-key enrols one)\n", act)
			return "", errNotSigned
		}
		if touch, err = a.Store.Key(keystore.Touch); err != nil {
			return "", err
		}
		// what the touch confirms, shown by the CLI, the display a HUD
		// doesn't control, and in the system's own prompt (4.3)
		what := ActReason(act, on.Name, body)
		if a.Say != nil {
			a.Say("touch     %s: your Mac asks for your touch to confirm it\n", what)
		}
		touch = keystore.WithReason(touch, what)
		d := deviceCert(tc)
		touchCert, pa.Confirmed = &d, true
	}
	b, err := provenance.SignPersonAct(pa, key, deviceCert(cert), touch, touchCert)
	if err != nil {
		return "", fmt.Errorf("signing %s: %w", act, err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ActReason is an act as the person is asked to confirm it: its verb, what
// it is on, and its params, printable and short.
func ActReason(act, on string, params []byte) string {
	p := Printable(string(params))
	if len(p) > 120 {
		p = p[:120] + "…"
	}
	if p == "{}" || p == "" {
		return "c4: " + Printable(act) + " " + Printable(on)
	}
	return "c4: " + Printable(act) + " " + Printable(on) + " " + p
}

func (a *Acts) once(kind, format string, v ...any) {
	if a.Say == nil || a.said[kind] {
		return
	}
	if a.said == nil {
		a.said = map[string]bool{}
	}
	a.said[kind] = true
	a.Say(format, v...)
}

// DoAct is Do for a request that is a person's act on the subject on: it
// carries the act, signed with this machine's keys (Acts), when a person
// is here to sign it, and goes as before otherwise.
//
// A request with no body (in nil) sends none, and its act's params are
// {}, as the HUD reads an empty body.
func (c *Client) DoAct(ctx context.Context, method, path, act string, on *api.ActOn, in, out any, also ...int) error {
	var b []byte
	params := []byte("{}")
	if in != nil {
		var err error
		if b, err = json.Marshal(in); err != nil {
			return err
		}
		params = b
	}
	h, err := c.Acts.header(ctx, c, act, on, params)
	switch {
	case errors.Is(err, errNotSigned):
		h = ""
	case err != nil:
		return err
	}
	return c.send(ctx, apiPrefix, method, path, b, h, out, also...)
}

// JobActOn is a job as an act on it names it: workjob:<ns>/<name>@<uid>
// at the spec digest the farm served; nil from a farm that serves neither.
func JobActOn(j *api.Job) *api.ActOn {
	if j == nil || j.UID == "" || j.SpecDigest == "" {
		return nil
	}
	s := provenance.ActSubject("workjob", j.Namespace, j.Name, j.UID, j.SpecDigest)
	return &api.ActOn{Name: s.Name, Digest: s.Digest}
}

// ProjectActOn is a project as an act that makes something in it names
// it, from the snapshot; nil when the farm serves none.
// A subject the farm serves is checked against what the person named: an
// act on another project is refused before anything is signed.
func ProjectActOn(ctx context.Context, c *Client, project string) (*api.ActOn, error) {
	if c.Acts == nil || !c.Acts.Present || project == "" {
		return nil, nil
	}
	s, err := Snapshot(ctx, c)
	if err != nil {
		return nil, nil
	}
	for _, p := range s.Projects {
		if p.Name == project {
			return p.ActOn, checkActOn(p.ActOn, "project", "", project)
		}
	}
	return nil, nil
}

// checkActOn checks a subject the farm served for an act on kind ns/name
// (a project's: name alone) names that object, by its UID, at a spec
// digest: never another, whatever the HUD says.
func checkActOn(on *api.ActOn, kind, ns, name string) error {
	if on == nil {
		return nil
	}
	want := kind + ":" + ns + "/" + name + "@"
	if kind == "project" {
		want = "project:" + name + "@"
	}
	uid, ok := strings.CutPrefix(on.Name, want)
	if !ok || uid == "" || strings.ContainsAny(uid, "/@: ") || len(on.Digest) != 1 || len(on.Digest["sha256"]) != 64 {
		return fmt.Errorf("the farm served %q to sign for %s %s: refused, nothing was signed", Printable(on.Name), kind, strings.TrimPrefix(ns+"/"+name, "/"))
	}
	return nil
}

// send is doAt with the body as bytes and an act to carry, if any.
func (c *Client) send(ctx context.Context, prefix, method, path string, body []byte, act string, out any, also ...int) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var rd io.Reader
	ctype := ""
	if body != nil {
		rd, ctype = bytes.NewReader(body), "application/json"
	}
	req, err := http.NewRequestWithContext(cctx, method, c.urlAt(prefix, path), rd)
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	c.headers(req, ctype)
	if act != "" {
		req.Header.Set(ActHeader, act)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("the farm at %s is unreachable: %v", c.Base, err)
	}
	defer resp.Body.Close()
	return answer(resp, out, also)
}
