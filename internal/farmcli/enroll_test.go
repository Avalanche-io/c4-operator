package farmcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A code is taken as a person types it off the runner's screen.
func TestEnrollCode(t *testing.T) {
	for in, want := range map[string]string{
		"ABCD-EFGH": "ABCD-EFGH", "abcd-efgh": "ABCD-EFGH", "abcdefgh": "ABCD-EFGH", " ABCD EFGH ": "ABCD-EFGH",
	} {
		if got, err := EnrollCode(in); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ABCD-EFG", "ABCD-EFGHJ"} {
		if got, err := EnrollCode(in); err == nil {
			t.Errorf("%q: %q, not an error", in, got)
		}
	}
}

// An acceptance is the code, as the caller, to the node's enroll; for
// names whom the holder adds; the farm's refusal is said as it is, with
// the fix it carries.
func TestEnroll(t *testing.T) {
	var path, as string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, as = r.URL.Path, r.URL.Query().Get("as")
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		if as != "noor" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "c4-host-studio's sign-in is noor's; only noor can let others' jobs run on it: noor enrolls you with c4 farm enroll c4-host-studio --for sam"})
			return
		}
		json.NewEncoder(w).Encode(Enrollment{Node: "c4-host-studio", Owners: []string{"noor", "sam"}, Message: "c4-host-studio enrolled for sam by its holder noor"})
	}))
	defer srv.Close()
	ctx := context.Background()

	c, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	e, err := Enroll(ctx, c, "c4-host-studio", "abcdefgh", "sam")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/nodes/c4-host-studio/enroll" || body["code"] != "ABCD-EFGH" || body["for"] != "sam" {
		t.Errorf("sent %s %v", path, body)
	}
	if strings.Join(e.Owners, ",") != "noor,sam" {
		t.Errorf("owners %v", e.Owners)
	}
	if _, err := Enroll(ctx, c, "c4-host-studio", "ABCD-EFGH", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["for"]; ok {
		t.Errorf("for sent with no one named: %v", body)
	}

	c, _ = NewClient(Farm{HUD: srv.URL, As: "sam"})
	if _, err := Enroll(ctx, c, "c4-host-studio", "ABCD-EFGH", ""); err == nil || !strings.Contains(err.Error(), "c4 farm enroll c4-host-studio --for sam") {
		t.Errorf("not the holder: %v", err)
	}
}
