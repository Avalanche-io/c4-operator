package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The telemetry controls ask the farm's endpoints and say its answers;
// resolving a pseudonym and raising the tier are refused inside an agent
// before anything is asked, and lowering it is not.
func TestTelemetry(t *testing.T) {
	var calls []string
	var body map[string]string
	status := api.Telemetry{Tier: "b", Epoch: 3, Record: "c4rec",
		Ring: api.TelemetryRing{Window: "2h0m0s", MaxBytes: 8 << 20, Bytes: 3 << 20, Snapshots: 340, Dropped: true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/v1/telemetry":
			json.NewEncoder(w).Encode(status)
		case "/api/v1/telemetry/resolve":
			json.NewEncoder(w).Encode(api.TelemetryName{Kind: "job", Name: "saltmarsh/sm0150-light-v004"})
		case "/api/v1/telemetry/tier":
			json.NewEncoder(w).Encode(api.TelemetryChange{Tier: body["tier"], Record: "c4tier", Message: "the farm's telemetry is now at tier " + body["tier"]})
		case "/api/v1/telemetry/rotate":
			json.NewEncoder(w).Encode(api.TelemetryChange{Epoch: 4, Record: "c4rot", Message: "the farm's telemetry key is now in epoch 4"})
		}
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	ctx := context.Background()
	var out bytes.Buffer
	o := Out{W: &out}

	if err := TelemetryStatus(ctx, c, o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tier      b (diagnostic bundles)", "kept      the last 2h, at most 8 MB: 3 MB held, 340 decisions, older ones let go", "key       epoch 3", "record    c4rec"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	status = api.Telemetry{Tier: "a", KillSwitch: true, Epoch: 1}
	TelemetryStatus(ctx, c, o)
	if s := out.String(); !strings.Contains(s, "tier      a (off)") || !strings.Contains(s, "C4_TELEMETRY=off") || strings.Contains(s, "kept") {
		t.Errorf("tier a, held off:\n%s", s)
	}

	out.Reset()
	if err := TelemetryResolve(ctx, c, o, "j.8f2a"); err != nil || body["pseudonym"] != "j.8f2a" || !strings.Contains(out.String(), "j.8f2a  job saltmarsh/sm0150-light-v004") {
		t.Errorf("resolve: %v, sent %v\n%s", err, body, out.String())
	}
	if err := TelemetryTier(ctx, c, o, "b"); err != nil || body["tier"] != "b" {
		t.Errorf("on: %v, sent %v", err, body)
	}
	if err := TelemetryRotate(ctx, c, o); err != nil || !strings.Contains(out.String(), "epoch 4") || !strings.Contains(out.String(), "record    c4rot") {
		t.Errorf("rotate: %v\n%s", err, out.String())
	}

	// inside an agent
	t.Setenv("CLAUDECODE", "1")
	calls = nil
	if err := TelemetryResolve(ctx, c, o, "j.8f2a"); err == nil || !strings.Contains(err.Error(), "in your own terminal, c4 farm telemetry resolve j.8f2a") {
		t.Errorf("resolve inside an agent: %v", err)
	}
	if err := TelemetryTier(ctx, c, o, "b"); err == nil || !strings.Contains(err.Error(), "in your own terminal, c4 farm telemetry on") {
		t.Errorf("on inside an agent: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("inside an agent, asked %v", calls)
	}
	if err := TelemetryTier(ctx, c, o, "a"); err != nil || body["tier"] != "a" {
		t.Errorf("off inside an agent: %v, sent %v", err, body)
	}
}
