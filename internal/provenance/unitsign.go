package provenance

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// UnitReportActs are the acts a unit signs about itself: what it asks
// (ask), answers (answer), needs (needs), withdraws (withdraw) and waits
// on (wait). Each is on its own pod.
var UnitReportActs = map[string]bool{"ask": true, "answer": true, "needs": true, "withdraw": true, "wait": true}

// VerifyUnitReport checks a unit's act on itself as the farm reads it
// from the unit's pod: the bundle, an act/v1 record (checkAct), signed by
// a unit (its leaf a well-formed unit identity), one of UnitReportActs,
// and signed no later than now. Unlike VerifyAct it neither requires a
// fresh signature nor takes the act once: the farm reads the pod's acts
// again on every pass and may read a needs long after the unit ended,
// and the act's subject pins it to one pod's UID and one attempt, so a
// replay could only repeat that pod's own report, which the farm reads
// by its number. The caller holds it to that pod, its attempt and its
// job's owner.
func VerifyUnitReport(data []byte, roots *x509.CertPool, now time.Time) (*Verified, *schema.Act, error) {
	v, err := VerifyBundle(data, roots)
	if err != nil {
		return nil, nil, err
	}
	if v.PredicateType != PredicateAct {
		return nil, nil, fmt.Errorf("a %s record, not an act", v.PredicateType)
	}
	a, err := checkAct(v)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := ParseUnitIdentity(v.Identity); !ok {
		return nil, nil, fmt.Errorf("%s is not a unit: a unit's report is signed by the unit", v.Identity)
	}
	if !UnitReportActs[a.Predicate.Act] {
		return nil, nil, fmt.Errorf("a %s act is not a unit's report on itself", a.Predicate.Act)
	}
	if v.SignedAt.After(now.Add(ActSkew)) {
		return nil, nil, fmt.Errorf("%w: signed at %s, after %s", ErrClock, v.SignedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return v, a, nil
}
