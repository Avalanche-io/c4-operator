package farmcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a bundle as the farm writes one: README.txt first, then the entries.
func testBundle(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, e := range []struct{ name, body string }{
		{"README.txt", "This bundle holds the farm's scheduling decisions, redacted.\nIt never holds names, paths or contents.\nKey epoch 1. Faithful: yes."},
		{"manifest.c4m", "c4…"},
		{"farm.json", `{"pools":[],"projects":[],"sites":[]}`},
		{"events.jsonl", "{\"seq\":1,\"kind\":\"admit\"}\n{\"seq\":2,\"kind\":\"start\"}\n"},
	} {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// diagnose saves the bundle whole, under the farm's own file name, and says
// which it is; --show prints the README and each entry's fields and writes
// nothing; a refusal is the farm's words, with nothing written.
func TestDiagnose(t *testing.T) {
	bundle := testBundle(t)
	var gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/diagnose") {
			http.NotFound(w, r)
			return
		}
		gotWindow = r.URL.Query().Get("window")
		if r.URL.Query().Get("as") == "artist" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"a diagnostic bundle takes the farm admin role"}`))
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="c4-diagnose-c41KtFp4oUC4.tgz"`)
		w.Header().Set("X-C4-Bundle", "c41KtFp4oUC4nu66hg")
		w.Header().Set("X-C4-Bundle-Tier", "present")
		w.Write(bundle)
	}))
	defer srv.Close()
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	admin, _ := NewClient(Farm{HUD: srv.URL, As: "admin"})
	var out bytes.Buffer
	if err := Diagnose(context.Background(), admin, Out{W: &out}, DiagnoseArgs{Window: "30m"}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "c4-diagnose-c41KtFp4oUC4.tgz"))
	if err != nil || !bytes.Equal(saved, bundle) || gotWindow != "30m" ||
		!strings.Contains(out.String(), "bundle    c4-diagnose-c41KtFp4oUC4.tgz · the present moment only") {
		t.Fatalf("saved %d bytes, window %q, err %v\n%s", len(saved), gotWindow, err, out.String())
	}
	if err := Diagnose(context.Background(), admin, Out{W: &out}, DiagnoseArgs{}); err == nil || !strings.Contains(err.Error(), "is there already") {
		t.Errorf("a second save overwrote the first: %v", err)
	}

	out.Reset()
	if err := Diagnose(context.Background(), admin, Out{W: &out}, DiagnoseArgs{Show: true, Out: filepath.Join(dir, "shown.tgz")}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "(nothing written)") || !strings.Contains(s, "It never holds names, paths or contents.") ||
		!strings.Contains(s, "farm.json") || !strings.Contains(s, "fields: pools, projects, sites") || !strings.Contains(s, "fields: kind, seq") {
		t.Errorf("--show:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "shown.tgz")); !os.IsNotExist(err) {
		t.Error("--show wrote the bundle")
	}

	artist, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	if err := Diagnose(context.Background(), artist, Out{W: &out}, DiagnoseArgs{Out: filepath.Join(dir, "refused.tgz")}); err == nil ||
		!strings.Contains(err.Error(), "a diagnostic bundle takes the farm admin role") {
		t.Errorf("refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "refused.tgz")); !os.IsNotExist(err) {
		t.Error("a refusal wrote a file")
	}
	if err := Diagnose(context.Background(), admin, Out{W: &out}, DiagnoseArgs{Window: "an hour"}); err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("bad window: %v", err)
	}
}
