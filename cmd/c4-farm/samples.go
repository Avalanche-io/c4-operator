package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/localfarm"
)

// loadScenario is `c4 farm samples load [SCENARIO]`: with no name, the
// list; with one, the scenario loaded onto the local farm.
func loadScenario(ctx context.Context, stdout io.Writer, cfg *farmcli.Config, utc bool, args []string) error {
	all, err := farmcli.Scenarios()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintf(stdout, "scenarios for the local farm (c4 farm samples load NAME):\n")
		for _, sc := range all {
			fmt.Fprintf(stdout, "  %-15s %s: %s\n", sc.Name, sc.Title, sc.Summary)
		}
		return nil
	}
	if len(args) > 1 {
		return errors.New("usage: c4 farm samples load [SCENARIO]")
	}
	sc, err := farmcli.LookupScenario(args[0])
	if err != nil {
		return err
	}
	f, ok := cfg.Farms[localfarm.Name]
	have, err := localfarm.Exists(localfarm.Name)
	if err != nil {
		return err
	}
	if !ok || !have {
		return errors.New("a scenario loads onto the local farm, and there is none; `c4 farm up` makes one")
	}
	o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: utc}}
	now := time.Now()
	o.Clock.Now = func() time.Time { return now }

	fmt.Fprintf(stdout, "loading   %s: %s\n", sc.Name, sc.Title)
	p, err := localfarm.Prepare(ctx, sc, all)
	if err != nil {
		return err
	}
	line := strings.Join(p.Projects, " · ")
	if len(p.Added) > 0 {
		line += fmt.Sprintf("   (%s added)", strings.Join(p.Added, " and "))
	}
	fmt.Fprintf(stdout, "projects  %s\n", line)
	for _, name := range p.Projects {
		if b, ok := sc.Budgets[name]; ok {
			fmt.Fprintf(stdout, "budget    %s may rent $%s of burst capacity today on its own\n", name, b)
		}
	}
	if p.Removed > 0 {
		fmt.Fprintf(stdout, "removed   %d jobs of an earlier scenario\n", p.Removed)
	}
	client := func(as string) (*farmcli.Client, error) {
		return farmcli.NewClient(farmcli.Farm{HUD: f.HUD, As: as, Local: true})
	}
	if err := farmcli.LoadScenario(ctx, o, sc, client); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "units     stand-ins: each takes the time the real work would and writes placeholder files\n")
	fmt.Fprintf(stdout, "clock     deadlines count from %s, when the scenario loaded\n", o.Clock.AtUTC(now))
	return nil
}
