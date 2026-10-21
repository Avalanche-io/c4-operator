package telemetry

import (
	"encoding/json"
)

// Enum is a named integer with a closed table of values. Index 0 of every
// table is "other": a value read from outside that is not in the table
// becomes other, never the raw text.
type Enum interface {
	Table() []string
	Index() int
}

func enumName(e Enum) string {
	t, i := e.Table(), e.Index()
	if i < 0 || i >= len(t) {
		return t[0]
	}
	return t[i]
}

func enumIndex(table []string, s string) int {
	for i, v := range table {
		if i > 0 && v == s {
			return i
		}
	}
	return 0
}

func enumJSON(e Enum) ([]byte, error) { return json.Marshal(enumName(e)) }

// Kind is what a pseudonym names, and its one-letter prefix.
type Kind uint16

// The pseudonym kinds (DESIGN.md section 6.9).
const (
	KindOther Kind = iota
	KindJob
	KindUnit
	KindProject
	KindPool
	KindNode
	KindSite
	KindPerson
	KindModel
	KindTracker
	KindLink
	KindC4ID
	KindGenerator
	KindHistory
	KindClass
)

var kindTable = []string{"other", "job", "unit", "project", "pool", "node", "site", "person", "model", "tracker", "link", "c4id", "generator", "history", "class"}

// kindPrefix is each kind's letter in a pseudonym's text form.
var kindPrefix = []string{"x", "j", "u", "p", "o", "n", "s", "a", "m", "t", "k", "i", "g", "h", "c"}

func (k Kind) Table() []string              { return kindTable }
func (k Kind) Index() int                   { return int(k) }
func (k Kind) MarshalJSON() ([]byte, error) { return enumJSON(k) }

func (k Kind) prefix() string {
	if int(k) < len(kindPrefix) {
		return kindPrefix[k]
	}
	return kindPrefix[0]
}

// Tier is why the policy released a unit.
type Tier uint16

var tierTable = []string{"other", "starvation", "deadline-need", "nominal-guarantee", "boost", "information", "surplus", "baseline"}

func (t Tier) Table() []string              { return tierTable }
func (t Tier) Index() int                   { return int(t) }
func (t Tier) MarshalJSON() ([]byte, error) { return enumJSON(t) }

// TierOf is s's tier, or other.
func TierOf(s string) Tier { return Tier(enumIndex(tierTable, s)) }

// TierName is t's name in the policy's own words.
func TierName(t Tier) string { return enumName(t) }

// Reason is why the policy stopped a unit.
type Reason uint16

var reasonTable = []string{"other", "reclaim-nominal", "restore-feasibility", "hold-stop", "wrangler-suspend", "guard-unit", "capacity-shrink"}

func (r Reason) Table() []string              { return reasonTable }
func (r Reason) Index() int                   { return int(r) }
func (r Reason) MarshalJSON() ([]byte, error) { return enumJSON(r) }

// ReasonOf is s's reason, or other.
func ReasonOf(s string) Reason { return Reason(enumIndex(reasonTable, s)) }

// ReasonName is r's name in the policy's own words.
func ReasonName(r Reason) string { return enumName(r) }

// Via is how a source gets a job's inputs to a pool.
type Via uint16

var viaTable = []string{"other", "move", "recompute", "reuse", "staging"}

func (v Via) Table() []string              { return viaTable }
func (v Via) Index() int                   { return int(v) }
func (v Via) MarshalJSON() ([]byte, error) { return enumJSON(v) }

// ViaOf is s's via, or other.
func ViaOf(s string) Via { return Via(enumIndex(viaTable, s)) }

// Channel is the kind of path a source moves over.
type Channel uint16

var channelTable = []string{"other", "same-zone", "inter-region", "internet", "interconnect", "shipment"}

func (c Channel) Table() []string              { return channelTable }
func (c Channel) Index() int                   { return int(c) }
func (c Channel) MarshalJSON() ([]byte, error) { return enumJSON(c) }

// ChannelOf is s's channel, or other.
func ChannelOf(s string) Channel { return Channel(enumIndex(channelTable, s)) }

// Planner is the policy's planner.
type Planner uint16

var plannerTable = []string{"other", "fluid-edf", "per-job"}

func (p Planner) Table() []string              { return plannerTable }
func (p Planner) Index() int                   { return int(p) }
func (p Planner) MarshalJSON() ([]byte, error) { return enumJSON(p) }

// PlannerOf is s's planner, or other.
func PlannerOf(s string) Planner { return Planner(enumIndex(plannerTable, s)) }

// Surplus is the policy's rule for its fair-share tier.
type Surplus uint16

var surplusTable = []string{"other", "least-slack", "fair-share"}

func (s Surplus) Table() []string              { return surplusTable }
func (s Surplus) Index() int                   { return int(s) }
func (s Surplus) MarshalJSON() ([]byte, error) { return enumJSON(s) }

// SurplusOf is s's rule, or other.
func SurplusOf(s string) Surplus { return Surplus(enumIndex(surplusTable, s)) }

// Divergence is a way a replayed decision differed from the farm's.
type Divergence uint16

var divergenceTable = []string{"other", "dispatch", "preempt", "capacity", "burst", "risk"}

func (d Divergence) Table() []string              { return divergenceTable }
func (d Divergence) Index() int                   { return int(d) }
func (d Divergence) MarshalJSON() ([]byte, error) { return enumJSON(d) }

// The divergence kinds.
const (
	DivergeOther Divergence = iota
	DivergeDispatch
	DivergePreempt
	DivergeCapacity
	DivergeBurst
	DivergeRisk
)

// GOOS and GOARCH are the platforms C4 builds for.
type GOOS uint16

var goosTable = []string{"other", "linux", "darwin", "windows"}

func (g GOOS) Table() []string              { return goosTable }
func (g GOOS) Index() int                   { return int(g) }
func (g GOOS) MarshalJSON() ([]byte, error) { return enumJSON(g) }

// GOARCH is a build's architecture.
type GOARCH uint16

var goarchTable = []string{"other", "amd64", "arm64"}

func (g GOARCH) Table() []string              { return goarchTable }
func (g GOARCH) Index() int                   { return int(g) }
func (g GOARCH) MarshalJSON() ([]byte, error) { return enumJSON(g) }

// GOAMD64 is an amd64 build's microarchitecture level.
type GOAMD64 uint16

var goamd64Table = []string{"other", "v1", "v2", "v3", "v4"}

func (g GOAMD64) Table() []string              { return goamd64Table }
func (g GOAMD64) Index() int                   { return int(g) }
func (g GOAMD64) MarshalJSON() ([]byte, error) { return enumJSON(g) }

// DivergenceName is d's name.
func DivergenceName(d Divergence) string { return enumName(d) }
