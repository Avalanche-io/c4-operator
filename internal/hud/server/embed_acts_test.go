package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4-operator/internal/hud"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// The HUD the manager embeds signs a person's act in the browser
// (web/js/act.js, PEOPLE-KEYS-2 K3), and the farm takes it: the browser's
// own WebCrypto code, run in Node, signs a decision with a scratch
// person's P-256 device key, and the decide control verifies it, takes it
// once and cites it, graded signed. A HUD whose act.js drifts from the
// farm's act format (the statement, the DSSE encoding, the bundle, the
// signature's one form) fails here, not in a person's browser.
func TestEmbeddedHUDSignsActs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the browser's signing code runs in Node")
	}
	dir := t.TempDir()
	js, err := fs.ReadFile(hud.Web, "web/js/act.js")
	if err != nil {
		t.Fatalf("the embedded HUD has no act.js: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "act.js"), js, 0o644)
	os.WriteFile(filepath.Join(dir, "sign.mjs"), []byte(signScript), 0o644)

	s, decide := decideFarm(t)
	now := time.Now()
	f := provtest.NewFarm(t, now)
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}}, now.Add(time.Hour)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	dana := f.Person(t, "dana", false)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(dana.P256)
	if err != nil {
		t.Fatal(err)
	}
	var chain []string
	for _, c := range dana.Chain {
		chain = append(chain, base64.StdEncoding.EncodeToString(c.Raw))
	}
	// what the browser knows: its key and chain from the issuer, the farm,
	// and the job's uid and specDigest from the snapshot
	in, _ := json.Marshal(map[string]any{
		"pkcs8": base64.StdEncoding.EncodeToString(pkcs8), "chain": chain, "farm": f.ID,
		"user": "dana", "identity": dana.Identity, "device": dana.Device,
		"job":  map[string]string{"namespace": "show-a", "name": "sh040-light-v007", "uid": provtest.JobUID, "specDigest": queue.SpecDigest(&rec.job)},
		"path": "/jobs/show-a/sh040-light-v007/decide", "body": map[string]string{"unit": "ruling", "do": "approve"},
	})
	cmd := exec.Command(node, filepath.Join(dir, "sign.mjs"))
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the browser's signing code: %v\n%s", err, stderr.String())
	}
	header := strings.TrimSpace(string(out))
	if header == "" {
		t.Fatal("the browser signed nothing for a decide on a job it can name")
	}

	code, p := decide(decideRequest("wrangler", map[string]string{ActHeader: header}))
	if code != http.StatusOK {
		t.Fatalf("the farm refused the browser's signed act: %d", code)
	}
	b, _ := base64.StdEncoding.DecodeString(header)
	var bundle provenance.Bundle
	json.Unmarshal(b, &bundle)
	payload, _ := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if p.Act == nil || p.Act.Payload != provenance.PayloadHash(payload) || p.Custody != CustodySigned {
		t.Fatalf("the control record cites %+v, custody %q", p.Act, p.Custody)
	}
	// taken once: the same bundle again is refused
	if code, _ := decide(decideRequest("wrangler", map[string]string{ActHeader: header})); code != http.StatusForbidden {
		t.Errorf("the browser's act again: %d, want 403", code)
	}
}

// signScript runs the embedded act.js as the HUD page does: it learns the
// job from a snapshot, uses the issuer's key and chain, and asks for the
// headers a write carries.
const signScript = `
import { readFileSync } from 'node:fs';
import { useSigner, learnJobs, actHeaders, ACT_HEADER } from './act.js';
const i = JSON.parse(readFileSync(0, 'utf8'));
const der = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
const key = await crypto.subtle.importKey('pkcs8', der(i.pkcs8), { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
useSigner({ key, chain: i.chain.map(der), farm: i.farm, user: i.user, identity: i.identity, device: i.device });
learnJobs([i.job]);
const h = await actHeaders(i.path, i.body);
process.stdout.write(h[ACT_HEADER] || '');
`
