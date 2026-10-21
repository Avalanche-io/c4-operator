package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Enrolling a machine is done once, with the farm's admin access (what
// `c4 farm up` has on a local farm): the machine gets its own account,
// c4-hosts/<node>, which may renew its own tokens and nothing more beyond
// what every host runner may do; a pool its owner owns, selecting its Node;
// and a kubeconfig that reads its token from a file. From then on its
// runner signs in as itself, and keeps its tokens fresh itself: the one
// for the farm's API, and those for the HUD and the certificate issuer.
const (
	// HostsNamespace holds each host runner's account.
	HostsNamespace = "c4-hosts"
	// LabelHost names a host runner's Node; its pool selects it.
	LabelHost = "c4.avalanche.io/host"
	// TokenLifetime is how long each token a runner holds lasts. The
	// runner renews each at half its life, so a machine that was off for
	// less than half of it signs in again on its own.
	TokenLifetime = 7 * 24 * time.Hour
)

// The tokens a runner holds, in its tokens folder, by file name: each
// one's audience ("" for the farm's API).
var runnerTokens = []struct{ file, audience string }{
	{"kube", ""},
	{"hud", c4v1.HUDAPIAudience},
	{"signer", c4v1.SignerAudience},
}

// TokenFile is the path of one of a runner's tokens in its tokens folder:
// "kube", "hud" or "signer".
func TokenFile(dir, name string) string { return filepath.Join(dir, name) }

// EnrollOptions says which machine to enroll, for whom, and where its
// kubeconfig and tokens go.
type EnrollOptions struct {
	Node  string
	Owner string
	// Pool is the pool to make for the machine, or to find already
	// selecting it.
	Pool  string
	Slots int
	// Dir is the machine's runner folder: its kubeconfig, and a tokens
	// folder.
	Dir string
	// Farm is the farm's API server as the admin's kubeconfig has it.
	Farm *clientcmdapi.Cluster
}

// Enrolled is what enrolling made or found.
type Enrolled struct {
	Node       string   `json:"node"`
	Owner      string   `json:"owner"`
	Pool       string   `json:"pool"`
	Kubeconfig string   `json:"kubeconfig"`
	Tokens     string   `json:"tokens"`
	Made       []string `json:"made"`
	// BlocksEnv is the config directory the machine's blocks run in, and
	// BlocksSignedIn whether its person has signed in there yet (c4-agent
	// enroll fills both; Enroll leaves them).
	BlocksEnv      string `json:"blocksEnv,omitempty"`
	BlocksSignedIn bool   `json:"blocksSignedIn"`
}

// Enroll enrolls a machine. Running it again changes nothing but the
// machine's first token.
func Enroll(ctx context.Context, c client.Client, o EnrollOptions) (Enrolled, error) {
	if o.Node == "" || o.Owner == "" || o.Pool == "" || o.Dir == "" || o.Farm == nil {
		return Enrolled{}, errors.New("enroll: a node, an owner, a pool, a folder and the farm are all needed")
	}
	if o.Slots <= 0 {
		o.Slots = 1
	}
	e := Enrolled{Node: o.Node, Owner: o.Owner, Pool: o.Pool, Kubeconfig: filepath.Join(o.Dir, "kubeconfig"), Tokens: filepath.Join(o.Dir, "tokens")}
	if err := c.Get(ctx, types.NamespacedName{Name: HostsNamespace}, &corev1.Namespace{}); err != nil {
		if apierrors.IsNotFound(err) {
			return e, fmt.Errorf("the farm has no %s namespace: its host runner access is not installed (config/rbac/host-runner.yaml)", HostsNamespace)
		}
		return e, err
	}
	made := func(what string, err error) error {
		if err == nil {
			e.Made = append(e.Made, what)
			return nil
		}
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: o.Node, Namespace: HostsNamespace}}
	if err := made("the account "+HostsNamespace+"/"+o.Node, c.Create(ctx, sa)); err != nil {
		return e, err
	}
	// its own tokens, and no one else's
	name := "token-" + o.Node
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: HostsNamespace},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{o.Node}, Verbs: []string{"create"}}}}
	if err := made("its leave to renew its own tokens", c.Create(ctx, role)); err != nil {
		return e, err
	}
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: HostsNamespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: o.Node, Namespace: HostsNamespace}}}
	if err := made("", c.Create(ctx, rb)); err != nil {
		return e, err
	}
	if err := ensurePool(ctx, c, o, &e); err != nil {
		return e, err
	}
	tok, err := mintToken(ctx, c, o.Node, "")
	if err != nil {
		return e, err
	}
	if err := os.MkdirAll(e.Tokens, 0o700); err != nil {
		return e, err
	}
	if err := writeAtomic(TokenFile(e.Tokens, "kube"), []byte(tok)); err != nil {
		return e, err
	}
	kc := clientcmdapi.NewConfig()
	farm := o.Farm.DeepCopy()
	farm.LocationOfOrigin = ""
	kc.Clusters["farm"] = farm
	kc.AuthInfos[o.Node] = &clientcmdapi.AuthInfo{TokenFile: TokenFile(e.Tokens, "kube")}
	kc.Contexts["farm"] = &clientcmdapi.Context{Cluster: "farm", AuthInfo: o.Node}
	kc.CurrentContext = "farm"
	if err := clientcmd.WriteToFile(*kc, e.Kubeconfig); err != nil {
		return e, err
	}
	if err := os.Chmod(e.Kubeconfig, 0o600); err != nil {
		return e, err
	}
	rec, _ := json.MarshalIndent(e, "", "  ")
	if err := writeAtomic(filepath.Join(o.Dir, enrolledFile), append(rec, '\n')); err != nil {
		return e, err
	}
	e.Made = slices.DeleteFunc(e.Made, func(s string) bool { return s == "" })
	return e, nil
}

// enrolledFile, in a runner folder, says which machine it is and whose.
const enrolledFile = "enrolled.json"

// RunnerDir is the runner folder in an agents home: where enrolling puts
// the machine's kubeconfig and tokens, and where its runner finds them.
func RunnerDir(home string) string { return filepath.Join(home, "runner") }

// ReadEnrolled reads a runner folder's enrollment; ok is false when the
// machine was never enrolled there.
func ReadEnrolled(dir string) (e Enrolled, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, enrolledFile))
	if errors.Is(err, os.ErrNotExist) {
		return Enrolled{}, false, nil
	}
	if err != nil {
		return Enrolled{}, false, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return Enrolled{}, false, fmt.Errorf("%s: %w", filepath.Join(dir, enrolledFile), err)
	}
	return e, true, nil
}

// ensurePool makes the machine's pool, or checks the one there: it must
// select this machine's Node and let its owner's work run there.
func ensurePool(ctx context.Context, c client.Client, o EnrollOptions, e *Enrolled) error {
	sel := map[string]string{LabelHost: o.Node}
	var p c4v1.Pool
	err := c.Get(ctx, types.NamespacedName{Name: o.Pool}, &p)
	if apierrors.IsNotFound(err) {
		p = c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: o.Pool}, Spec: c4v1.PoolSpec{
			NodeSelector: sel,
			Tolerations:  []corev1.Toleration{{Key: TaintHostRunner, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			SlotsPerNode: int32(o.Slots),
			Owners:       []string{o.Owner},
		}}
		if err := c.Create(ctx, &p); err != nil {
			return err
		}
		e.Made = append(e.Made, fmt.Sprintf("the pool %s, for %s's work on this machine", o.Pool, o.Owner))
		return nil
	}
	if err != nil {
		return err
	}
	if !maps.Equal(p.Spec.NodeSelector, sel) {
		return fmt.Errorf("the pool %s is there already and selects other machines; name another with --pool", o.Pool)
	}
	if len(p.Spec.Owners) > 0 && !slices.Contains(p.Spec.Owners, o.Owner) {
		return fmt.Errorf("the pool %s is there already and runs only the work of %s", o.Pool, strings.Join(p.Spec.Owners, ", "))
	}
	return nil
}

// mintToken asks the farm for a token of the machine's account, for an
// audience ("" for the farm's API), lasting TokenLifetime.
func mintToken(ctx context.Context, c client.Client, node, audience string) (string, error) {
	secs := int64(TokenLifetime / time.Second)
	tr := &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &secs}}
	if audience != "" {
		tr.Spec.Audiences = []string{audience}
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: node, Namespace: HostsNamespace}}
	if err := c.SubResource("token").Create(ctx, sa, tr); err != nil {
		return "", fmt.Errorf("a token for %s/%s: %w", HostsNamespace, node, err)
	}
	return tr.Status.Token, nil
}

// tokenExpiry reads when a token in a file expires, from its own claims
// (zero when there is none, or it cannot be read).
func tokenExpiry(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	parts := strings.Split(strings.TrimSpace(string(b)), ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// renewTokens keeps the runner's tokens fresh, when it has a tokens
// folder: each is renewed once less than half its life is left, with the
// farm token it holds. One that has run out cannot renew itself: the
// machine is enrolled again.
func (r *Runner) renewTokens(ctx context.Context) error {
	if r.Tokens == "" {
		return nil
	}
	now := r.now()
	if r.tokensDue.After(now) {
		return nil
	}
	next := now.Add(TokenLifetime / 2)
	for _, t := range runnerTokens {
		path := TokenFile(r.Tokens, t.file)
		exp := tokenExpiry(path)
		if t.file == "kube" && !exp.IsZero() && !exp.After(now) {
			return fmt.Errorf("this machine's token for the farm ran out at %s: enroll it again (c4 farm up)", exp.Local().Format(time.DateTime))
		}
		if !exp.IsZero() && exp.Sub(now) > TokenLifetime/2 {
			if due := exp.Add(-TokenLifetime / 2); due.Before(next) {
				next = due
			}
			continue
		}
		tok, err := mintToken(ctx, r.Client, r.Node, t.audience)
		if err != nil {
			r.tokensDue = now.Add(time.Minute)
			return err
		}
		if err := writeAtomic(path, []byte(tok)); err != nil {
			return err
		}
		r.logf("renewed its %s token", t.file)
	}
	r.tokensDue = next
	return nil
}
