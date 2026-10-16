// Package api is the HUD's wire contract (internal/hud/API.md) as Go
// types, shared by the real backend (internal/hud/server) and the mock
// (hack/hudmock), so the two cannot drift apart.
package api

import "time"

// Viewer is who the response is filtered for.
type Viewer struct {
	User     string   `json:"user"`
	Projects []string `json:"projects"`
	All      bool     `json:"allProjects"`
}

// Sees reports whether the viewer may see a project's jobs and objects.
func (v *Viewer) Sees(project string) bool {
	if v == nil {
		return false
	}
	for _, p := range v.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// BurstOffer is the smallest burst that restores a job's deadline.
type BurstOffer struct {
	Pool          string  `json:"pool"`
	Slots         int     `json:"slots"`
	Cost          float64 `json:"cost"`
	NeedsApproval bool    `json:"needsApproval"`
}

type Snapshot struct {
	Now      time.Time  `json:"now"`
	Viewer   *Viewer    `json:"viewer"`
	Farm     Farm       `json:"farm"`
	Pools    []Pool     `json:"pools"`
	Classes  []Class    `json:"classes"`
	Projects []Project  `json:"projects"`
	Jobs     []Job      `json:"jobs"`
	Nodes    []Node     `json:"nodes"`
	Tiers    Tiers      `json:"tiers"`
	Capacity []Capacity `json:"capacity"`
	Usage    Usage      `json:"usage"`
}

// Usage is metered use. Billing is by cluster-hours, container-hours or
// a production window, never by bytes; bytes avoided is the savings proof.
type Usage struct {
	ClusterHours      float64          `json:"clusterHours"`
	ContainerHours    []ContainerHours `json:"containerHours"`
	GPUHours          []GPUHours       `json:"gpuHours"`
	ProductionWindows []Window         `json:"productionWindows"`
}

type ContainerHours struct {
	Project string  `json:"project"`
	Pool    string  `json:"pool"`
	Hours   float64 `json:"hours"`
}

type GPUHours struct {
	Project string  `json:"project"`
	Hours   float64 `json:"hours"`
}

type Window struct {
	Project string    `json:"project"`
	Name    string    `json:"name"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Active  bool      `json:"active"`
}

type Farm struct {
	SlotsUsed          int     `json:"slotsUsed"`
	SlotsTotal         int     `json:"slotsTotal"`
	QueuedUnits        int     `json:"queuedUnits"`
	UnitsPerHour       int     `json:"unitsPerHour"`
	FailuresLastHour   int     `json:"failuresLastHour"`
	BurstSlots         int     `json:"burstSlots"`
	BurstCostPerHour   float64 `json:"burstCostPerHour"`
	BytesMaterialized  float64 `json:"bytesMaterialized"`
	BytesFetchedStore  float64 `json:"bytesFetchedFromStore"`
	BytesAvoided       float64 `json:"bytesAvoided"`
	OtherProjectsSlots int     `json:"otherProjectsSlots"`
}

type Pool struct {
	Name            string         `json:"name"`
	Attrs           map[string]any `json:"attrs"`
	Slots           int            `json:"slots"`
	Used            int            `json:"used"`
	CostPerSlotHour float64        `json:"costPerSlotHour"`
	Burst           *PoolBurst     `json:"burst,omitempty"`
}

type PoolBurst struct {
	MaxSlots            int     `json:"maxSlots"`
	ProvisioningSeconds float64 `json:"provisioningSeconds"`
	CostPerSlotHour     float64 `json:"costPerSlotHour"`
	Requested           int     `json:"requested"`
	Provisioned         int     `json:"provisioned"`
}

type Class struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver"`
	SLO    map[string]any `json:"slo"`
	Lens   string         `json:"lens"`
}

type Project struct {
	Name              string           `json:"name"`
	Quotas            []map[string]any `json:"quotas"`
	BurstSpendToday   float64          `json:"burstSpendToday"`
	BurstBudgetPerDay float64          `json:"burstBudgetPerDay"`
	BytesAvoided      float64          `json:"bytesAvoided"`
	WorkerHours       []map[string]any `json:"workerHours"`
	GPUHours          float64          `json:"gpuHours"`
}

type Counts struct {
	Total       int `json:"total"`
	Unevaluated int `json:"unevaluated"`
	Queued      int `json:"queued"`
	Blocked     int `json:"blocked"`
	Running     int `json:"running"`
	Retrying    int `json:"retrying"`
	Succeeded   int `json:"succeeded"`
	Failed      int `json:"failed"`
	Held        int `json:"held"`
}

type States struct {
	Unevaluated string `json:"unevaluated"`
	Queued      string `json:"queued"`
	Blocked     string `json:"blocked"`
	Running     string `json:"running"`
	Retrying    string `json:"retrying"`
	Succeeded   string `json:"succeeded"`
	Failed      string `json:"failed"`
}

type Objective struct {
	Type                 string     `json:"type"`
	Deadline             *time.Time `json:"deadline,omitempty"`
	LatencyTargetSeconds float64    `json:"latencyTargetSeconds,omitempty"`
	From                 string     `json:"from,omitempty"`
}

type Estimate struct {
	RemainingSeconds    float64     `json:"remainingSeconds"`
	P90RemainingSeconds float64     `json:"p90RemainingSeconds"`
	EtaP50              time.Time   `json:"etaP50"`
	EtaP90              time.Time   `json:"etaP90"`
	RequiredSlots       float64     `json:"requiredSlots"`
	AttainableSlots     float64     `json:"attainableSlots"`
	RunningSlots        int         `json:"runningSlots"`
	SlackSeconds        float64     `json:"slackSeconds"`
	Feasible            bool        `json:"feasible"`
	Message             string      `json:"message,omitempty"`
	Burst               *BurstOffer `json:"burst,omitempty"`
}

type Input struct {
	Materialization string `json:"materialization,omitempty"`
	View            string `json:"view,omitempty"`
	FromJob         string `json:"fromJob,omitempty"`
}

type Job struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Project      string    `json:"project"`
	Class        string    `json:"class"`
	Driver       string    `json:"driver"`
	Owner        string    `json:"owner,omitempty"`
	Phase        string    `json:"phase"`
	Order        string    `json:"order"`
	Units        Counts    `json:"units"`
	UnitStates   States    `json:"unitStates"`
	Objective    Objective `json:"objective"`
	Estimate     Estimate  `json:"estimate"`
	Rank         int       `json:"rank"`
	Pools        []string  `json:"pools"`
	Inputs       []Input   `json:"inputs"`
	Output       *Output   `json:"output,omitempty"`
	DependsOn    []string  `json:"dependsOn"`
	CreatedAt    time.Time `json:"createdAt"`
	Nudge        Nudge     `json:"nudge"`
	HeavyRegions []Heavy   `json:"heavyRegions,omitempty"`
	Blacklist    []string  `json:"blacklist,omitempty"`
}

type Output struct {
	View       string `json:"view"`
	Changesets int    `json:"changesets"`
}

type Nudge struct {
	Hold  bool    `json:"hold"`
	Boost float64 `json:"boost"`
}

type Heavy struct {
	From   int     `json:"from"`
	To     int     `json:"to"`
	Factor float64 `json:"factor"`
	Label  string  `json:"label,omitempty"`
}

type Node struct {
	Name             string    `json:"name"`
	Pool             string    `json:"pool"`
	Locality         string    `json:"locality"`
	State            string    `json:"state"`
	Slots            int       `json:"slots"`
	Used             int       `json:"used"`
	FailuresLastHour int       `json:"failuresLastHour"`
	Suspect          bool      `json:"suspect"`
	CacheBytes       float64   `json:"cacheBytes"`
	CacheHitRate     float64   `json:"cacheHitRate"`
	Running          []Running `json:"running,omitempty"`
}

type Running struct {
	Job   string `json:"job"`
	Unit  int    `json:"unit"`
	Slots int    `json:"slots"`
}

type Tiers struct {
	Tiers      []Tier         `json:"tiers"`
	FanIn      map[string]int `json:"fanIn"`
	PublishP50 float64        `json:"publishP50"`
	PublishP95 float64        `json:"publishP95"`
	CaptureP50 float64        `json:"captureP50"`
}

type Tier struct {
	Name             string  `json:"name"`
	HitRate          float64 `json:"hitRate"`
	RequestsIn       int     `json:"requestsIn"`
	UpstreamRequests int     `json:"upstreamRequests"`
	BytesServed      float64 `json:"bytesServed"`
	ShedRate         float64 `json:"shedRate"`
	InflightFills    int     `json:"inflightFills"`
}

type Capacity struct {
	Pool         string    `json:"pool"`
	Slots        int       `json:"slots"`
	Until        time.Time `json:"until"`
	Reason       string    `json:"reason"`
	Jobs         []string  `json:"jobs"`
	EstCost      float64   `json:"estCost"`
	State        string    `json:"state"`
	PrewarmViews []string  `json:"prewarmViews"`
}
