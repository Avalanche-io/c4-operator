package server

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4-operator/internal/hud"
)

// The HUD the manager embeds reads the farm book as the route serves it
// (web/design/components/farmbook.js, the Book tab): the browser's own code,
// run in Node on a real page of GET /book/farm, reads the farm-level acts
// the HUD booked (the binding, a node taken out and put back) as sealed
// entries in their words, the one written after the seal as pending, and
// the segment's seal time. A HUD whose reading drifts from the page's
// shape fails here.
func TestEmbeddedHUDReadsFarmBook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the browser's book code runs in Node")
	}
	dir := t.TempDir()
	for _, f := range []string{"farmbook.js", "fmt.js"} {
		js, err := fs.ReadFile(hud.Web, "web/design/components/"+f)
		if err != nil {
			t.Fatalf("the embedded HUD has no %s: %v", f, err)
		}
		os.WriteFile(filepath.Join(dir, f), js, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "read.mjs"), []byte(readBookScript), 0o644)

	b := newBookHUD(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "render-07"}})
	s := b.s
	s.SinglePerson = "noor"
	if code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "laptop-1/*"}`, nil); code != http.StatusOK {
		t.Fatalf("bind: %d %v", code, out)
	}
	installer := map[string]string{InstallerHeader: testInstallerToken}
	toggle := func(disabled bool) {
		t.Helper()
		if w := send(s, http.MethodPost, "/api/v1/nodes/render-07/disable?as=wrangler", `{"disabled": `+strconv.FormatBool(disabled)+`}`, installer); w.Code != http.StatusOK {
			t.Fatalf("disable %v: %d %s", disabled, w.Code, w.Body)
		}
	}
	toggle(true)
	toggle(false)
	b.seal(t)
	toggle(true) // written after the seal: pending

	w := send(s, http.MethodGet, "/api/v1/book/farm?since=0&as=wrangler", "", installer)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /book/farm: %d %s", w.Code, w.Body)
	}
	cmd := exec.Command(node, filepath.Join(dir, "read.mjs"))
	cmd.Stdin = bytes.NewReader(w.Body.Bytes())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the browser's book code: %v\n%s", err, stderr.String())
	}
	var r struct {
		Head struct{ Position int64 }
		Rows []struct {
			Seq             int64
			Message, Signer string
			Open            bool
			SealedAt        string
		}
		Refused []any
		Sealed  string
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("the browser's book code wrote %q: %v", out, err)
	}
	want := []string{"the farm bound to noor", "took node render-07 out", "put node render-07 back", "took node render-07 out (not sealed yet)"}
	if len(r.Rows) != len(want) || r.Head.Position != 3 || r.Sealed == "" || len(r.Refused) != 0 {
		t.Fatalf("the browser read head %d, sealed %q, refused %v, rows %+v", r.Head.Position, r.Sealed, r.Refused, r.Rows)
	}
	for k, row := range r.Rows {
		pending := k == 3
		if row.Message != want[k] || row.Seq != int64(k+1) || row.Open != pending || row.Signer != "hud" || (row.SealedAt == "") != pending {
			t.Errorf("row %d reads %+v; want %q, position %d, pending %v", k, row, want[k], k+1, pending)
		}
	}
}

// readBookScript reads a page of GET /book/farm as the Book tab does.
const readBookScript = `
import { readFileSync } from 'node:fs';
import { readPage } from './farmbook.js';
import { setZone } from './fmt.js';
setZone('UTC');
const r = readPage(JSON.parse(readFileSync(0, 'utf8')));
process.stdout.write(JSON.stringify(r));
`
