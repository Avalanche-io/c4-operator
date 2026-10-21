package provenance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

func farm(t *testing.T) (*CA, *httptest.Server) {
	t.Helper()
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{CA: ca, Identify: func(_ context.Context, tok string) (string, time.Duration, error) {
		switch tok {
		case "node-w2":
			return NodeIdentity("w2"), time.Hour, nil
		case "importer":
			return ServiceAccountIdentity("mono", "importer"), time.Hour, nil
		}
		return "", 0, errors.New("unknown token")
	}}
	srv := httptest.NewServer(iss)
	t.Cleanup(srv.Close)
	return ca, srv
}

func roots(t *testing.T, ca *CA) []byte {
	t.Helper()
	certPEM, _, err := ca.PEM()
	if err != nil {
		t.Fatal(err)
	}
	return certPEM
}

func taskFor(node string) TaskPredicate {
	return TaskPredicate{
		Builder:    Builder{ID: "https://cccc.io/c4-operator/node", Version: "test"},
		Invocation: Invocation{Project: "show-a", Namespace: "show-a", Pod: "p", Job: "sh010", Unit: "3", Attempt: "1"},
		Runtime:    Runtime{Node: node, StartedOn: time.Now().Add(-time.Minute), FinishedOn: time.Now()},
	}
}

func TestEachSignerSignsAsItself(t *testing.T) {
	ca, srv := farm(t)
	ctx := context.Background()
	node, err := NewSigner(ctx, RemoteIssuer(srv.URL, func() (string, error) { return "node-w2", nil }, nil))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := NewSigner(ctx, LocalIssuer(ca, ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := RootsFromPEM(roots(t, ca))
	if err != nil {
		t.Fatal(err)
	}
	frame := c4.Identify(strings.NewReader("frame"))
	rec, err := SignStatement(ctx, node, PredicateTask, []Subject{SubjectFor("renders/f.exr", frame)}, taskFor("w2"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifyBundle(rec, pool)
	if err != nil {
		t.Fatal(err)
	}
	if v.Identity != NodeIdentity("w2") || !v.HasSubject(frame.String()) || CheckSigner(v) != nil {
		t.Fatalf("verified %+v, policy %v", v, CheckSigner(v))
	}
	// A task record claiming another node fails the policy even though
	// its signature verifies.
	lie, _ := SignStatement(ctx, node, PredicateTask, nil, taskFor("w7"), time.Now())
	v, err = VerifyBundle(lie, pool)
	if err != nil || CheckSigner(v) == nil {
		t.Fatalf("a node signed a record for another node: %v %v", err, v)
	}
	// The queue signs its decisions; a node cannot.
	job, _ := SignStatement(ctx, queue, PredicateJob, nil, map[string]any{"job": "show-a/sh010"}, time.Now())
	v, err = VerifyBundle(job, pool)
	if err != nil || CheckSigner(v) != nil {
		t.Fatalf("queue record: %v %v", err, CheckSigner(v))
	}
	forged, _ := SignStatement(ctx, node, PredicateJob, nil, map[string]any{"job": "show-a/sh010"}, time.Now())
	v, _ = VerifyBundle(forged, pool)
	if CheckSigner(v) == nil {
		t.Fatal("a node signed a queue decision and the policy accepted it")
	}
}

func TestTamperedBundlesAndForeignRootsFail(t *testing.T) {
	ca, srv := farm(t)
	ctx := context.Background()
	node, err := NewSigner(ctx, RemoteIssuer(srv.URL, func() (string, error) { return "node-w2", nil }, nil))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := SignStatement(ctx, node, PredicateTask, nil, taskFor("w2"), time.Now())
	pool, _ := RootsFromPEM(roots(t, ca))

	var b Bundle
	json.Unmarshal(rec, &b)
	payload, _ := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	b.DSSEEnvelope.Payload = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(payload), `"unit":"3"`, `"unit":"4"`, 1)))
	tampered, _ := json.Marshal(b)
	if _, err := VerifyBundle(tampered, pool); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("tampered payload: %v", err)
	}
	other, _ := NewCA(time.Now())
	otherPool, _ := RootsFromPEM(roots(t, other))
	if _, err := VerifyBundle(rec, otherPool); !errors.Is(err, ErrChain) {
		t.Fatalf("a record verified against another farm's root: %v", err)
	}
	// Signed after the certificate expired: the chain check fails at the
	// claimed time.
	late, _ := SignStatement(ctx, node, PredicateTask, nil, taskFor("w2"), time.Now().Add(3*time.Hour))
	if _, err := VerifyBundle(late, pool); err == nil {
		t.Fatal("a record signed outside its certificate's validity verified")
	}
}

func TestIssuerRefusesUnknownTokensAndBadKeys(t *testing.T) {
	_, srv := farm(t)
	ctx := context.Background()
	if _, err := NewSigner(ctx, RemoteIssuer(srv.URL, func() (string, error) { return "stolen", nil }, nil)); err == nil {
		t.Fatal("an unknown token got a certificate")
	}
	issue := RemoteIssuer(srv.URL, func() (string, error) { return "importer", nil }, nil)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	chain, err := issue(ctx, pub)
	if err != nil || CertIdentity(chain[0]) != ServiceAccountIdentity("mono", "importer") {
		t.Fatalf("importer certificate: %v %v", err, chain)
	}
}

func TestLegacyRecordsStillVerifyButNameNoSigner(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	st := &Statement{Predicate: taskFor("w2")}
	env, _ := Sign(st, priv)
	v, err := VerifyRecord(env, nil, map[string]ed25519.PublicKey{KeyID(pub): pub})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSigner(v); err == nil || !strings.Contains(err.Error(), "farm key") {
		t.Fatalf("legacy record policy: %v", err)
	}
}

// A host runner signs the task records of the units it runs, as
// runner/<node>: the policy takes a task record from the node it says it
// ran on, or from that node's host runner, and from no other.
func TestHostRunnerSignsItsTasks(t *testing.T) {
	ca, _ := farm(t)
	ctx := context.Background()
	runner, err := NewSigner(ctx, LocalIssuer(ca, RunnerIdentity("laptop-1"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := RootsFromPEM(roots(t, ca))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := SignStatement(ctx, runner, PredicateTask, nil, taskFor("laptop-1"), time.Now())
	v, err := VerifyBundle(rec, pool)
	if err != nil || v.Identity != RunnerIdentity("laptop-1") || CheckSigner(v) != nil {
		t.Fatalf("the runner's own task: %v %+v, policy %v", err, v, CheckSigner(v))
	}
	lie, _ := SignStatement(ctx, runner, PredicateTask, nil, taskFor("w2"), time.Now())
	if v, err = VerifyBundle(lie, pool); err != nil || CheckSigner(v) == nil {
		t.Fatalf("a runner signed a task for another node: %v", err)
	}
	job, _ := SignStatement(ctx, runner, PredicateJob, nil, map[string]any{"job": "show-a/sh010"}, time.Now())
	if v, _ = VerifyBundle(job, pool); CheckSigner(v) == nil {
		t.Fatal("a runner signed a queue record and the policy accepted it")
	}
}
