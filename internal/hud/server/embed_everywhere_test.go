package server

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/hud"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The HUD the manager embeds signs a person's acts beyond job controls
// (web/js/act.js, shared piece S): the browser's own code, run in Node,
// learns the project from the snapshot, reads what needs a touch from
// GET /acts/needs, and signs a grant on the project as the HUD posts it,
// confirmed with the person's touch key; then, having read the grant
// list, its revocation on the grant, as a DELETE with no body. The grant
// endpoints take both and cite them: confirmed, then signed. A HUD whose
// subjects, verbs or params drift from S's fails here.
func TestEmbeddedHUDSignsEverywhere(t *testing.T) {
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
	os.WriteFile(filepath.Join(dir, "everywhere.mjs"), []byte(everywhereScript), 0o644)

	s := grantFarm(t)
	g := newActRig(t, s)
	maya := g.f.Person(t, "maya", true)
	on := projectActOn(t, s, "artist", "show-a")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/acts/needs?as=artist", nil))
	var needs any
	if err := json.Unmarshal(w.Body.Bytes(), &needs); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET /acts/needs: %d %s", w.Code, w.Body.String())
	}

	keyOf := func(k *provtest.Key) map[string]any {
		der, _ := x509.MarshalPKCS8PrivateKey(k.P256)
		var chain []string
		for _, c := range k.Chain {
			chain = append(chain, base64.StdEncoding.EncodeToString(c.Raw))
		}
		return map[string]any{"pkcs8": base64.StdEncoding.EncodeToString(der), "chain": chain, "identity": k.Identity, "device": k.Device}
	}
	sign := func(in map[string]any) string {
		t.Helper()
		in["key"], in["touch"], in["farm"], in["user"], in["needs"] = keyOf(maya), keyOf(maya.Touch), g.f.ID, "maya", needs
		in["snap"] = map[string]any{"projects": []any{map[string]any{"name": "show-a", "actOn": on}}}
		b, _ := json.Marshal(in)
		cmd := exec.Command(node, filepath.Join(dir, "everywhere.mjs"))
		cmd.Stdin = bytes.NewReader(b)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the browser's signing code: %v\n%s", err, stderr.String())
		}
		return strings.TrimSpace(string(out))
	}

	// the grant, as the HUD's form posts it: on the project, confirmed
	body := map[string]any{"namespace": "show-a", "tracker": "github", "classes": []string{}, "pools": []string{"cpu"}, "until": "7d"}
	header := sign(map[string]any{"method": "POST", "path": "/grants", "body": body})
	if header == "" {
		t.Fatal("the browser signed nothing for a grant on a project the snapshot names")
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/grants?as=artist", body, header, "show-a", nil)
	cites(t, "the browser's grant", code, out, p, on, CustodyConfirmed)

	// its revocation, once the grant list names the grant
	gr := grants(t, s, "?as=artist")[0]
	withUID(t, s, &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: gr.Name}}, "9a7e0000-0002")
	gr = grants(t, s, "?as=artist")[0]
	header = sign(map[string]any{"method": "DELETE", "path": "/grants/show-a/" + gr.Name, "read": map[string]any{"path": "/grants", "data": map[string]any{"grants": []any{gr}}}})
	if header == "" {
		t.Fatal("the browser signed nothing for revoking a grant the list names")
	}
	code, out, p = g.send(t, http.MethodDelete, "/api/v1/grants/show-a/"+gr.Name+"?as=artist", nil, header, "show-a", nil)
	cites(t, "the browser's revocation", code, out, p, gr.ActOn, CustodySigned)
}

// everywhereScript signs one write as the HUD page does: it learns the
// snapshot and a read, reads the needs list, and asks act.js for the
// headers, a touch key standing by as the CLI's would (a raw signature
// over the same bytes).
const everywhereScript = `
import { readFileSync } from 'node:fs';
import { useSigner, useToucher, learnSnapshot, learnRead, learnNeeds, actHeaders, ACT_HEADER, b64, hex } from './act.js';
const i = JSON.parse(readFileSync(0, 'utf8'));
const der = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
const key = async (k) => crypto.subtle.importKey('pkcs8', der(k.pkcs8), { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
useSigner({ key: await key(i.key), chain: i.key.chain.map(der), farm: i.farm, user: i.user, identity: i.key.identity, device: i.key.device });
const touchKey = await key(i.touch);
const touchChain = i.touch.chain.map(der);
const N = 0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551n;
const lowS = (sig) => { let s = 0n; for (const b of sig.slice(32)) s = (s << 8n) | BigInt(b); if (s <= N >> 1n) return sig; s = N - s; const o = new Uint8Array(sig); for (let k = 63; k >= 32; k--) { o[k] = Number(s & 0xffn); s >>= 8n; } return o; };
useToucher(() => async (signed) => ({
  keyid: hex(new Uint8Array(await crypto.subtle.digest('SHA-256', touchChain[0]))),
  sig: b64(lowS(new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, touchKey, signed)))),
  chain: touchChain,
}));
learnSnapshot(i.snap);
learnNeeds(i.needs);
if (i.read) learnRead(i.read.path, i.read.data);
const h = await actHeaders(i.path, i.body, { method: i.method });
process.stdout.write(h[ACT_HEADER] || '');
`
