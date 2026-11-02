package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/needs"
	"github.com/Avalanche-io/c4-operator/internal/node"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/unitact"
)

// A host unit signs what it reports about itself as itself, as a pod's
// unit does (PEOPLE-KEYS 5.1), with a key its runner holds: on a host, the
// runner is where the unit runs, as c4-unit is in a pod. For each unit pod
// it runs, the runner makes a key, has it certified at the HUD on a
// request it signs as itself (the leaf names the runner), renews it at
// half-life while the pod runs, and keeps it in memory and in the unit's
// directory (unit.key, the key and its chain, readable only by its own
// account) beside unit.json, what the unit's acts name: its pod, its job
// and the job's owner. c4-unit, started for the unit by its hook or by the
// runner, signs with that file (UnitEnv); the runner signs what it carries
// out for a block itself (its needs, a re-check's withdrawal). Both go to
// the unit's .c4/acts, which the runner relays. The key goes when the pod
// ends. A runner with no key of its own (Key), or a HUD that serves no
// unit certificates, leaves its units unsigned, as before.

// heldKey is a host unit's key and chain, and when to renew it.
type heldKey struct {
	key   ed25519.PrivateKey
	chain []*x509.Certificate
	renew time.Time
}

// unitSubject is what a host unit's acts name (unit.json): its pod, its
// job's UID and spec, and the job's owner.
type unitSubject struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Pod       string `json:"pod"`
	PodUID    string `json:"podUID"`
	JobUID    string `json:"jobUID"`
	Spec      string `json:"spec"`
	Owner     string `json:"owner"`
}

func unitKeyPath(u UnitDirs) string     { return filepath.Join(u.Root, "unit.key") }
func unitSubjectPath(u UnitDirs) string { return filepath.Join(u.Root, "unit.json") }

// UnitEnv is the environment c4-unit runs in for a host unit: the
// runner's own, the unit's directories, and, when its runner holds a key
// for it, the key and what its acts name.
func UnitEnv(u UnitDirs) []string {
	env := append(os.Environ(), "C4_OUTPUT="+u.Out, "C4_CONTROL="+u.Control, "C4_PODINFO="+u.PodInfo)
	var s unitSubject
	if b, err := os.ReadFile(unitSubjectPath(u)); err != nil || json.Unmarshal(b, &s) != nil {
		return env
	}
	if _, err := os.Stat(unitKeyPath(u)); err != nil {
		return env
	}
	return append(env, "C4_NAMESPACE="+s.Namespace, "C4_JOB="+s.Job, "C4_POD="+s.Pod, "C4_POD_UID="+s.PodUID, "C4_JOB_UID="+s.JobUID,
		"C4_SPEC="+s.Spec, "C4_OWNER="+s.Owner, "C4_UNIT_KEY="+unitKeyPath(u))
}

// unitOf is a host unit as unitact signs for it, in its UnitEnv.
func unitOf(u UnitDirs) unitact.Unit {
	env := map[string]string{}
	for _, kv := range UnitEnv(u) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return unitact.Unit{Getenv: func(k string) string { return env[k] }}
}

// holdUnitKey makes, or renews past half-life, unit pod p's key and writes
// it with what its acts name to the unit's directory. A key that can't be
// had is said once, and the unit goes unsigned until it can.
func (r *Runner) holdUnitKey(ctx context.Context, p *corev1.Pod, u UnitDirs) {
	if r.Key == nil || r.HUD == "" {
		return
	}
	if r.unitKeys == nil {
		r.unitKeys, r.unitKeyErr = map[types.UID]*heldKey{}, map[types.UID]string{}
	}
	if k := r.unitKeys[p.UID]; k != nil && r.now().Before(k.renew) {
		return
	}
	err := r.certifyUnit(ctx, p, u)
	switch {
	case err != nil && r.unitKeyErr[p.UID] != err.Error():
		r.unitKeyErr[p.UID] = err.Error()
		r.logf("%s/%s: the unit's own key: %v; what it reports goes unsigned until it has one", p.Namespace, p.Name, err)
	case err == nil:
		delete(r.unitKeyErr, p.UID)
	}
}

// certifyUnit makes a key for unit pod p and has the HUD certify it.
func (r *Runner) certifyUnit(ctx context.Context, p *corev1.Pod, u UnitDirs) error {
	j, why, err := r.jobOf(ctx, p)
	if err != nil || why != "" {
		return fmt.Errorf("its job: %v%s", err, why)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"publicKey": []byte(pub), "namespace": p.Namespace, "pod": p.Name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.HUD, "/")+"/workload/v1/unit-certificate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := SignHUDRequest(ctx, r.Key, req, body); err != nil {
		return fmt.Errorf("signing the request: %w", err)
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the farm: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Chain [][]byte `json:"chain"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Chain) == 0 {
		return fmt.Errorf("the farm's answer: %v", err)
	}
	var chain []*x509.Certificate
	for _, der := range out.Chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		chain = append(chain, c)
	}
	held, err := provenance.EncodeHeld(key, chain)
	if err != nil {
		return err
	}
	subject, _ := json.Marshal(unitSubject{Namespace: p.Namespace, Job: p.Labels[c4v1.LabelJob], Pod: p.Name, PodUID: string(p.UID), JobUID: j.UID,
		Spec: p.Annotations[c4v1.AnnotationUnitSpec], Owner: j.Owner})
	if err := writePrivateFile(unitSubjectPath(u), subject); err != nil {
		return err
	}
	if err := writePrivateFile(unitKeyPath(u), held); err != nil {
		return err
	}
	leaf := chain[0]
	r.unitKeys[p.UID] = &heldKey{key: key, chain: chain, renew: leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)}
	return nil
}

// dropUnitKey forgets unit pod p's key and removes its file: the pod ended.
func (r *Runner) dropUnitKey(p *corev1.Pod, u UnitDirs) {
	delete(r.unitKeys, p.UID)
	delete(r.unitKeyErr, p.UID)
	os.Remove(unitKeyPath(u))
}

// signHostReport signs a report the runner carries out for host unit p
// (its needs, a re-check's withdrawal) as the unit, and relays it to the
// pod before the report itself. One it can't sign goes unsigned, said.
func (r *Runner) signHostReport(ctx context.Context, p *corev1.Pod, u UnitDirs, act string, params map[string]any) {
	unit := unitOf(u)
	if !unit.Signs() {
		return
	}
	if _, err := unit.Sign(u.Out, act, params); err != nil {
		r.logf("%s/%s: this %s could not be signed (%v): the farm gets it unsigned, and from K8 refuses it", p.Namespace, p.Name, act, err)
		return
	}
	b, _, ok := node.ReadUnitActs(u.Out)
	if !ok {
		return
	}
	if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationUnitActs: b}); err != nil {
		r.logf("%s/%s: relaying its signed %s: %v", p.Namespace, p.Name, act, err)
		return
	}
	if st := r.relays[p.UID]; st != nil {
		st.acts = b
	}
}

// fitNeeds is a block's needs message cut to fit a termination message,
// and its fields as the unit signs them.
func fitNeeds(msg string) (string, map[string]any) {
	text, why, about := needs.Fit(needs.Parse(msg))
	return needs.Message(text, why, about), map[string]any{"what": text, "why": why, "about": about}
}

// writePrivateFile writes data to path whole, readable only by this
// account.
func writePrivateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
