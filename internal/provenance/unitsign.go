package provenance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// UnitAct is an act a unit makes as itself (PEOPLE-KEYS 5.1): on Subject
// (an object by UID and digest, ActSubject), for the job's owner Owner,
// whom the unit acts for under the job's ownership.
type UnitAct struct {
	Subject Subject
	Act     string
	Params  json.RawMessage
	Owner   string
}

// SignUnitAct signs a as the unit s is certified as,
// unit/<ns>/<job>/<unit>@<job UID>/<attempt>, with the key that never
// leaves the unit: by the unit, for the job's owner, under the job's
// ownership, graded signed. It returns the bundle a request carries,
// base64, in X-C4-Act.
func SignUnitAct(ctx context.Context, s *Signer, a UnitAct, now time.Time) ([]byte, error) {
	farm, err := s.Farm(ctx, now) // also obtains the certificate
	if err != nil {
		return nil, err
	}
	u, ok := ParseUnitIdentity(s.Identity())
	switch {
	case !ok:
		return nil, errors.New("this key is not certified as a unit")
	case a.Owner == "":
		return nil, errors.New("a unit acts for its job's owner: the job names none")
	case a.Act == "":
		return nil, errors.New("an act names what it does")
	}
	params := a.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	by := schema.Actor{Unit: schema.UnitRef(u.Namespace, u.Job, u.JobUID, u.Unit), Grade: schema.GradeSigned}
	p := schema.ActPredicate{Farm: farm, Act: a.Act, Params: params, Signer: s.Identity(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Chain: schema.ActChain{By: by, For: schema.For{Party: a.Owner, Grade: schema.GradeSigned},
			Under: []schema.Delegation{{Job: u.Namespace + "/" + u.Job, Kind: "ownership", From: a.Owner}}}}
	return SignStatement(ctx, s, PredicateAct, []Subject{a.Subject}, p, now)
}
