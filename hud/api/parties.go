package api

import "time"

// Parties (internal/hud/API.md, Parties): the farm's party list, a person
// or an organisation each, and an organisation's members, by name or by a
// group of the studio's own, with the purposes each holds.

// PartyRequest is POST /api/v1/parties. A farm admin makes an
// organisation, naming its first admin, who counts once they accept; a
// person makes their own person Party, signed in with what it lists.
type PartyRequest struct {
	// Name is the bare name records use (kestrel): lower case, digits,
	// dots and dashes, never changed and never reused.
	Name string `json:"name"`
	// Kind is person or organisation.
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	// LegalName, for an organisation, is the name a contract carries.
	LegalName string `json:"legalName,omitempty"`
	// SignIns, for a person, are the sign-in subjects that are them: the
	// one they are signed in with, and a provider's subject IDToken proves.
	SignIns []SignIn `json:"signIns,omitempty"`
	// IDToken, for a person, is a fresh ID token from the farm's sign-in
	// provider for the provider subject SignIns adds.
	IDToken string `json:"idToken,omitempty"`
	// FirstAdmin, for an organisation, names the person Party that is
	// its first admin once they accept (POST /parties/{name}/accept).
	FirstAdmin string `json:"firstAdmin,omitempty"`
}

// SignIn is one sign-in subject that is a person: a provider's subject ID
// at its issuer URL, which the people's issuer certifies their devices on,
// or, without an issuer, a name the HUD signs them in as.
type SignIn struct {
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject"`
}

// PartyChange is PATCH /api/v1/parties/{name}: a party's names, and an
// organisation's per-person views policy. A field left out stays.
type PartyChange struct {
	DisplayName    *string `json:"displayName,omitempty"`
	LegalName      *string `json:"legalName,omitempty"`
	PerPersonViews *bool   `json:"perPersonViews,omitempty"`
}

// SignInsRequest is PUT /api/v1/parties/{name}/sign-ins: a person's
// sign-in subjects, all of them.
type SignInsRequest struct {
	SignIns []SignIn `json:"signIns"`
	// IDToken proves the provider subject SignIns adds, if any.
	IDToken string `json:"idToken,omitempty"`
}

// MemberRequest is a membership's add, change or end: a person by name,
// or a group of the studio's own, never both, and its purposes (none to
// end it).
type MemberRequest struct {
	Person   string   `json:"person,omitempty"`
	Group    string   `json:"group,omitempty"`
	Purposes []string `json:"purposes,omitempty"`
}

// Party is a party as the HUD shows it.
type Party struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	LegalName   string `json:"legalName,omitempty"`
	// SignIns go only to the person and to farm admins.
	SignIns []SignIn `json:"signIns,omitempty"`
	// PerPersonViews: the organisation's policy turns per-person spend
	// views on, and view-people with it.
	PerPersonViews bool      `json:"perPersonViews,omitempty"`
	Made           PartyAct  `json:"made"`
	Changed        *PartyAct `json:"changed,omitempty"`
	Members        []Member  `json:"members"`
	// Groups go with an organisation: the studio's own groups whose
	// members hold purposes, the c4:party:<name> group among them.
	Groups []Member `json:"groups,omitempty"`
	// Booked says the party is in the farm book; false while its
	// make-party record waits to be booked, when it has no members yet.
	Booked bool `json:"booked"`
	// FirstAdmin, for an organisation, is the first admin its make-party
	// named, and whether they accepted.
	FirstAdmin *FirstAdmin `json:"firstAdmin,omitempty"`
	// Yours are the purposes the viewer holds for it, in effect.
	Yours []string `json:"yours,omitempty"`
	// ActOn is what an act changing it is on (PEOPLE-KEYS-2 2.1).
	ActOn *ActOn `json:"actOn,omitempty"`
}

// FirstAdmin is an organisation's first admin: a member, holding member
// and admin, only once they accept with their own touched act.
type FirstAdmin struct {
	Person   string `json:"person"`
	Accepted bool   `json:"accepted"`
	Record   string `json:"record,omitempty"`
}

// PartyAct is one signed change to a party.
type PartyAct struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Record string    `json:"record,omitempty"`
}

// Member is a membership in force: a person by name, or a group.
type Member struct {
	Person string `json:"person,omitempty"`
	Group  string `json:"group,omitempty"`
	// Purposes are those granted; InEffect those that hold now (the
	// view-people purpose holds only where the organisation's policy
	// turns per-person views on).
	Purposes []string `json:"purposes"`
	InEffect []string `json:"inEffect"`
	// By and Record are who granted it and the record that did; the
	// c4:party:<name> group has neither, being the studio's own sign-in.
	By     string `json:"by,omitempty"`
	Record string `json:"record,omitempty"`
	// Since is when it came into force: its book entry's seal, or its
	// writing while pending.
	Since time.Time `json:"since,omitzero"`
	// Pending: its entry waits for the seal.
	Pending bool `json:"pending,omitempty"`
	// Lapsed says why it holds nothing: it names no Party (a name booked
	// before memberships named Parties).
	Lapsed string `json:"lapsed,omitempty"`
}

// PartyList is GET /api/v1/parties.
type PartyList struct {
	Parties []Party `json:"parties"`
	// Purposes are the purposes a member may hold, in order.
	Purposes []string `json:"purposes"`
}

// PartyResponse answers a party's make or change.
type PartyResponse struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message,omitempty"`
	Existing bool   `json:"existing,omitempty"`
	Party    *Party `json:"party"`
	Record   string `json:"record,omitempty"`
}
