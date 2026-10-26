package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

const refresh = "sk-ant-ort01-never-leaves-the-machine"

func signIn(access string, expires time.Time) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference"],"subscriptionType":"max"}}`,
		access, refresh, expires.UnixMilli()))
}

// A lease carries the access token, its expiry and what it is for, never
// the refresh token; a document with no sign-in is refused without quoting
// it.
func TestShortLived(t *testing.T) {
	exp := time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
	out, got, err := ShortLived(signIn("sk-ant-oat01-short", exp))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(exp) || bytes.Contains(out, []byte(refresh)) || bytes.Contains(out, []byte("refreshToken")) {
		t.Fatalf("expiry %s, document %s", got, out)
	}
	var d struct {
		OAuth struct {
			AccessToken      string   `json:"accessToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(out, &d) != nil || d.OAuth.AccessToken != "sk-ant-oat01-short" || d.OAuth.ExpiresAt != exp.UnixMilli() || d.OAuth.SubscriptionType != "max" {
		t.Fatalf("document %s", out)
	}
	for _, bad := range []string{`{}`, `not json sk-ant-secret`, `{"claudeAiOauth":{"refreshToken":"sk-ant-secret"}}`} {
		if _, _, err := ShortLived([]byte(bad)); err == nil || strings.Contains(err.Error(), "sk-ant") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

// Claude Code's keychain item for a config dir other than ~/.claude is
// suffixed with its path's SHA-256.
func TestClaudeSource(t *testing.T) {
	t.Setenv("HOME", "/Users/holder")
	s, err := ClaudeSource("/Users/holder/.claude-b")
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := s.(KeychainSource); ok {
		// sha256("/Users/holder/.claude-b")[:8]
		if !strings.HasPrefix(k.Service, "Claude Code-credentials-") || len(k.Service) != len("Claude Code-credentials-")+8 {
			t.Fatalf("service %q", k.Service)
		}
		d, _ := ClaudeSource("")
		if d.(KeychainSource).Service != "Claude Code-credentials" {
			t.Fatalf("default service %q", d.(KeychainSource).Service)
		}
		return
	}
	if s.Where() != "/Users/holder/.claude-b/.credentials.json" {
		t.Fatalf("source %s", s.Where())
	}
}

type memSource struct {
	mu  sync.Mutex
	doc []byte
}

func (m *memSource) Read(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.doc...), nil
}
func (m *memSource) Where() string { return "memory" }
func (m *memSource) set(doc []byte) {
	m.mu.Lock()
	m.doc = doc
	m.mu.Unlock()
}

// The holder's agent issues the lease, renews it when the holder's sign-in
// is refreshed, and when it stops deletes the Secret and marks the lease
// revoked. Nothing it logs carries the token.
func TestAgentIssuesRenewsRevokes(t *testing.T) {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&c4v1.CredentialLease{}).Build()
	src := &memSource{doc: signIn("sk-ant-oat01-first", time.Now().Add(time.Hour))}
	var mu sync.Mutex
	var logs []string
	a := &Agent{Client: c, Namespace: "practice", Name: "noor-claude", Source: src, Retry: 50 * time.Millisecond,
		Spec: c4v1.CredentialLeaseSpec{Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"kubernetes-admin"}},
			Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{"blocks-kind"}},
			TTL: metav1.Duration{Duration: time.Hour}, RenewBefore: metav1.Duration{Duration: 59*time.Minute + 59*time.Second}},
		Logf: func(f string, args ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, args...)); mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	ctxb := context.Background()
	secret := func() string {
		var sec corev1.Secret
		if c.Get(ctxb, types.NamespacedName{Namespace: "practice", Name: "c4-lease-noor-claude"}, &sec) != nil {
			return ""
		}
		return string(sec.Data[c4v1.LeaseCredentialKey])
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	waitFor("the first token", func() bool { return strings.Contains(secret(), "sk-ant-oat01-first") })
	if strings.Contains(secret(), refresh) {
		t.Fatal("the refresh token left the machine")
	}
	// The Secret is written before the lease's status, so wait for both.
	var l c4v1.CredentialLease
	waitFor("the lease to be active", func() bool {
		return c.Get(ctxb, types.NamespacedName{Namespace: "practice", Name: "noor-claude"}, &l) == nil &&
			l.Status.State == c4v1.LeaseActive && l.Status.ExpiresAt != nil
	})

	// the holder's Claude Code refreshes: the agent copies the new token
	src.set(signIn("sk-ant-oat01-second", time.Now().Add(2*time.Hour)))
	waitFor("the renewed token", func() bool { return strings.Contains(secret(), "sk-ant-oat01-second") })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if secret() != "" {
		t.Fatal("the Secret outlived the agent")
	}
	c.Get(ctxb, types.NamespacedName{Namespace: "practice", Name: "noor-claude"}, &l)
	if l.Status.State != c4v1.LeaseRevoked {
		t.Fatalf("state %q after the agent stopped", l.Status.State)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "sk-ant") {
			t.Fatalf("a log line carries a token: %s", line)
		}
	}
	if len(logs) == 0 {
		t.Fatal("the agent said nothing")
	}
	var _ client.Client = c
}

// When the farm ends a lease (the controller revokes it: the farm's switch
// is off), the holder's agent does not issue it again: it stops, with the
// Secret deleted.
func TestAgentHonoursTheFarm(t *testing.T) {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&c4v1.CredentialLease{}).Build()
	src := &memSource{doc: signIn("sk-ant-oat01-first", time.Now().Add(time.Hour))}
	a := &Agent{Client: c, Namespace: "practice", Name: "noor-claude", Source: src, Retry: 50 * time.Millisecond,
		Spec: c4v1.CredentialLeaseSpec{Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"o"}},
			Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{"p"}},
			TTL: metav1.Duration{Duration: time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "practice", Name: "noor-claude"}
	var l c4v1.CredentialLease
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c.Get(ctx, key, &l) == nil && l.Status.State == c4v1.LeaseActive {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the lease never became active")
		}
	}
	// the controller revokes it
	orig := l.DeepCopy()
	l.Status.State, l.Status.Message = c4v1.LeaseRevoked, "this farm does not use holder-transfer leases"
	if err := c.Status().Patch(ctx, &l, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept a lease the farm revoked")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "practice", Name: "c4-lease-noor-claude"}, &corev1.Secret{}); err == nil {
		t.Fatal("the Secret outlived the farm's revocation")
	}
	c.Get(ctx, key, &l)
	if l.Status.State != c4v1.LeaseRevoked {
		t.Fatalf("state %q", l.Status.State)
	}
}

// The holder's agent stamps the lease with its sign-in's account label,
// on a new lease and on one it brings in line, keeping labels others set.
func TestAgentLabelsTheLeaseWithItsAccount(t *testing.T) {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	old := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: "practice", Name: "noor-old", Labels: map[string]string{"example.com/team": "comp"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&c4v1.CredentialLease{}).WithObjects(old).Build()
	ctx := context.Background()
	for _, name := range []string{"noor-new", "noor-old"} {
		a := &Agent{Client: c, Namespace: "practice", Name: name, Labels: map[string]string{c4v1.LabelAccount: "a1b2c3d4e5f6"},
			Spec: c4v1.CredentialLeaseSpec{Account: "anthropic", Holder: "noor"}}
		l, err := a.ensure(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got c4v1.CredentialLease
		if err := c.Get(ctx, types.NamespacedName{Namespace: "practice", Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Labels[c4v1.LabelAccount] != "a1b2c3d4e5f6" || l.Labels[c4v1.LabelAccount] != "a1b2c3d4e5f6" {
			t.Fatalf("%s: labels %v", name, got.Labels)
		}
		if name == "noor-old" && got.Labels["example.com/team"] != "comp" {
			t.Fatalf("an existing label was dropped: %v", got.Labels)
		}
	}
}
