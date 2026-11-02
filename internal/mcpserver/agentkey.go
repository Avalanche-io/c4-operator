package mcpserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The MCP server's own key (PEOPLE-KEYS-2 5.2, K5): on its first
// connection it makes a P-256 key under the agent's name, separate from
// the person's, and asks the people's issuer to certify it as
// agent/<name>. The issuer does only while a person's touched grant of that
// key is in force (c4 farm integrate). With a certificate, the server signs
// the controls it sends as the agent's own acts (X-C4-Act); without one it
// sends them as before, on the person's token, and the HUD records the
// agent as the client's claim.

// AgentKey is an agent's key on this machine, shaped as the keystore's
// Key (K4): its public key (DER SubjectPublicKeyInfo), a signature in the
// farm's one form (raw r||s, low S, over the SHA-256 of msg), and what
// keeps it.
type AgentKey interface {
	SPKI() []byte
	Sign(msg []byte) ([]byte, error)
	Protection() string
}

// Kind is which key, as the keystore names it (K4's keystore.Kind).
type Kind string

// Workload is a workload's own key, named for what it signs as: the
// keystore's Workload(name), the file workload-<name>.key.
func Workload(name string) Kind { return Kind("workload-" + name) }

// AgentKeys keeps keys on this machine, shaped as the keystore's Store
// (K4): Key makes the key the first time it is asked for, and loads it
// after. K4's keystore.Store replaces it, its Key(keystore.Workload(name))
// reading the file KeyFiles writes.
type AgentKeys interface {
	Key(kind Kind) (AgentKey, error)
}

// KeyFiles keeps each key in a file in Dir, the farm's key directory
// (~/.c4/<farm>/keys, K4's keystore.Store.Dir), which an integrated
// agent's sandbox denies to the agent itself (P6): <kind>.key, a PKCS#8
// P-256 key in PEM, mode 0600, in a directory of mode 0700, as the
// keystore keeps a key file. Only the MCP server reads it.
type KeyFiles struct{ Dir string }

// Key reads the key of this kind, or makes it the first time.
func (k KeyFiles) Key(kind Kind) (AgentKey, error) {
	if k.Dir == "" {
		return nil, errors.New("no directory for the agent's key")
	}
	name, ok := strings.CutPrefix(string(kind), "workload-")
	if !ok || !validAgent(name) {
		return nil, fmt.Errorf("key %q: a workload's, named with letters, digits, '.', '_' and '-', at most 64", kind)
	}
	path := filepath.Join(k.Dir, string(kind)+".key")
	if b, err := os.ReadFile(path); err == nil {
		return loadKeyFile(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// O_EXCL: two servers starting at once keep the first key made.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return loadKeyFile(b)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return newFileKey(key)
}

func validAgent(a string) bool {
	if a == "" || len(a) > 64 {
		return false
	}
	for i, r := range a {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && strings.ContainsRune("._-", r)
		if !ok {
			return false
		}
	}
	return true
}

func loadKeyFile(b []byte) (AgentKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the agent's key file holds no PEM key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("the agent's key file holds no P-256 key")
	}
	return newFileKey(key)
}

type fileKey struct {
	key  *ecdsa.PrivateKey
	spki []byte
}

func newFileKey(key *ecdsa.PrivateKey) (AgentKey, error) {
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &fileKey{key: key, spki: spki}, nil
}

func (k *fileKey) SPKI() []byte                    { return k.spki }
func (k *fileKey) Sign(msg []byte) ([]byte, error) { return provenance.SignP256(k.key, msg) }
func (k *fileKey) Protection() string              { return schema.ProtectionFile }

// agentID is the agent's key and its certificate, once the issuer gave one.
type agentID struct {
	mu   sync.Mutex
	key  AgentKey
	cert *people.AgentCertified
	got  time.Time
	// tried is when the issuer last refused, so a farm with no grant isn't
	// asked on every control.
	tried  time.Time
	reason string
}

// retryEvery bounds how often the server asks the issuer again after it
// refused a certificate.
const retryEvery = time.Minute

// Connect makes the agent's key, the first time, and asks the people's
// issuer for its certificate: what the server does on its first
// connection. It returns why the agent signs nothing, "" when it does.
func (c *Client) Connect(ctx context.Context) string {
	_, why := c.agentCert(ctx, true)
	return why
}

// agentCert is the agent's key and a certificate with at least half its
// life left, asking the issuer for one when there isn't; nil, and why,
// when the agent signs nothing.
func (c *Client) agentCert(ctx context.Context, force bool) (*agentID, string) {
	if c.Keys == nil || c.Agent == "" || c.Base == "" {
		return nil, "the server has no agent key store, agent name or HUD"
	}
	c.idMu.Lock()
	if c.id == nil {
		c.id = &agentID{}
	}
	id := c.id
	c.idMu.Unlock()
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.key == nil {
		k, err := c.Keys.Key(Workload(c.Agent))
		if err != nil {
			return nil, "the agent's key: " + err.Error()
		}
		id.key = k
	}
	now := time.Now()
	if cc := id.cert; cc != nil && now.Before(id.got.Add(cc.Expires.Sub(id.got)/2)) {
		return id, ""
	}
	if !force && id.cert == nil && now.Sub(id.tried) < retryEvery {
		return nil, id.reason
	}
	cert, err := c.certify(ctx, id.key)
	if err != nil {
		id.tried, id.reason = now, err.Error()
		if id.cert != nil && now.Before(id.cert.Expires.Add(-time.Minute)) {
			return id, "" // the old certificate still lives
		}
		id.cert = nil
		return nil, id.reason
	}
	id.cert, id.got, id.reason = cert, now, ""
	return id, ""
}

// certify asks the people's issuer, at the HUD's origin, to certify key
// for the agent.
func (c *Client) certify(ctx context.Context, key AgentKey) (*people.AgentCertified, error) {
	var cfg people.Config
	if err := c.issuer(ctx, "GET", "/config", nil, &cfg); err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	q := people.AgentRequest{Farm: cfg.Farm, Agent: c.Agent, Key: base64.RawURLEncoding.EncodeToString(key.SPKI()),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: time.Now().UTC().Format(time.RFC3339)}
	sig, err := key.Sign(q.ProofMessage())
	if err != nil {
		return nil, err
	}
	var out people.AgentCertified
	if err := c.issuer(ctx, "POST", "/agent", people.AgentEnrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig)}, &out); err != nil {
		return nil, err
	}
	if len(out.Chain) == 0 || out.Device != schema.DeviceID(key.SPKI()) {
		return nil, errors.New("the issuer's certificate is not for this key")
	}
	return &out, nil
}

// issuer calls the people's issuer at the HUD's origin.
func (c *Client) issuer(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+"/issuer/v1/people"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("the people's issuer at %s is unreachable: %v", c.Base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("the people's issuer: %s", e.Error)
	}
	return json.Unmarshal(data, out)
}

// signAct signs the agent's act on subject: verb, with params exactly the
// request's body, for the person whose grant it acts under.
func (id *agentID) signAct(subject provenance.Subject, verb string, params []byte) ([]byte, error) {
	id.mu.Lock()
	cert, key := id.cert, id.key
	id.mu.Unlock()
	if cert == nil {
		return nil, errors.New("the agent has no certificate")
	}
	return provenance.SignAgentAct(provenance.AgentAct{Farm: cert.Farm, Subject: subject, Act: verb, Params: params}, key,
		provenance.AgentCert{Identity: cert.Identity, Agent: cert.Agent, Grant: cert.Grant, Person: cert.Person, Chain: cert.Chain})
}

// signedActs are the job controls the agent signs, by the path's last
// element, and the verb each is: the action the HUD's control record names.
var signedActs = map[string]string{"decide": "decide", "requeue": "requeue", "steer": "steer", "supply": "supply",
	"close": "close", "plan": "plan", "accept-validated": "accept-validated", "withdraw": "withdraw", "units": "append"}

// signControl signs a job control as the agent, when it has a
// certificate: the act names the job by UID and the spec digest the HUD
// serves now, and its params are body exactly. A decision says whose wait
// it decides (waitsFor), and is signed only for a wait that names this
// agent: a person's wait is theirs to decide. It returns nil when the
// control goes unsigned.
func (c *Client) signControl(ctx context.Context, path string, body map[string]any) ([]byte, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	parts := strings.Split(path, "/")
	verb, ok := signedActs[parts[len(parts)-1]]
	if !ok || len(parts) != 5 || parts[1] != "jobs" {
		return raw, nil, nil
	}
	id, _ := c.agentCert(ctx, false)
	if id == nil {
		return raw, nil, nil
	}
	var j struct {
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		UID        string `json:"uid"`
		SpecDigest string `json:"specDigest"`
		Waits      []struct {
			Unit     int    `json:"unit"`
			Name     string `json:"name"`
			WaitsFor string `json:"waitsFor"`
			Do       string `json:"do"`
		} `json:"waits"`
	}
	if err := c.do(ctx, "GET", strings.Join(parts[:4], "/"), nil, &j); err != nil {
		return nil, nil, err
	}
	if j.UID == "" || j.SpecDigest == "" {
		return raw, nil, nil // a HUD that doesn't serve what an act names
	}
	if verb == "decide" {
		unit := fmt.Sprint(body["unit"])
		server := ""
		for _, w := range j.Waits {
			if w.Do == "" && (w.Name == unit || fmt.Sprint(w.Unit) == unit) {
				server = w.WaitsFor
			}
		}
		if server != "agent:"+c.Agent {
			return raw, nil, nil
		}
		body["waitsFor"] = server
		if raw, err = json.Marshal(body); err != nil {
			return nil, nil, err
		}
	}
	act, err := id.signAct(provenance.ActSubject("workjob", j.Namespace, j.Name, j.UID, j.SpecDigest), verb, raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, act, nil
}
