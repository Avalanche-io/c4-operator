package agent_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// applyHostRunnerAccess installs what every host runner may do, from the
// release's own manifest.
func applyHostRunnerAccess(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "host-runner.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(b), "\n---\n") {
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			t.Fatal(err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if err := k8s.Create(context.Background(), &u); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

func audiences(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(string(b)), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Aud []string `json:"aud"`
	}
	json.Unmarshal(raw, &c)
	return c.Aud
}

// TestEnrollAMachine: enrolling makes the machine's own account, a pool its
// owner owns selecting its Node, and a kubeconfig that reads its token
// from a file. Its runner signs in with that alone, registers its Node,
// mints its tokens for the HUD and the issuer, and renews its farm token
// once half its life has gone. Enrolling again makes nothing new; a pool
// of another owner's is refused.
func TestEnrollAMachine(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	applyHostRunnerAccess(t)
	node := "c4-host-enroll"
	home := t.TempDir()
	farm := &clientcmdapi.Cluster{Server: k8sCfg.Host, CertificateAuthorityData: k8sCfg.CAData}
	o := agent.EnrollOptions{Node: node, Owner: "sam", Pool: "agent-enroll", Slots: 2, Dir: agent.RunnerDir(home), Farm: farm}
	e, err := agent.Enroll(ctx, k8s, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
		k8s.Delete(ctx, &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "agent-enroll"}})
	})
	if len(e.Made) != 3 {
		t.Errorf("made %q", e.Made)
	}
	var pool c4v1.Pool
	if err := k8s.Get(ctx, types.NamespacedName{Name: "agent-enroll"}, &pool); err != nil {
		t.Fatal(err)
	}
	if pool.Spec.NodeSelector[agent.LabelHost] != node || !slices.Equal(pool.Spec.Owners, []string{"sam"}) || len(pool.Spec.Tolerations) != 1 {
		t.Errorf("pool %+v", pool.Spec)
	}
	if info, err := os.Stat(e.Kubeconfig); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig %v %v", info, err)
	}
	if got, ok, err := agent.ReadEnrolled(agent.RunnerDir(home)); err != nil || !ok || got.Owner != "sam" || got.Node != node {
		t.Errorf("enrolled %+v %v %v", got, ok, err)
	}

	// the runner, signed in as the machine and nothing else
	cfg, err := clientcmd.BuildConfigFromFlags("", e.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	hc, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	if err := hc.Get(ctx, types.NamespacedName{Name: "agent-enroll"}, &c4v1.Pool{}); !apierrors.IsForbidden(err) {
		t.Errorf("the machine's account reads pools: %v", err)
	}
	now := time.Now()
	h := agent.New(&agent.Store{Dir: home}, &fake{active: map[string]bool{}})
	r := &agent.Runner{Client: hc, Harness: h, Node: node, Labels: pool.Spec.NodeSelector, Owners: []string{"sam"}, Slots: 2,
		Tokens: e.Tokens, Now: func() time.Time { return now }, Logf: t.Logf}
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: node}, &corev1.Node{}); err != nil {
		t.Fatalf("no Node: %v", err)
	}
	if a := audiences(t, agent.TokenFile(e.Tokens, "hud")); !slices.Equal(a, []string{c4v1.HUDAPIAudience}) {
		t.Errorf("hud token for %v", a)
	}
	if a := audiences(t, agent.TokenFile(e.Tokens, "signer")); !slices.Equal(a, []string{c4v1.SignerAudience}) {
		t.Errorf("signer token for %v", a)
	}
	first, _ := os.ReadFile(agent.TokenFile(e.Tokens, "kube"))
	now = now.Add(agent.TokenLifetime/2 + time.Hour)
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(agent.TokenFile(e.Tokens, "kube")); string(again) == string(first) {
		t.Error("the farm token was not renewed past half its life")
	}

	if e, err := agent.Enroll(ctx, k8s, o); err != nil || len(e.Made) != 0 {
		t.Errorf("enrolling again: made %q, %v", e.Made, err)
	}
	o.Owner = "ivo"
	if _, err := agent.Enroll(ctx, k8s, o); err == nil || !strings.Contains(err.Error(), "runs only the work of sam") {
		t.Errorf("another owner's pool: %v", err)
	}
}
