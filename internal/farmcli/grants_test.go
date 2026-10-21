package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A grant's end is said as every time is, local with UTC beside it, not
// in the farm's UTC alone.
func TestGrantSaysItsEndLocally(t *testing.T) {
	until := time.Date(2026, 11, 20, 14, 42, 58, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.GrantResponse{Message: "sandbox may submit your work until " + until.Format(time.RFC3339),
			Grant: api.Grant{Namespace: "saltmarsh", Name: "sandbox", Until: until}})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	la, _ := time.LoadLocation("America/Los_Angeles")
	o := Out{W: &out, Clock: Clock{Loc: la, Now: func() time.Time { return until.Add(-24 * time.Hour) }}}
	if err := Grant(context.Background(), c, o, "saltmarsh", api.GrantRequest{Tracker: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "2026-11-20T14:42:58Z") || !strings.Contains(s, "14:42 UTC") {
		t.Errorf("grant:\n%s", s)
	}
}
