package farmcli

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
)

// KeepArgs are KeepKeys'.
type KeepArgs struct {
	// Store is the farm's keys on this machine.
	Store keystore.Store
	// Open opens the person's browser, for a studio farm's renewal.
	Open func(string) error
	// Say tells the person.
	Say func(format string, a ...any)
	// Now is the time; zero is time.Now.
	Now time.Time
}

// KeepKeys is what each run of the CLI does for this machine's keys at a
// farm, when a person runs it at a terminal: it renews a key whose
// certificate ends within RenewWithin, and tells the person of each device
// enrolled as them since the last run, from the people's index
// (PEOPLE-KEYS-2 7). A machine with no key there does nothing; nothing it
// does stops the command, and what didn't work is said.
func KeepKeys(ctx context.Context, c *Client, a KeepArgs) {
	say := a.Say
	if say == nil {
		say = func(string, ...any) {}
	}
	mine, err := LoadCert(a.Store, keystore.Device)
	if err != nil || mine == nil {
		return
	}
	now := a.Now
	if now.IsZero() {
		now = time.Now()
	}
	// the reads before a command are brief; a renewal's sign-in in the
	// browser has its own wait
	renew := NeedsRenewal(mine, now)
	if !renew {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	cfg, err := PeopleConfig(ctx, c)
	if err != nil {
		return
	}
	if renew {
		la := LoginArgs{Store: a.Store, Farm: cfg, Open: a.Open, Say: a.Say}
		var err error
		if cfg.Provider != nil {
			say("key       this machine's key ends at %s: renewing it, through your browser\n", mine.Expires.Local().Format("15:04"))
			la.Provider = &http.Client{Timeout: 30 * time.Second}
			la.Endpoints, err = Discover(ctx, la.Provider, cfg.Provider.Issuer)
		}
		if err == nil {
			_, err = Renew(ctx, c, la)
		}
		if err != nil {
			say("key       this machine's key ends at %s and did not renew: %v\n", mine.Expires.Local().Format("Jan 2 15:04"), err)
		}
	}
	ns, err := NewDevices(ctx, c, a.Store, cfg)
	if errors.Is(err, ErrIndexRollback) {
		say("devices   %v: the farm may be hiding a device of yours; ask its admin, and trust no new device until it is explained\n", err)
		return
	}
	for _, n := range ns {
		say("devices   %s\n", n)
	}
}
