package api

// Telemetry is what the farm keeps about itself (GET /api/v1/telemetry,
// DESIGN.md section 6.9).
type Telemetry struct {
	// Tier is a (off: a diagnostic bundle holds the present moment) or b
	// (diagnostic bundles: the ring keeps recent decisions, locally).
	Tier string `json:"tier"`
	// KillSwitch says C4_TELEMETRY=off is set in the manager's
	// environment, which holds every tier at off.
	KillSwitch bool `json:"killSwitch"`
	// Epoch is the farm key's epoch: pseudonyms of one epoch are
	// unrelated to another's.
	Epoch uint32        `json:"epoch"`
	Ring  TelemetryRing `json:"ring"`
	// Record is the control record that last changed the tier, if any.
	Record string `json:"record,omitempty"`
}

// TelemetryRing is the ring's state: what a bundle can cover.
type TelemetryRing struct {
	// Window is how far back the ring keeps decisions, as a Go duration.
	Window    string `json:"window"`
	MaxBytes  int    `json:"maxBytes"`
	Bytes     int    `json:"bytes"`
	Snapshots int    `json:"snapshots"`
	// Dropped says older snapshots were let go to stay in bounds.
	Dropped bool `json:"dropped"`
}

// TelemetryResolve asks which name a pseudonym is (POST
// /api/v1/telemetry/resolve).
type TelemetryResolve struct {
	Pseudonym string `json:"pseudonym"`
}

// TelemetryName is a pseudonym resolved on the farm: its kind (job, unit,
// project, pool, link, class, ...) and the name.
type TelemetryName struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// TelemetryTier sets the tier (POST /api/v1/telemetry/tier): a or b.
type TelemetryTier struct {
	Tier string `json:"tier"`
}

// TelemetryChange is a change's answer: the tier or epoch now, the
// control record that signs it, and the change in words.
type TelemetryChange struct {
	Tier    string `json:"tier,omitempty"`
	Epoch   uint32 `json:"epoch,omitempty"`
	Record  string `json:"record,omitempty"`
	Message string `json:"message"`
}
