package farmcli

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/agent"
	"github.com/Avalanche-io/c4-operator/internal/power"
)

// PowerArgs are `c4 farm agent power`'s.
type PowerArgs struct {
	// SlotsFile is the host runner's --slots-file (default slots in
	// c4-agent's home, ~/.c4/agents/slots).
	SlotsFile string
	Policy    power.Policy
	Every     time.Duration
	// Once reads, writes and returns.
	Once bool
	// Read reads the battery (default power.Read).
	Read func(context.Context) (power.Reading, error)
}

// DefaultSlotsFile is where the power reader writes and the host runner
// reads, unless told otherwise.
func DefaultSlotsFile() (string, error) {
	home, err := agent.DefaultHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "slots"), nil
}

// AgentPower is `c4 farm agent power`: the battery, read every minute,
// written as the slots this machine's host runner offers (c4-agent runner
// --slots-file). It prints a line only when the slots change.
func AgentPower(ctx context.Context, o Out, a PowerArgs) error {
	if a.SlotsFile == "" {
		var err error
		if a.SlotsFile, err = DefaultSlotsFile(); err != nil {
			return err
		}
	}
	read := a.Read
	if read == nil {
		read = power.Read
	}
	if a.Every <= 0 {
		a.Every = time.Minute
	}
	var s power.State
	last := -1
	for {
		r, err := read(ctx)
		if errors.Is(err, power.ErrNoBattery) {
			// a desktop: always on power
			r, err = power.Reading{Percent: 100, Powered: true}, nil
		}
		if err != nil {
			return err
		}
		var n int
		var why string
		s, n, why = a.Policy.Step(s, r)
		if _, err := power.Write(a.SlotsFile, n, why); err != nil {
			return err
		}
		if n != last || a.Once {
			o.printf("%s  %d slots · %s → %s\n", o.Clock.At(o.Clock.now()), n, why, a.SlotsFile)
			last = n
		}
		if a.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.Every):
		}
	}
}
