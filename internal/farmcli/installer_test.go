package farmcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A local farm's client carries its installer token on every request; the
// browser is sent by a one-time code, the token never in the address; a
// HUD from before tokens, or a farm without one, opens the page as it is.
func TestInstallerToken(t *testing.T) {
	tok := strings.Repeat("ab", 32)
	file := filepath.Join(t.TempDir(), "installer-token")
	os.WriteFile(file, []byte(tok+"\n"), 0o600)
	var got []string
	codes := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-C4-Installer"))
		if r.URL.Path == "/api/v1/installer/code" && codes {
			json.NewEncoder(w).Encode(map[string]string{"code": "one-time"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ctx := context.Background()

	c, err := NewClient(Farm{HUD: srv.URL, As: "noor", Local: true, InstallerTokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	page := c.Web("needs", nil)
	u, err := SignedIn(ctx, c, page)
	if err != nil {
		t.Fatal(err)
	}
	if u != srv.URL+"/installer?code=one-time&next=%2F%3Fas%3Dnoor%23%2Fneeds" {
		t.Errorf("signed in by %s", u)
	}
	if strings.Contains(u, tok) || len(got) != 1 || got[0] != tok {
		t.Errorf("the token: address %s, header %v", u, got)
	}

	codes = false
	if u, err := SignedIn(ctx, c, page); err != nil || u != page {
		t.Errorf("a HUD without codes: %s, %v", u, err)
	}
	got = nil
	plain, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	if u, err := SignedIn(ctx, plain, page); err != nil || u != page || len(got) != 0 {
		t.Errorf("a farm without a token: %s, %v, asked %v", u, err, got)
	}

	// a token farm.yaml names that is gone says how to get it back
	if _, err := NewClient(Farm{HUD: srv.URL, InstallerTokenFile: filepath.Join(t.TempDir(), "gone")}); err == nil || !strings.Contains(err.Error(), "c4 farm up makes it again") {
		t.Errorf("a missing token: %v", err)
	}
}
