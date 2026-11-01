package people

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The acts the issuer books (POST /v1/people/acts), each an act/v1
// signed by a person's device in force at the index's position, taken
// once by its payload hash:
//   - remove-device: on device:<ID>, one device; on farm:<farm ID> with
//     params.person, all of that person's devices enrolled before it. The
//     person themselves or an admin. From its position on, the device's
//     certificates are no longer in force. params.from, a time before the
//     act, needs a touch (ActNeeds).
//   - set-sign-in-provider is refused. It is on the touch list, and until
//     the touch is built (K9) a page served by a compromised HUD could
//     sign it with an admin's browser key and point the farm at another
//     client. The provider changes only by redeploying the farm with
//     --sign-in-provider and --sign-in-client-id, a cluster
//     administrator's act; K9 opens it, confirmed.
//   - approve-machine: on device:<ID> of a key not yet enrolled, confirmed
//     with the person's touch key: that key may enrol as the person's
//     within ApprovalTTL (evidence approval), as a second machine on a
//     one-person farm or a touch key on a second device.
//   - browser-code: on farm:<farm ID>, params {code: the SHA-256 of a
//     one-time code}, by a one-person farm's owner with the key the
//     installer path enrolled, over loopback.
//   - countersign-device: on device:<ID>, params {record: the device
//     record its code enrolled}, the same.

// ApprovalTTL is how long an approval of a new device works.
const ApprovalTTL = 10 * time.Minute

// The verbs the issuer books.
const (
	ActRemoveDevice = "remove-device"
	ActSetProvider  = "set-sign-in-provider"
	ActApprove      = "approve-machine"
	ActBrowserCode  = "browser-code"
	ActCountersign  = "countersign-device"
)

// Booked is what booking an act returns.
type Booked struct {
	Record   string `json:"record"`
	Position int64  `json:"position"`
	Grade    string `json:"grade"`
	Entry    *Entry `json:"entry"`
}

func (i *Issuer) handleAct(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		fail(w, refuse(http.StatusRequestEntityTooLarge, "an act is at most %d bytes", maxBody))
		return
	}
	b, err := i.takeAct(r, data)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// takeAct verifies an act, checks it may be booked, and books it.
func (i *Issuer) takeAct(r *http.Request, data []byte) (*Booked, error) {
	ctx, now := r.Context(), i.now()
	x := i.Index
	v, err := provenance.VerifyBundle(data, x.Roots)
	if err != nil {
		return nil, fmt.Errorf("the act: %w", err)
	}
	if v.PredicateType != provenance.PredicateAct {
		return nil, refuse(http.StatusBadRequest, "a %s record, not an act", v.PredicateType)
	}
	a, errs := schema.ParseAct(v.Payload)
	if errs != nil {
		return nil, refuse(http.StatusBadRequest, "not an act: %s: %s", errs[0].Path, errs[0].Message)
	}
	verb, by := a.Predicate.Act, a.Predicate.Chain.By.Person
	grade := GradeSigned
	if v.Confirmed() {
		grade = GradeConfirmed
	}
	if verb == ActSetProvider {
		return nil, refuse(http.StatusForbidden, "until a touch can confirm it, the sign-in provider changes only by redeploying the farm with --sign-in-provider and --sign-in-client-id")
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, err
	}
	if by == "" {
		return nil, refuse(http.StatusForbidden, "%s is a person's act", verb)
	}
	if d := now.Sub(v.SignedAt); d > provenance.ActSkew || d < -provenance.ActSkew {
		return nil, fmt.Errorf("%w: signed %s from the issuer's clock", provenance.ErrClock, d.Round(time.Second))
	}
	pos, err := x.Position(ctx)
	if err != nil {
		return nil, err
	}
	if err := x.CheckAct(ctx, v, pos); err != nil {
		return nil, err
	}
	var params map[string]string
	if err := json.Unmarshal(a.Predicate.Params, &params); err != nil {
		return nil, refuse(http.StatusBadRequest, "%s's params are an object of strings: %v", verb, err)
	}
	subject := a.Subject[0]
	farmSubject := subject.Name == "farm:"+i.Farm() && subject.Digest["c4"] == i.Farm()
	device := ""
	if id := subject.Digest["c4"]; subject.Name == "device:"+id {
		device = id
	}
	var build func() (*Entry, error)
	switch verb {
	case ActRemoveDevice:
		build, err = i.removal(r, by, device, farmSubject, params)
	case ActApprove:
		build, err = i.approval(by, device, now)
	case ActBrowserCode, ActCountersign:
		build, err = i.ownerAct(r, v, verb, by, device, farmSubject, params, now)
	default:
		err = refuse(http.StatusBadRequest, "the people's issuer books %s, %s, %s and %s acts, not %s", ActRemoveDevice, ActApprove, ActBrowserCode, ActCountersign, verb)
	}
	if err != nil {
		return nil, err
	}
	until := v.Expires
	if t := v.SignedAt.Add(provenance.ActSkew); t.After(until) {
		until = t
	}
	if err := i.acts().Take(provenance.PayloadHash(v.Payload), until); err != nil {
		return nil, fmt.Errorf("the act was taken once already: %w", err)
	}
	hash := provenance.PayloadHash(v.Payload)
	es, err := x.append(ctx, now, func() ([]pending, error) {
		// The index is shared and lasting, so an act it books is refused a
		// second time at any replica, after any restart, by its payload
		// hash (PEOPLE-KEYS-2 2.1, replay).
		if x.acts[hash] {
			return nil, refuse(http.StatusConflict, "the act is booked already (payload %.16s)", hash)
		}
		e, err := build()
		if err != nil {
			return nil, err
		}
		e.By = by
		return []pending{{entry: e, record: data}}, nil
	})
	if err != nil {
		return nil, err
	}
	e := es[0]
	return &Booked{Record: e.Record, Position: e.Position, Grade: grade, Entry: e}, nil
}

// removal books a device's removal, or all of a person's devices': by the
// person themselves or an admin.
func (i *Issuer) removal(r *http.Request, by, device string, farmSubject bool, params map[string]string) (func() (*Entry, error), error) {
	for k := range params {
		if k != "from" && k != "person" {
			return nil, refuse(http.StatusBadRequest, "remove-device takes from and person, not %s", k)
		}
	}
	if from := params["from"]; from != "" {
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			return nil, refuse(http.StatusBadRequest, "from %q is not RFC 3339", from)
		}
	}
	whole := params["person"]
	switch {
	case device != "" && whole == "":
	case farmSubject && whole != "":
	default:
		return nil, refuse(http.StatusBadRequest, "remove-device is on device:<ID>, or on farm:<farm ID> with params.person for all of a person's devices")
	}
	admin := i.admin(r.Context(), by)
	x := i.Index
	return func() (*Entry, error) {
		person := whole
		if device != "" {
			d := x.at(int64(len(x.entries)))[device]
			if d == nil || d.removed {
				return nil, refuse(http.StatusNotFound, "device %.16s is not enrolled here, or is removed already", device)
			}
			person = d.person()
		}
		if by != person && !admin {
			return nil, refuse(http.StatusForbidden, "%s may remove their own devices, and an admin anyone's; %s's are not %s's", by, person, by)
		}
		return &Entry{Kind: KindRemoval, Device: device, Person: person, From: params["from"]}, nil
	}, nil
}

// approval books a confirmed approval of a new device: the verifier has
// checked the act is confirmed by the person's touch key, in force.
func (i *Issuer) approval(by, device string, now time.Time) (func() (*Entry, error), error) {
	if device == "" {
		return nil, refuse(http.StatusBadRequest, "approve-machine is on the new key, device:<ID>")
	}
	x := i.Index
	return func() (*Entry, error) {
		if err := x.unseen(device); err != nil {
			return nil, err
		}
		return &Entry{Kind: KindApproval, Device: device, Person: by, Expires: now.Add(ApprovalTTL).UTC().Format(time.RFC3339Nano)}, nil
	}, nil
}

// ownerAct books a one-person farm's browser code or countersignature:
// the owner's, over loopback, signed with the key the installer path
// enrolled.
func (i *Issuer) ownerAct(r *http.Request, v *provenance.Verified, verb, by, device string, farmSubject bool, params map[string]string, now time.Time) (func() (*Entry, error), error) {
	if !loopback(r) {
		return nil, refuse(http.StatusForbidden, "%s is made over loopback, from the farm's own machine", verb)
	}
	x := i.Index
	check := func() error {
		owner := x.ownerRecord()
		if owner == nil || owner.Predicate.Person != by || i.owner(r.Context()) != by {
			return refuse(http.StatusForbidden, "%s is the one-person farm owner's act, and %s is not its enrolled owner", verb, by)
		}
		for _, d := range x.inForce(int64(len(x.entries)), now) {
			if d.id == v.Device && d.rec.Predicate.Evidence != schema.EvidenceInstaller {
				return refuse(http.StatusForbidden, "%s is signed with the owner's own key from c4 farm up, not device %.16s", verb, v.Device)
			}
		}
		return nil
	}
	switch {
	case verb == ActBrowserCode && farmSubject && len(params) == 1 && hex64.MatchString(params["code"]):
		code := params["code"]
		return func() (*Entry, error) {
			if err := check(); err != nil {
				return nil, err
			}
			if x.code(code) != nil {
				return nil, refuse(http.StatusConflict, "that code is booked already: make a new one")
			}
			return &Entry{Kind: KindCode, Code: code, Person: by, Expires: now.Add(CodeTTL).UTC().Format(time.RFC3339Nano)}, nil
		}, nil
	case verb == ActCountersign && device != "" && len(params) == 1 && params["record"] != "":
		record := params["record"]
		return func() (*Entry, error) {
			if err := check(); err != nil {
				return nil, err
			}
			d := x.devices[record]
			e := x.find(KindDevice, record)
			switch {
			case d == nil || e == nil || e.Device != device:
				return nil, refuse(http.StatusNotFound, "record %.16s is not device %.16s's", record, device)
			case d.Predicate.Evidence != schema.EvidenceOwner || d.Predicate.Person != by:
				return nil, refuse(http.StatusForbidden, "the owner countersigns a browser their code enrolled, and %.16s is not one", record)
			}
			for _, c := range x.entries {
				if c.Kind == KindCountersign && c.Of == record {
					return nil, refuse(http.StatusConflict, "record %.16s is countersigned already", record)
				}
			}
			return &Entry{Kind: KindCountersign, Device: device, Person: by, Of: record}, nil
		}, nil
	}
	return nil, refuse(http.StatusBadRequest, "browser-code is on farm:<farm ID> with params {code: a SHA-256, hex}; countersign-device on device:<ID> with params {record}")
}
