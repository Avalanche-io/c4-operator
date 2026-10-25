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
	// OnePerson: the owner is the farm's one person, holding every role.
	OnePerson bool      `json:"onePerson,omitempty"`
	For       []string  `json:"for"`
	Start     time.Time `json:"start"`
	By        string    `json:"by"`
	Record    string    `json:"record,omitempty"`
	Follows   string    `json:"follows,omitempty"`
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
