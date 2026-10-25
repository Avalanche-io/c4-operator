package farmcli

import (
	"context"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The farm's telemetry controls (DESIGN.md section 6.9), the farm admin's:
// its status, a pseudonym resolved on the farm, the tier set and the key
// rotated. Raising the tier and resolving a pseudonym are a person's,
// acting as themselves: the farm refuses them through MCP, and this CLI
// refuses them inside an agent, which the farm cannot tell from the person
// at a terminal.

// farmTiers is each of the farm's tiers in words, as status says it.
var farmTiers = map[string]string{
	"a": "a (off): a diagnostic bundle holds the present moment; the farm keeps no decisions",
	"b": "b (diagnostic bundles): the farm keeps its recent decisions, redacted, for a bundle; nothing leaves the farm",
}

// TelemetryStatus prints the farm's telemetry: its tier, the kill switch,
// the key's epoch, what the ring holds and the record of the tier's last
// change.
func TelemetryStatus(ctx context.Context, c *Client, o Out) error {
	var t api.Telemetry
	if err := c.Do(ctx, "GET", "/telemetry", nil, &t); err != nil {
		return err
	}
	tier := farmTiers[t.Tier]
	if tier == "" {
		tier = t.Tier
	}
	o.printf("tier      %s\n", tier)
	if t.KillSwitch {
		o.printf("held off  C4_TELEMETRY=off is set in the manager's environment, which holds every tier at off\n")
	}
	if t.Tier != "a" {
		window := t.Ring.Window
		if d, err := time.ParseDuration(window); err == nil {
			window = Span(d)
		}
		line := fmt.Sprintf("the last %s, at most %s: %s held, %s", window, Bytes(int64(t.Ring.MaxBytes)), Bytes(int64(t.Ring.Bytes)),
			plural(t.Ring.Snapshots, "decision", "decisions"))
		if t.Ring.Dropped {
			line += ", older ones let go to stay in bounds"
		}
		o.printf("kept      %s\n", line)
	}
	o.printf("key       epoch %d: a pseudonym in a bundle is resolved only under the key it was made with (c4 farm telemetry resolve P)\n", t.Epoch)
	if t.Record != "" {
		o.printf("record    %s, the tier's last change\n", t.Record)
	}
	return nil
}

// TelemetryResolve prints the name a pseudonym from a bundle stands for,
// among the names the farm holds now. Never inside an agent: the name is
// the person's to see.
func TelemetryResolve(ctx context.Context, c *Client, o Out, pseudonym string) error {
	if InsideAgent() {
		return fmt.Errorf("a pseudonym's name is yours to see, not an agent's: in your own terminal, c4 farm telemetry resolve %s", pseudonym)
	}
	var n api.TelemetryName
	if err := c.Do(ctx, "POST", "/telemetry/resolve", api.TelemetryResolve{Pseudonym: pseudonym}, &n); err != nil {
		return err
	}
	o.printf("%s  %s %s\n", pseudonym, n.Kind, n.Name)
	return nil
}

// TelemetryTier sets the farm's tier: "a" (off) or "b" (diagnostic
// bundles). Raising it is never from inside an agent; lowering it is from
// anywhere, since turning telemetry down can never leak.
func TelemetryTier(ctx context.Context, c *Client, o Out, tier string) error {
	if tier == "b" && InsideAgent() {
		return fmt.Errorf("raising the farm's telemetry is yours to do, not an agent's: in your own terminal, c4 farm telemetry on")
	}
	var r api.TelemetryChange
	if err := c.Do(ctx, "POST", "/telemetry/tier", api.TelemetryTier{Tier: tier}, &r); err != nil {
		return err
	}
	o.printf("%s\n", r.Message)
	if r.Record != "" {
		o.printf("record    %s\n", r.Record)
	}
	return nil
}

// TelemetryRotate gives the farm key a new epoch: pseudonyms from then on
// are unrelated to earlier ones, and the decisions kept go with the old
// key.
func TelemetryRotate(ctx context.Context, c *Client, o Out) error {
	var r api.TelemetryChange
	if err := c.Do(ctx, "POST", "/telemetry/rotate", struct{}{}, &r); err != nil {
		return err
	}
	o.printf("%s\n", r.Message)
	if r.Record != "" {
		o.printf("record    %s\n", r.Record)
	}
	return nil
}
