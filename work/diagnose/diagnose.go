// Package diagnose reads a diagnostic bundle (DESIGN.md section 6.9) back
// into the scheduler's own types, for tools outside this module, such as
// the HUD's mock farm, that show or replay what a farm decided. Every name
// in it is synthetic: a kind letter and the name's rank among the
// snapshot's names of its kind (j00000003 is the fourth job in sort
// order), so names compare as they did on the farm. Names maps each one
// to the farm's pseudonym, which only the farm can map back.
package diagnose

import (
	"io"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Bundle is a diagnostic bundle read back.
type Bundle struct {
	// ID is the bundle's C4 ID (its manifest's).
	ID string
	// README is the bundle's README.txt.
	README   string
	KeyEpoch uint32
	// Snapshots are the ticks that changed a decision, oldest first.
	Snapshots []Snapshot
}

// Snapshot is one decision as the farm made it.
type Snapshot struct {
	// Policy names the policy that decided it: objective, edf, fifo, or
	// other for one this build does not know.
	Policy string
	// Faithful says replaying it gives the farm's decisions.
	Faithful bool
	// Input, State and Config are what the policy was given, with times
	// from a fixed epoch (Input.Now) and money as scaled.
	Input  sched.Snapshot
	State  sched.StateDoc
	Config sched.Config
	// Decided is the farm's decision, replayed: what the policy decides
	// on Input, State and Config. When Faithful, it is the farm's.
	Decided sched.Decisions
	// Names maps each synthetic name of a pool, project, job or link to
	// its pseudonym.
	Names map[string]string
}

// Read reads a bundle, checking every entry against its manifest.
func Read(r io.Reader) (Bundle, error) {
	c, err := bundle.Read(r)
	if err != nil {
		return Bundle{}, err
	}
	out := Bundle{ID: c.ID.String(), README: c.README, KeyEpoch: c.Redaction.KeyEpoch}
	for _, rec := range c.Snapshots {
		s, st, cfg := telemetry.Restore(rec)
		snap := Snapshot{Policy: telemetry.PolicyName(rec.Policy), Faithful: rec.Faithful, Input: s, State: st.Doc(), Config: cfg, Names: map[string]string{}}
		if policy := rec.Policy.Func(); policy != nil {
			snap.Decided = policy(s, sched.StateFrom(snap.State), cfg)
		}
		for _, p := range rec.Pools {
			snap.Names[p.Name.Synthetic()] = p.Name.Text()
		}
		for _, p := range rec.Projects {
			snap.Names[p.Name.Synthetic()] = p.Name.Text()
		}
		for _, j := range rec.Jobs {
			snap.Names[j.ID.Synthetic()] = j.ID.Text()
		}
		for _, l := range rec.Links {
			snap.Names[l.Name.Synthetic()] = l.Name.Text()
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	return out, nil
}
