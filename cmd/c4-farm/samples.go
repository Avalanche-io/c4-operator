package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/localfarm"
)

// loadScenario is `c4 farm samples load [SCENARIO]`: with no name, the
// list; with one, the scenario loaded onto the local farm named (or the
// current one).
func loadScenario(ctx context.Context, stdout io.Writer, cfg *farmcli.Config, utc bool, name string, args []string) error {
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
	clusters, err := localfarm.Clusters()
	if err != nil {
		return err
	}
	name, f, err := scenarioFarm(cfg, clusters, name)
	if err != nil {
		return err
	}
	localfarm.Name = name
	sc = localfarm.OnFarm(sc)
	o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: utc}}
	now := time.Now()
	o.Clock.Now = func() time.Time { return now }

	fmt.Fprintf(stdout, "loading   %s: %s, onto %s\n", sc.Name, sc.Title, name)
	if len(sc.Issues) > 0 {
		// issues for the Tracker, not jobs: the farm's other work stays
		inst, err := f.InstallerToken()
		if err != nil {
			return err
		}
		if err := farmcli.LoadIssues(ctx, o, sc, f.HUD, f.Owner, inst); err != nil {
			return err
		}
		// a command, not a link: the page is the HUD's, which takes a
		// browser c4 farm open has signed in
		fmt.Fprintf(stdout, "sandbox   the stand-in tracker: c4 farm open, then /sandbox/ in that window\n")
		return nil
	}
	if len(sc.Shots) > 0 {
		// Tasks in the stand-in Flow, each with its plate
		inst, err := f.InstallerToken()
		if err != nil {
			return err
		}
		client := func(as string) (*farmcli.Client, error) {
			return farmcli.NewClient(farmcli.Farm{HUD: f.HUD, As: as, Local: true, InstallerTokenFile: f.InstallerTokenFile})
		}
		if err := farmcli.LoadShots(ctx, o, sc, f.HUD, f.Owner, inst, localfarm.Project, client, localfarm.FlowPlate); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "sandbox   the stand-in Flow: c4 farm open, then /sandbox/flow/ in that window\n")
		return nil
	}
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
			if v, err := strconv.ParseFloat(b, 64); err == nil {
				b = fmt.Sprintf("%.2f", v)
			}
			fmt.Fprintf(stdout, "budget    %s may rent $%s of burst capacity today on its own\n", name, b)
		}
	}
	if p.Removed > 0 {
		fmt.Fprintf(stdout, "removed   %s of an earlier scenario\n", map[bool]string{true: "1 job", false: fmt.Sprintf("%d jobs", p.Removed)}[p.Removed == 1])
	}
	client := func(as string) (*farmcli.Client, error) {
		return farmcli.NewClient(farmcli.Farm{HUD: f.HUD, As: as, Local: true, InstallerTokenFile: f.InstallerTokenFile})
	}
	if err := farmcli.LoadScenario(ctx, o, sc, client); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "units     stand-ins: each takes the time the real work would and writes placeholder files\n")
	fmt.Fprintf(stdout, "clock     deadlines count from %s, when the scenario loaded\n", o.Clock.AtUTC(now))
	return nil
}

// scenarioFarm is the local farm a scenario loads onto (localFarm).
func scenarioFarm(cfg *farmcli.Config, clusters []string, name string) (string, farmcli.Farm, error) {
	return localFarm(cfg, clusters, name, "a scenario loads onto")
}

// errNoLocalFarm is localFarm's answer when no local farm runs here.
var errNoLocalFarm = errors.New("no local farm")

// localFarm is the local farm a command (does: "a scenario loads onto")
// means: the one name names, else the current farm when it is local, else
// the only local farm; with several and none current, it names them and
// asks for --name, never guessing one, as another person's farm on this
// machine may be one of them. A local farm is one up made that is still
// running (clusters).
func localFarm(cfg *farmcli.Config, clusters []string, name, does string) (string, farmcli.Farm, error) {
	var local []string
	for _, n := range cfg.Names() {
		if cfg.Farms[n].Local && slices.Contains(clusters, n) {
			local = append(local, n)
		}
	}
	switch {
	case len(local) == 0:
		return "", farmcli.Farm{}, fmt.Errorf("%s a local farm, and there is none; `c4 farm up` makes one: %w", does, errNoLocalFarm)
	case name != "":
		if !slices.Contains(local, name) {
			return "", farmcli.Farm{}, fmt.Errorf("there is no local farm %s; the local farms here: %s", name, strings.Join(local, ", "))
		}
	case slices.Contains(local, cfg.Current):
		name = cfg.Current
	case len(local) == 1:
		name = local[0]
	default:
		return "", farmcli.Farm{}, fmt.Errorf("%s a local farm, and the current farm is not one; name one with --name: %s", does, strings.Join(local, ", "))
	}
	return name, cfg.Farms[name], nil
}
