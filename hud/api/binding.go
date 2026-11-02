package api

import "time"

// FarmBindingRequest is POST /api/v1/farm/binding: the installer names the
// farm's owner, binding every deployment of the farm to them.
type FarmBindingRequest struct {
	// Owner is the party the farm's records accrue to.
	Owner string `json:"owner"`
	// To is <farm>/*, every deployment of the farm.
	To string `json:"to"`
	// RunFor, on a managed farm, is the party the owner runs it for.
	RunFor string `json:"runFor,omitempty"`
	// OnePerson says the farm is one person's, the owner's, who then holds
	// every role on it. Only a local farm's bind request says it; a
	// shared farm's binding leaves it unset. It never goes with RunFor.
	OnePerson bool `json:"onePerson,omitempty"`
}

// FarmBinding is the farm's owner as bound: every deployment's records
// accrue to Owner.
type FarmBinding struct {
	Owner  string `json:"owner"`
	To     string `json:"to"`
	RunFor string `json:"runFor,omitempty"`
	// OnePerson: the installer bound the farm as the owner's alone.
	OnePerson bool `json:"onePerson,omitempty"`
	// OnePersonInEffect is whether the owner holds every role now: only
	// on a HUD reached with this machine's installer token. OnePersonNote
	// says so in words for the farm page, for a onePerson binding.
	OnePersonInEffect bool      `json:"onePersonInEffect,omitempty"`
	OnePersonNote     string    `json:"onePersonNote,omitempty"`
	For               []string  `json:"for"`
	Start             time.Time `json:"start"`
	By                string    `json:"by"`
	Record            string    `json:"record,omitempty"`
	Follows           string    `json:"follows,omitempty"`
	// Name is the DeploymentBinding's name; Deployment what its to names
	// after the farm (node/laptop-1), or * for the farm binding.
	Name       string `json:"name,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	// End is when it stops by itself. Standing is a binding with no end,
	// not ended: the farm page marks it "standing" until the farm has an
	// annual review.
	End      *time.Time `json:"end,omitempty"`
	Standing bool       `json:"standing,omitempty"`
	// Ended is its early end, by an end-binding or a binding that
	// follows it.
	Ended *BindingEnded `json:"ended,omitempty"`
	// InForce is whether it is in force now (GET /api/v1/bindings).
	InForce bool `json:"inForce,omitempty"`
	// ActOn is what an act rotating or ending it is on.
	ActOn *ActOn `json:"actOn,omitempty"`
}

// BindingEnded is a binding's early end: when, by whom, the record that
// ended it, and the binding that follows it when an owner change did.
type BindingEnded struct {
	At         time.Time `json:"at"`
	By         string    `json:"by"`
	Record     string    `json:"record"`
	FollowedBy string    `json:"followedBy,omitempty"`
}

// BindingRequest is POST /api/v1/bindings: a farm admin binds one
// deployment of the farm to its owner. For defaults to every kind the
// signer policy lets that deployment sign, and may only narrow it.
type BindingRequest struct {
	Owner string     `json:"owner"`
	To    string     `json:"to"`
	For   []string   `json:"for,omitempty"`
	End   *time.Time `json:"end,omitempty"`
}

// RotateBindingRequest is POST /api/v1/bindings/{name}/rotate: a new
// owner for the same deployment, a binding that follows this one from
// now, which ends it at the same instant.
type RotateBindingRequest struct {
	Owner string     `json:"owner"`
	For   []string   `json:"for,omitempty"`
	End   *time.Time `json:"end,omitempty"`
}

// EndBindingRequest is POST /api/v1/bindings/{name}/end: the binding
// ended now. Owner is the binding's owner, as the act names it: its owner
// ending their own needs no touch, anyone else ending it does.
type EndBindingRequest struct {
	Owner string `json:"owner"`
}

// BindingsResponse is GET /api/v1/bindings: every binding of the farm,
// in force or not, the farm binding first, then by deployment and start.
type BindingsResponse struct {
	OK       bool          `json:"ok"`
	Farm     string        `json:"farm,omitempty"`
	Bindings []FarmBinding `json:"bindings"`
}

// SignerBinding is a record's signer followed to its owner (DESIGN 3.3):
// the binding its certificate names, that binding's owner and deployment,
// and a label where it doesn't lead to one. Label is
// "unattributed: deployment has no owner" for a signer certified with no
// binding, or one not in force at the record's book position, and
// "revoked" for a record booked after its binding ended early. Before
// strict accountability (K8) a label never refuses a record.
type SignerBinding struct {
	Binding    string `json:"id,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Label      string `json:"label,omitempty"`
}

// FarmBindingResponse answers a bind or GET /api/v1/farm/binding.
// Existing says the farm was already bound to the same owner, so nothing
// changed.
type FarmBindingResponse struct {
	OK       bool         `json:"ok"`
	Message  string       `json:"message,omitempty"`
	Existing bool         `json:"existing,omitempty"`
	Binding  *FarmBinding `json:"binding"`
	Record   string       `json:"record,omitempty"`
}
