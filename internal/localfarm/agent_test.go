package localfarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A runner just started has no HUD token for a moment, so its offer is
// asked again while it makes one; any other refusal is said at once.
func TestOfferMachine(t *testing.T) {
	defer func(w time.Duration) { OfferWait = w }(OfferWait)
	OfferWait = 10 * time.Second
	dir := t.TempDir()
	tries := filepath.Join(dir, "tries")
	bin := filepath.Join(dir, "c4-agent")
	script := `#!/bin/sh
echo x >> "` + tries + `"
case "$(wc -l < "` + tries + `" | tr -d ' ')" in
1) echo "c4-agent: enroll-offer: no HUD token yet: start the runner once, which makes it" >&2; exit 1 ;;
esac
echo '{"node":"c4-host-studio","code":"ABCD-EFGH","expires":"2026-10-24T21:00:00Z","message":"give this code"}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := OfferMachine(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if o.Node != "c4-host-studio" || o.Code != "ABCD-EFGH" {
		t.Errorf("offer %+v", o)
	}
	if b, _ := os.ReadFile(tries); strings.Count(string(b), "x") != 2 {
		t.Errorf("asked %d times, not 2", strings.Count(string(b), "x"))
	}

	no := `#!/bin/sh
echo "c4-agent: enroll-offer: this machine is not enrolled with a HUD (c4-agent enroll --hud URL)" >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(no), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := OfferMachine(context.Background(), bin); err == nil || err.Error() != "this machine is not enrolled with a HUD (c4-agent enroll --hud URL)" {
		t.Errorf("not enrolled: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a refusal other than the token's was asked again")
	}
}
