package api

import "time"

// GrantRequest is POST /api/v1/grants: the signed-in person lets a
// Tracker submit work for them (integrations GAPS 17). The grant is
// always the viewer's own; nobody grants for someone else.
type GrantRequest struct {
	// Project or Namespace is where the Tracker is; one project you can
	// see may be left out.
	Project   string `json:"project,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Tracker is the Tracker's name.
	Tracker string `json:"tracker"`
	// Pools and Classes limit the jobs it may submit for you; empty, any.
	Pools   []string `json:"pools,omitempty"`
	Classes []string `json:"classes,omitempty"`
	// Until is when the grant ends: an RFC 3339 time, or a span from now
	// ("30d", "72h"). Default 30 days, at most 90; every grant ends.
	Until string `json:"until,omitempty"`
}

// Grant is a person's grant to a Tracker as the HUD shows it.
type Grant struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Project   string    `json:"project"`
	Person    string    `json:"person"`
	Tracker   string    `json:"tracker"`
	Pools     []string  `json:"pools,omitempty"`
	Classes   []string  `json:"classes,omitempty"`
	Until     time.Time `json:"until"`
	// Record is the C4 ID of the signed control record of the grant.
	Record  string        `json:"record,omitempty"`
	Revoked *GrantRevoked `json:"revoked,omitempty"`
	// Active says whether it serves now; Why says why not.
	Active bool   `json:"active"`
	Why    string `json:"why,omitempty"`
}

// GrantRevoked is who took a grant back, and when.
type GrantRevoked struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Record string    `json:"record,omitempty"`
}

// GrantResponse answers a grant or a revocation.
type GrantResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Grant   Grant  `json:"grant"`
	Record  string `json:"record,omitempty"`
}

// GrantList answers GET /api/v1/grants.
type GrantList struct {
	Grants []Grant `json:"grants"`
}
