package mcpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// keyDir is a farm's key directory under a scratch HOME, as the keystore
// lays it out (~/.c4/<farm>/keys): never the real one.
func keyDir(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".c4", "studio", "keys")
}

// The agent's own key is a P-256 key in the farm's key directory, in a
// file only the person can read, named as the keystore names a workload's
// key, made once and used from then on: another agent's is another key,
// and a kind that isn't a workload's, or a name that isn't one, is
// refused.
func TestAgentKeyMadeOnceInItsFile(t *testing.T) {
	dir := keyDir(t)
	keys := KeyFiles{Dir: dir}
	k, err := keys.Key(Workload("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workload-claude-code.key")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v, mode %v", err, fi.Mode().Perm())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("the key directory's mode %v", di.Mode().Perm())
	}
	again, err := keys.Key(Workload("claude-code"))
	if err != nil || string(again.SPKI()) != string(k.SPKI()) {
		t.Errorf("the key read again is another: %v", err)
	}
	if other, _ := keys.Key(Workload("reviewer")); other == nil || string(other.SPKI()) == string(k.SPKI()) {
		t.Error("another agent shares the key")
	}
	for _, bad := range []Kind{Workload(""), Workload("../people"), Workload("a/b"), Workload(".hidden"), Workload(strings.Repeat("a", 65)), "device", "touch"} {
		if _, err := keys.Key(bad); err == nil {
			t.Errorf("key %q is made", bad)
		}
	}
	// The keystore's file form: PKCS#8 P-256 in PEM, so K4's store reads it.
	b, _ := os.ReadFile(path)
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatal("not a PEM private key")
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if _, ok := pk.(*ecdsa.PrivateKey); err != nil || !ok {
		t.Fatalf("not a PKCS#8 P-256 key: %v", err)
	}
	sig, err := k.Sign([]byte("a message"))
	pub, _ := x509.ParsePKIXPublicKey(k.SPKI())
	if err != nil || provenance.VerifyP256(pub.(*ecdsa.PublicKey), []byte("a message"), sig) != nil {
		t.Errorf("the key's signature is not in the farm's one form: %v", err)
	}
}

// agentHUD is a HUD with its people's issuer: it certifies the agent's
// key under noor's grant while granted is set, serves one job, and keeps
// each control it is sent and the act it carried.
type agentHUD struct {
	t       *testing.T
	f       *provtest.Farm
	grant   string
	mu      sync.Mutex
	granted bool
	asked   int
	posts   []agentPost
	// tamper changes the certificate the issuer answers.
	tamper func(c *people.AgentCertified, pub *ecdsa.PublicKey, ca *provenance.CA)
}

type agentPost struct {
	path string
	body []byte
	act  []byte
}

func newAgentHUD(t *testing.T) *agentHUD {
	return &agentHUD{t: t, f: provtest.NewFarm(t, time.Now()), grant: provtest.ID("noor's grant to claude-code"), granted: true}
}

const agentJobUID = "4c1d2b9e-7a35-4e0f-9b61-2d8a5c3e7f40"

var agentJobSpec = strings.Repeat("ab", 32)

func (h *agentHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Path == "/issuer/v1/people/config":
		json.NewEncoder(w).Encode(people.Config{Farm: h.f.ID})
	case r.URL.Path == "/issuer/v1/people/agent":
		h.asked++
		var e people.AgentEnrolment
		json.NewDecoder(r.Body).Decode(&e)
		spki, _ := base64.RawURLEncoding.DecodeString(e.Request.Key)
		pk, _ := x509.ParsePKIXPublicKey(spki)
		pub, _ := pk.(*ecdsa.PublicKey)
		sig, _ := base64.RawURLEncoding.DecodeString(e.Proof)
		if !h.granted || pub == nil || provenance.VerifyP256(pub, e.Request.ProofMessage(), sig) != nil || e.Request.Farm != h.f.ID {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "no agent grant in force: agent claude-code's key is granted by no one now"})
			return
		}
		device := schema.DeviceID(spki)
		leaf, err := h.f.CA.IssueLeaf(pub, provenance.AgentIdentity(e.Request.Agent), device, h.grant, people.LeafTTL, time.Now())
		if err != nil {
			h.t.Error(err)
		}
		c := people.AgentCertified{AgentGrant: people.AgentGrant{Grant: h.grant, Agent: e.Request.Agent, Device: device, Person: "noor"},
			Farm: h.f.ID, Identity: provenance.CertIdentity(leaf), Chain: [][]byte{leaf.Raw, h.f.CA.Cert.Raw}, Expires: leaf.NotAfter}
		if h.tamper != nil {
			h.tamper(&c, pub, h.f.CA)
		}
		json.NewEncoder(w).Encode(c)
	case r.Method == "GET" && r.URL.Path == "/api/v1/jobs/show-a/sh040-light-v007":
		json.NewEncoder(w).Encode(api.Job{Namespace: "show-a", Name: "sh040-light-v007", UID: agentJobUID, SpecDigest: agentJobSpec,
			Waits: []api.Wait{{Unit: 3, Name: "ruling", WaitsFor: "agent:claude-code"}, {Unit: 4, Name: "approval", WaitsFor: "person:noor"}}})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/show-a/sh040-light-v007/"):
		body, _ := io.ReadAll(r.Body)
		var act []byte
		if hdr := r.Header.Get("X-C4-Act"); hdr != "" {
			act, _ = base64.StdEncoding.DecodeString(hdr)
		}
		h.posts = append(h.posts, agentPost{path: strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/show-a/sh040-light-v007/"), body: body, act: act})
		json.NewEncoder(w).Encode(map[string]string{"message": "done"})
	default:
		http.NotFound(w, r)
	}
}

func (h *agentHUD) last() agentPost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posts[len(h.posts)-1]
}

// On its first connection the server makes the agent's key and the
// issuer certifies it under the person's grant. Its controls on a job are
// then the agent's own acts: on the job by UID and the spec the HUD
// serves, the request's body exactly as their params, by the agent for
// the grant's person, under the grant, and verifying as the farm takes
// them. A decision of the agent's own wait says so (waitsFor); one of a
// person's wait is not signed as the agent's, nor is a control the agent
// doesn't sign.
func TestAgentSignsItsControlsUnderItsGrant(t *testing.T) {
	h := newAgentHUD(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := &Client{Base: srv.URL, Agent: "claude-code", Keys: KeyFiles{Dir: keyDir(t)}}
	ctx := context.Background()
	if why := c.Connect(ctx); why != "" {
		t.Fatalf("the first connection: %s", why)
	}
	job := JobRef{Namespace: "show-a", Name: "sh040-light-v007"}
	seen := provenance.NewMemorySeen()
	check := func(what, verb string) map[string]any {
		t.Helper()
		p := h.last()
		if p.act == nil {
			t.Fatalf("%s carried no act", what)
		}
		v, a, err := provenance.VerifyAct(p.act, h.f.Roots, time.Now(), seen)
		if err != nil {
			t.Fatalf("%s: the act doesn't verify: %v", what, err)
		}
		on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", agentJobUID, agentJobSpec)
		bodyID, _ := schema.ParamsID(p.body)
		paramsID, _ := schema.ParamsID(a.Predicate.Params)
		switch pr := a.Predicate; {
		case a.Subject[0].Name != on.Name || a.Subject[0].Digest["sha256"] != agentJobSpec:
			t.Errorf("%s: the act is on %v", what, a.Subject)
		case pr.Act != verb || bodyID != paramsID:
			t.Errorf("%s: the act is %s with params %s, the request %s", what, pr.Act, pr.Params, p.body)
		case pr.Chain.By.Agent != "claude-code" || pr.Chain.For.Party != "noor" || v.Grant != h.grant || pr.Chain.Under[0].Grant != h.grant:
			t.Errorf("%s: by %+v for %+v under %.16s", what, pr.Chain.By, pr.Chain.For, v.Grant)
		case pr.Device != "":
			t.Errorf("%s: an agent's act names device %s, which is a person's field", what, pr.Device)
		}
		var body map[string]any
		json.Unmarshal(p.body, &body)
		return body
	}
	if _, _, err := c.decide(ctx, nil, DecideIn{JobRef: job, Unit: "ruling", Do: "approve"}); err != nil {
		t.Fatal(err)
	}
	if body := check("deciding its own wait", "decide"); body["waitsFor"] != "agent:claude-code" {
		t.Errorf("the decision says waitsFor %v", body["waitsFor"])
	}
	if _, _, err := c.control(ctx, "/jobs/show-a/sh040-light-v007/steer", map[string]any{"unit": "3", "text": "use the wide lens"}); err != nil {
		t.Fatal(err)
	}
	check("steering", "steer")
	if _, _, err := c.decide(ctx, nil, DecideIn{JobRef: job, Unit: "approval", Do: "approve"}); err != nil {
		t.Fatal(err)
	}
	if p := h.last(); p.act != nil || strings.Contains(string(p.body), "waitsFor") {
		t.Errorf("a person's wait was decided as the agent's act: %s", p.body)
	}
	if _, _, err := c.holdJob(ctx, nil, HoldIn{JobRef: job, Hold: true}); err != nil {
		t.Fatal(err)
	}
	if p := h.last(); p.act != nil {
		t.Error("a hold, which the agent doesn't sign, carried an act")
	}
}

// With no grant in force the issuer certifies nothing: the key is made
// all the same, the controls go as before, on the person's token, and the
// issuer isn't asked again on every control.
func TestAgentWithoutAGrantSendsAsBefore(t *testing.T) {
	h := newAgentHUD(t)
	h.granted = false
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := keyDir(t)
	c := &Client{Base: srv.URL, Agent: "claude-code", Keys: KeyFiles{Dir: dir}}
	ctx := context.Background()
	if why := c.Connect(ctx); !strings.Contains(why, "no agent grant") {
		t.Fatalf("the first connection without a grant: %q", why)
	}
	if _, err := os.Stat(filepath.Join(dir, "workload-claude-code.key")); err != nil {
		t.Errorf("the key wasn't made on the first connection: %v", err)
	}
	job := JobRef{Namespace: "show-a", Name: "sh040-light-v007"}
	for range 3 {
		if _, _, err := c.decide(ctx, nil, DecideIn{JobRef: job, Unit: "ruling", Do: "approve"}); err != nil {
			t.Fatal(err)
		}
		if p := h.last(); p.act != nil || strings.Contains(string(p.body), "waitsFor") {
			t.Errorf("sent with an act, or changed: %s", p.body)
		}
	}
	if h.asked != 1 {
		t.Errorf("the issuer was asked %d times, want once", h.asked)
	}
}

// Serving its first connection, the server makes the agent's key and asks
// for its certificate before any tool is called.
func TestServeConnectsTheAgentKey(t *testing.T) {
	h := newAgentHUD(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := keyDir(t)
	c := &Client{Base: srv.URL, Agent: "claude-code", Keys: KeyFiles{Dir: dir}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, st := mcp.NewInMemoryTransports()
	go Serve(ctx, c, st)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		asked := h.asked
		h.mu.Unlock()
		if _, err := os.Stat(filepath.Join(dir, "workload-claude-code.key")); err == nil && asked == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("serving, the server made no key, or didn't ask for its certificate")
}

// Probe (K5 review): a key file planted before the server's first run is
// refused, never used: a symlink to another file, one others can read, or
// one in a directory others can read. A file cut short by a crash is kept
// aside and a new key made, which needs a new grant, and the server says
// so.
func TestPlantedOrBrokenKeyFileRefused(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemb := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	cases := map[string]func(dir string) error{
		"a symlink to another file": func(dir string) error {
			elsewhere := filepath.Join(filepath.Dir(dir), "planted.key")
			if err := os.WriteFile(elsewhere, pemb, 0o600); err != nil {
				return err
			}
			return os.Symlink(elsewhere, filepath.Join(dir, "workload-claude-code.key"))
		},
		"a file others can read": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "workload-claude-code.key"), pemb, 0o644)
		},
		"a directory where the key file is": func(dir string) error {
			return os.Mkdir(filepath.Join(dir, "workload-claude-code.key"), 0o600)
		},
		"a directory others can read": func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "workload-claude-code.key"), pemb, 0o600); err != nil {
				return err
			}
			return os.Chmod(dir, 0o755)
		},
	}
	for name, plant := range cases {
		dir := keyDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := plant(dir); err != nil {
			t.Fatal(err)
		}
		if got, err := (KeyFiles{Dir: dir}).Key(Workload("claude-code")); err == nil || got != nil {
			t.Errorf("%s: used, %v", name, err)
		}
	}
	// Cut short.
	dir := keyDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workload-claude-code.key")
	if err := os.WriteFile(path, pemb[:20], 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (KeyFiles{Dir: dir}).Key(Workload("claude-code"))
	var remade *KeyRemade
	if !errors.As(err, &remade) || got == nil || !strings.Contains(err.Error(), "needs a new grant") {
		t.Fatalf("a truncated key file: %v, key %v", err, got != nil)
	}
	if b, _ := os.ReadFile(remade.Aside); string(b) != string(pemb[:20]) {
		t.Error("the broken file wasn't kept aside")
	}
	if again, err := (KeyFiles{Dir: dir}).Key(Workload("claude-code")); err != nil || string(again.SPKI()) != string(got.SPKI()) {
		t.Errorf("the new key isn't the one kept: %v", err)
	}
	// The server says so until a person grants the new key.
	h := newAgentHUD(t)
	h.granted = false
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir2 := keyDir(t)
	os.MkdirAll(dir2, 0o700)
	os.WriteFile(filepath.Join(dir2, "workload-claude-code.key"), pemb[:20], 0o600)
	c := &Client{Base: srv.URL, Agent: "claude-code", Keys: KeyFiles{Dir: dir2}}
	if why := c.Connect(context.Background()); !strings.Contains(why, "a new key was made, which needs a new grant") {
		t.Errorf("the server's answer after remaking the key: %q", why)
	}
}

// The server checks a certificate the issuer returns before it signs
// with it: one for another key, another agent, another grant than it
// says, no person, another farm's CA, or an end other than its leaf's is
// refused, and the agent's controls go unsigned.
func TestReturnedCertificateCheckedBeforeUse(t *testing.T) {
	other := provtest.NewFarm(t, time.Now())
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for name, tamper := range map[string]func(c *people.AgentCertified, pub *ecdsa.PublicKey, ca *provenance.CA){
		"another key's leaf": func(c *people.AgentCertified, _ *ecdsa.PublicKey, ca *provenance.CA) {
			spki, _ := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
			leaf, _ := ca.IssueLeaf(&otherKey.PublicKey, c.Identity, schema.DeviceID(spki), c.Grant, time.Hour, time.Now())
			c.Chain[0], c.Expires = leaf.Raw, leaf.NotAfter
		},
		"another agent's leaf": func(c *people.AgentCertified, pub *ecdsa.PublicKey, ca *provenance.CA) {
			leaf, _ := ca.IssueLeaf(pub, provenance.AgentIdentity("other-agent"), c.Device, c.Grant, time.Hour, time.Now())
			c.Chain[0], c.Expires = leaf.Raw, leaf.NotAfter
		},
		"a grant its leaf doesn't name": func(c *people.AgentCertified, _ *ecdsa.PublicKey, _ *provenance.CA) {
			c.Grant = provtest.ID("another grant")
		},
		"no person": func(c *people.AgentCertified, _ *ecdsa.PublicKey, _ *provenance.CA) { c.Person = "" },
		"another farm's CA": func(c *people.AgentCertified, pub *ecdsa.PublicKey, _ *provenance.CA) {
			leaf, _ := other.CA.IssueLeaf(pub, c.Identity, c.Device, c.Grant, time.Hour, time.Now())
			c.Chain, c.Expires = [][]byte{leaf.Raw, other.CA.Cert.Raw}, leaf.NotAfter
		},
		"a device other than its key's": func(c *people.AgentCertified, _ *ecdsa.PublicKey, _ *provenance.CA) {
			c.Device = provtest.ID("another key")
		},
		"a leaf its CA didn't sign": func(c *people.AgentCertified, pub *ecdsa.PublicKey, _ *provenance.CA) {
			leaf, _ := other.CA.IssueLeaf(pub, c.Identity, c.Device, c.Grant, time.Hour, time.Now())
			c.Chain[0], c.Expires = leaf.Raw, leaf.NotAfter
		},
		"an end other than its leaf's": func(c *people.AgentCertified, _ *ecdsa.PublicKey, _ *provenance.CA) {
			c.Expires = c.Expires.Add(24 * time.Hour)
		},
	} {
		h := newAgentHUD(t)
		h.tamper = tamper
		srv := httptest.NewServer(h)
		c := &Client{Base: srv.URL, Agent: "claude-code", Keys: KeyFiles{Dir: keyDir(t)}}
		if why := c.Connect(context.Background()); !strings.Contains(why, "the issuer's certificate") {
			t.Errorf("%s: certified: %q", name, why)
		}
		if _, _, err := c.decide(context.Background(), nil, DecideIn{JobRef: JobRef{Namespace: "show-a", Name: "sh040-light-v007"}, Unit: "ruling", Do: "approve"}); err != nil {
			t.Fatal(err)
		}
		if p := h.last(); p.act != nil {
			t.Errorf("%s: a control signed with it", name)
		}
		srv.Close()
	}
}
