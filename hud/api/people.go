package api

import "time"

// PeopleEntry is one mapping of a Tracker's people map, as the HUD shows
// it: a tracker login offered as a C4 person (the Tracker's spec.people),
// and whether the person has confirmed it. A confirmed login's decisions
// are relayed as that person's, naming the confirmation; an offered one's
// are relayed marked unconfirmed.
type PeopleEntry struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	Tracker   string `json:"tracker"`
	Login     string `json:"login"`
	// Person is the C4 person, or party:<name>, the login is offered as.
	Person string `json:"person"`
	// State is offered (not confirmed yet), confirmed, or revoked (taken
	// back, or replaced by a later confirmation for the login).
	State string `json:"state"`
	// Name is the confirmation's name, when there is one (for DELETE).
	Name string `json:"name,omitempty"`
	// Confirmed is who confirmed it and when, with its control record.
	Confirmed *PeopleAct `json:"confirmed,omitempty"`
	Revoked   *PeopleAct `json:"revoked,omitempty"`
	// Yours: the person is the viewer, or a party the viewer is in, so
	// it is theirs to confirm or revoke.
	Yours bool `json:"yours"`
	// Lets says, in words, what confirming the login lets it do.
	Lets string `json:"lets"`
}

// PeopleAct is who did something to an entry, when, and its record.
type PeopleAct struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Record string    `json:"record,omitempty"`
}

// PeopleList answers GET /api/v1/people.
type PeopleList struct {
	Entries []PeopleEntry `json:"entries"`
}

// PeopleConfirm is POST /api/v1/people: the signed-in person confirms
// that a tracker login the Tracker offers as them is them.
type PeopleConfirm struct {
	// Project or Namespace is where the Tracker is; one project you can
	// see may be left out.
	Project   string `json:"project,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Tracker   string `json:"tracker"`
	Login     string `json:"login"`
}

// PeopleResponse answers a confirmation or a revocation.
type PeopleResponse struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message"`
	Entry   PeopleEntry `json:"entry"`
	Record  string      `json:"record,omitempty"`
}
