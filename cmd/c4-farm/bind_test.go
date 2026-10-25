package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/localfarm"
)

// up binds the farm to its owner as one person's farm, as the owner, once
// the HUD answers: the same owner again is no change, and a farm bound to
// someone else stops up with the HUD's word on how an owner changes.
// Each bind carries the farm's installer token, as every request to a
// local HUD does.
func TestUpBindsTheFarmToItsOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tok, err := localfarm.InstallerToken("c4-walk")
	if err != nil {
		t.Fatal(err)
	}
	var bound *api.FarmBindingRequest
	var as, tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/farm/binding" {
			http.NotFound(w, r)
			return
		}
		as = append(as, r.URL.Query().Get("as"))
		tokens = append(tokens, r.Header.Get("X-C4-Installer"))
		var req api.FarmBindingRequest
		json.NewDecoder(r.Body).Decode(&req)
		switch {
		case bound == nil:
			bound = &req
			json.NewEncoder(w).Encode(api.FarmBindingResponse{OK: true})
		case *bound == req:
			json.NewEncoder(w).Encode(api.FarmBindingResponse{OK: true, Existing: true})
		default:
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("this farm is bound to %s, its one person, since 2026-10-25T00:00:00Z (record c4abc): a new owner takes an owner change", bound.Owner)})
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	ctx := context.Background()

	o := localfarm.Options{Name: "c4-walk", Owner: "noor", HUDHostPort: port}
	if err := bindFarm(ctx, o); err != nil {
		t.Fatal(err)
	}
	if bound == nil || bound.Owner != "noor" || bound.To != "c4-walk/*" || !bound.OnePerson || bound.RunFor != "" {
		t.Fatalf("bound %+v", bound)
	}
	if err := bindFarm(ctx, o); err != nil {
		t.Errorf("the same owner again: %v", err)
	}
	o.Owner = "sam"
	err = bindFarm(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "not bound to its owner sam") || !strings.Contains(err.Error(), "bound to noor, its one person") {
		t.Errorf("another owner: %v", err)
	}
	if strings.Join(as, ",") != "noor,noor,sam" {
		t.Errorf("bound as %v, not the owner each time", as)
	}
	if strings.Join(tokens, ",") != tok+","+tok+","+tok {
		t.Errorf("installer tokens sent %v", tokens)
	}
}
