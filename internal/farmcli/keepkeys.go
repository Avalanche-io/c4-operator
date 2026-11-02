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
	// a one-person farm whose owner has no touch key: its window, or that
	// it has closed (PEOPLE-KEYS-2 3.3)
	if until, err := time.Parse(time.RFC3339, cfg.TouchKeyUntil); err == nil && touchCertless(a.Store) {
		if at, err := time.Parse(time.RFC3339, cfg.TouchKeyReopened); err == nil {
			say("touch key the window to make your touch key was reopened at %s, with the installer token\n", at.Local().Format("Jan 2 15:04"))
		}
		if left := until.Sub(now); left > 0 {
			say("touch key you have no touch key: run c4 farm touch-key at a terminal within %s\n", Span(left))
		} else {
			say("touch key you have no touch key, and its window closed at %s: the acts that need a touch can't be confirmed on this farm; c4 farm touch-key --reopen, at a terminal, opens a fresh one\n", until.Local().Format("Jan 2 15:04"))
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

// touchCertless reports whether this machine has no certified touch key.
func touchCertless(st keystore.Store) bool {
	tc, err := LoadCert(st, keystore.Touch)
	return err == nil && tc == nil
}
