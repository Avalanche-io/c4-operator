package people

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Request is a device's enrolment request, as its proof signs it
// (schema.DevicePredicate.ProofMessage): the farm and the person, so one
// request can't be replayed at another farm or for another person; the
// key; the device key a touch key confirms for; a nonce; and the device's
// clock.
type Request struct {
	Farm        string `json:"farm"`
	Person      string `json:"person"`
	Key         string `json:"key"`
	TouchFor    string `json:"touchFor,omitempty"`
	Nonce       string `json:"nonce"`
	RequestedAt string `json:"requestedAt"`
}

// Enrolment is what a device sends to be certified or renewed: its
// request, its proof over it, what it is, and its evidence.
type Enrolment struct {
	Request Request `json:"request"`
	// Proof is the key's raw r||s signature (low S) over the request's
	// canonical JSON, base64url.
	Proof      string `json:"proof"`
	Protection string `json:"protection"`
	Kind       string `json:"kind"`
	Label      string `json:"label,omitempty"`
	// IDToken is the sign-in provider's ID token, whose nonce is the
	// device ID.
	IDToken string `json:"idToken,omitempty"`
	// Approval is the C4 ID of a booked approve-machine act naming this
	// device (evidence approval).
	Approval string `json:"approval,omitempty"`
	// Code is a one-person farm's one-time code (POST /v1/people/browser).
	Code string `json:"code,omitempty"`
}

// Certified is a device's certificate and the record behind it.
type Certified struct {
	// Farm is the farm's ID, the C4 ID of the CA certificate.
	Farm string `json:"farm"`
	// Identity is the leaf's identity exactly as certified
	// (https://cccc.io/id/user/<name>): a client copies it, never
	// computes it.
	Identity string `json:"identity"`
	Device   string `json:"device"`
	// Record is the device record's C4 ID, and Position its entry's.
	Record   string `json:"record"`
	Position int64  `json:"position"`
	Order    string `json:"order"`
	// Chain is DER, leaf first, then the farm CA.
	Chain   [][]byte  `json:"chain"`
	Expires time.Time `json:"expires"`
}

// predicate is an enrolment as a device record's predicate, with its key
// checked and its proof verified; the evidence is the caller's.
func (i *Issuer) predicate(e *Enrolment, now time.Time) (*schema.DevicePredicate, *ecdsa.PublicKey, string, error) {
	q := e.Request
	p := &schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt,
		Proof: e.Proof, Protection: e.Protection, Kind: e.Kind, Label: e.Label}
	if q.Farm != i.Farm() {
		return nil, nil, "", refuse(http.StatusBadRequest, "the request is for farm %.16s, and this is %.16s", q.Farm, i.Farm())
	}
	at, err := time.Parse(time.RFC3339, q.RequestedAt)
	if err != nil {
		return nil, nil, "", refuse(http.StatusBadRequest, "requestedAt %q is not RFC 3339", q.RequestedAt)
	}
	if d := now.Sub(at); d > provenance.ActSkew || d < -provenance.ActSkew {
		return nil, nil, "", refuse(http.StatusForbidden, "the request was made %s from the issuer's clock: send a fresh one", d.Round(time.Second))
	}
	if err := checkProof(p); err != nil {
		return nil, nil, "", refuse(http.StatusForbidden, "%v", err)
	}
	spki, _ := p.PublicKey()
	pub, _ := x509.ParsePKIXPublicKey(spki)
	return p, pub.(*ecdsa.PublicKey), schema.DeviceID(spki), nil
}

// checkProof checks a device record's key is P-256 and its proof is the
// key's own signature over the request.
func checkProof(p *schema.DevicePredicate) error {
	spki, err := p.PublicKey()
	if err != nil {
		return fmt.Errorf("the key is not base64url: %v", err)
	}
	pk, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return fmt.Errorf("the key is not a SubjectPublicKeyInfo: %v", err)
	}
	pub, ok := pk.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return errors.New("a device key is ECDSA P-256")
	}
	sig, err := base64.RawURLEncoding.DecodeString(p.Proof)
	if err != nil {
		return errors.New("the proof is not base64url")
	}
	if err := provenance.VerifyP256(pub, p.ProofMessage(), sig); err != nil {
		return fmt.Errorf("the proof is not the key's signature over its request: %w", err)
	}
	return nil
}

// deviceRecord signs a device record as the issuer, after checking its
// shape against the schema.
func (i *Issuer) deviceRecord(ctx context.Context, p *schema.DevicePredicate, device string, now time.Time) ([]byte, error) {
	p.SignedAt = now.UTC().Format(time.RFC3339Nano)
	subject := schema.Subject{Name: "device:" + device, Digest: map[string]string{"c4": device}}
	st, err := json.Marshal(schema.Device{Type: schema.StatementType, Subject: []schema.Subject{subject}, PredicateType: provenance.PredicateDevice, Predicate: *p})
	if err != nil {
		return nil, err
	}
	if _, errs := schema.ParseDevice(st); errs != nil {
		return nil, refuse(http.StatusBadRequest, "the device record: %s: %s", errs[0].Path, errs[0].Message)
	}
	s, err := i.sign(ctx)
	if err != nil {
		return nil, err
	}
	return provenance.SignStatement(ctx, s, provenance.PredicateDevice, []provenance.Subject{{Name: subject.Name, Digest: subject.Digest}}, p, now)
}

// certify issues the leaf for a device whose record e booked.
func (i *Issuer) certify(pub *ecdsa.PublicKey, person, device string, e *Entry, now time.Time) (*Certified, error) {
	leaf, err := i.CA.IssueLeaf(pub, provenance.UserIdentity(person), device, "", LeafTTL, now)
	if err != nil {
		return nil, err
	}
	return &Certified{Farm: i.Farm(), Identity: provenance.CertIdentity(leaf), Device: device, Record: e.Record, Position: e.Position,
		Order: OrderIssuer, Chain: [][]byte{leaf.Raw, i.CA.Cert.Raw}, Expires: leaf.NotAfter}, nil
}

// handleEnroll certifies a new device on an ID token, or on a booked
// approval from another of the person's devices.
func (i *Issuer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var e Enrolment
	if err := decode(r, &e); err != nil {
		fail(w, err)
		return
	}
	var c *Certified
	var err error
	switch {
	case e.Code != "":
		err = refuse(http.StatusBadRequest, "a browser the owner's code opened enrols at POST /v1/people/browser")
	case e.Approval != "":
		c, err = i.enrollApproved(r, &e)
	default:
		c, err = i.enrollSignIn(r, &e)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// enrollSignIn certifies a device on an ID token from the farm's sign-in
// provider: the subject is the request's person's sign-in, the nonce the
// device ID. A touch key's token is a step-up, and the device it confirms
// for is the person's, in force.
func (i *Issuer) enrollSignIn(r *http.Request, e *Enrolment) (*Certified, error) {
	ctx, now := r.Context(), i.now()
	p, pub, device, err := i.predicate(e, now)
	if err != nil {
		return nil, err
	}
	if e.IDToken == "" {
		return nil, refuse(http.StatusBadRequest, "an enrolment carries the sign-in provider's ID token (idToken), or an approval")
	}
	in, err := i.signedIn(ctx, e.IDToken, device, p.Person, p.TouchFor != "")
	if err != nil {
		return nil, err
	}
	p.Evidence, p.IDToken, p.ProviderKey = schema.EvidenceIDToken, e.IDToken, in.keyID
	x := i.Index
	es, err := x.append(ctx, now, func() ([]pending, error) {
		if err := x.unseen(device); err != nil {
			return nil, err
		}
		if err := x.touchesFor(p, now); err != nil {
			return nil, err
		}
		var ps []pending
		if !x.booked(KindProviderKey, in.keyID) {
			ps = append(ps, pending{entry: &Entry{Kind: KindProviderKey, Issuer: in.issuer, KeyID: in.kid}, record: in.jwk})
		}
		rec, err := i.deviceRecord(ctx, p, device, now)
		if err != nil {
			return nil, err
		}
		return append(ps, pending{entry: &Entry{Kind: KindDevice, Device: device, Person: p.Person, Addr: i.addr(r)}, record: rec}), nil
	})
	if err != nil {
		return nil, err
	}
	return i.certify(pub, p.Person, device, es[len(es)-1], now)
}

// signedIn checks an ID token for device against the setting in force,
// and that its subject is person's sign-in.
func (i *Issuer) signedIn(ctx context.Context, raw, device, person string, stepUp bool) (*signIn, error) {
	set, err := i.setting(ctx)
	if err != nil {
		return nil, err
	}
	if set == nil {
		set = &Setting{}
	}
	in, err := i.checkIDToken(ctx, set.Provider, raw, device, stepUp)
	if err != nil {
		return nil, err
	}
	if i.SignIns == nil {
		return nil, refuse(http.StatusForbidden, "this farm names no one's sign-ins")
	}
	who, err := i.SignIns.Person(ctx, in.issuer, in.subject)
	if err != nil {
		return nil, err
	}
	switch {
	case who == "":
		return nil, refuse(http.StatusForbidden, "subject %q at %s is no one's sign-in here: a person's signIns name it", in.subject, in.issuer)
	case who != person:
		return nil, refuse(http.StatusForbidden, "subject %q at %s is %s's sign-in, not %s's", in.subject, in.issuer, who, person)
	}
	return in, nil
}

// enrollApproved certifies a device on a confirmed approve-machine act
// the index booked: a touch from another of the person's devices, naming
// this one, unexpired and not used before.
func (i *Issuer) enrollApproved(r *http.Request, e *Enrolment) (*Certified, error) {
	ctx, now := r.Context(), i.now()
	p, pub, device, err := i.predicate(e, now)
	if err != nil {
		return nil, err
	}
	// On a farm with a sign-in provider a new device enrols by signing in
	// (PEOPLE-KEYS-2 7); an approval enrols only a touch key there.
	if e.Request.TouchFor == "" {
		set, err := i.setting(ctx)
		if err != nil {
			return nil, err
		}
		if set != nil {
			return nil, refuse(http.StatusForbidden, "this farm signs in at %s: a new device enrols by signing in, and an approval enrols only a touch key", set.Issuer)
		}
	}
	p.Evidence, p.Approval = schema.EvidenceApproval, e.Approval
	x := i.Index
	es, err := x.append(ctx, now, func() ([]pending, error) {
		if err := x.unseen(device); err != nil {
			return nil, err
		}
		a := x.find(KindApproval, e.Approval)
		switch {
		case a == nil:
			return nil, refuse(http.StatusForbidden, "no approval %.16s is booked", e.Approval)
		case a.Device != device || a.Person != p.Person:
			return nil, refuse(http.StatusForbidden, "approval %.16s is for %s's device %.16s, not %s's %.16s", e.Approval, a.Person, a.Device, p.Person, device)
		case now.After(a.expires()):
			return nil, refuse(http.StatusForbidden, "approval %.16s lapsed at %s", e.Approval, a.Expires)
		}
		if err := x.touchesFor(p, now); err != nil {
			return nil, err
		}
		rec, err := i.deviceRecord(ctx, p, device, now)
		if err != nil {
			return nil, err
		}
		return []pending{{entry: &Entry{Kind: KindDevice, Device: device, Person: p.Person, Addr: i.addr(r)}, record: rec}}, nil
	})
	if err != nil {
		return nil, err
	}
	return i.certify(pub, p.Person, device, es[len(es)-1], now)
}

// handleRenew certifies a device in force again, for another LeafTTL: on
// a fresh ID token with the same nonce (the device ID) for a device
// enrolled on one, and on the device's own proof for one enrolled on a
// one-person farm, which has no provider to go back to. Each renewal is a
// device record of its own, naming the record it renews.
func (i *Issuer) handleRenew(w http.ResponseWriter, r *http.Request) {
	var e Enrolment
	if err := decode(r, &e); err != nil {
		fail(w, err)
		return
	}
	c, err := i.renew(r, &e)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (i *Issuer) renew(r *http.Request, e *Enrolment) (*Certified, error) {
	ctx, now := r.Context(), i.now()
	p, pub, device, err := i.predicate(e, now)
	if err != nil {
		return nil, err
	}
	x := i.Index
	x.mu.Lock()
	err = x.sync(ctx)
	var evidence string
	if d := x.inForce(int64(len(x.entries)), now)[device]; err == nil && d != nil {
		evidence = d.rec.Predicate.Evidence
	}
	x.mu.Unlock()
	if err != nil {
		return nil, err
	}
	set, err := i.setting(ctx)
	if err != nil {
		return nil, err
	}
	var in *signIn
	if set != nil {
		// On a farm with a sign-in provider, every device renews on a
		// fresh ID token for its person, whatever it was enrolled on: the
		// provider vouches again, or nothing renews, so a leaver's last
		// leaf ends. Only a farm with no provider, a one-person farm,
		// renews on the device's own proof.
		if e.IDToken == "" {
			return nil, refuse(http.StatusForbidden, "this farm signs in at %s: device %.16s renews on a fresh ID token", set.Issuer, device)
		}
		if in, err = i.signedIn(ctx, e.IDToken, device, p.Person, false); err != nil {
			return nil, err
		}
	}
	addr := i.addr(r)
	es, err := x.append(ctx, now, func() ([]pending, error) {
		d := x.at(int64(len(x.entries)))[device]
		switch {
		case d == nil:
			return nil, refuse(http.StatusNotFound, "device %.16s is not enrolled: enrol it", device)
		case d.removed:
			return nil, refuse(http.StatusForbidden, "device %.16s was removed: a removed key never comes back; make a new key", device)
		case now.Sub(d.lastAt) > Lapse:
			return nil, refuse(http.StatusForbidden, "device %.16s lapsed: it last renewed at %s, more than %s ago; make a new key", device, d.lastAt.UTC().Format(time.RFC3339), Lapse)
		case d.rec.Predicate.Evidence != evidence:
			return nil, refuse(http.StatusConflict, "device %.16s changed while it renewed: send again", device)
		}
		old := d.rec.Predicate
		if p.Person != old.Person || p.TouchFor != old.TouchFor {
			return nil, refuse(http.StatusForbidden, "device %.16s is %s's (touchFor %q); the request says %s's (%q)", device, old.Person, old.TouchFor, p.Person, p.TouchFor)
		}
		if old.TouchFor != "" && x.inForce(int64(len(x.entries)), now)[old.TouchFor] == nil {
			return nil, refuse(http.StatusForbidden, "touch key %.16s's device %.16s is not in force", device, old.TouchFor)
		}
		q := *p
		q.Protection, q.Kind, q.Label, q.Evidence, q.Code, q.Approval, q.Renews = old.Protection, old.Kind, old.Label, old.Evidence, old.Code, old.Approval, d.record
		var ps []pending
		if in != nil {
			q.IDToken, q.ProviderKey = in.raw, in.keyID
			if !x.booked(KindProviderKey, in.keyID) {
				ps = append(ps, pending{entry: &Entry{Kind: KindProviderKey, Issuer: in.issuer, KeyID: in.kid}, record: in.jwk})
			}
		}
		rec, err := i.deviceRecord(ctx, &q, device, now)
		if err != nil {
			return nil, err
		}
		ps = append(ps, pending{entry: &Entry{Kind: KindDevice, Device: device, Person: q.Person, Addr: addr}, record: rec})
		// The same device from two places at once: renewals hours apart
		// are the rule, so two within TwoPlaces from different addresses
		// are flagged, and the person is told (PEOPLE-KEYS-2 7).
		if d.addr != "" && addr != d.addr && now.Sub(d.lastAt) < TwoPlaces {
			ps = append(ps, pending{entry: &Entry{Kind: KindFlag, Flag: FlagTwoPlaces, Device: device, Person: q.Person,
				Seen: []string{d.addr + " at " + d.lastAt.UTC().Format(time.RFC3339), addr + " at " + now.UTC().Format(time.RFC3339)}}})
		}
		return ps, nil
	})
	if err != nil {
		return nil, err
	}
	for _, en := range es {
		if en.Kind == KindDevice {
			return i.certify(pub, p.Person, device, en, now)
		}
	}
	return nil, errors.New("the renewal booked no device record")
}

// unseen refuses a key the index has seen before: one in force renews,
// and a removed or lapsed one never comes back.
func (x *Index) unseen(device string) error {
	d := x.at(int64(len(x.entries)))[device]
	switch {
	case d == nil:
		return nil
	case d.removed:
		return refuse(http.StatusConflict, "device %.16s was removed: a removed key never comes back; make a new key", device)
	}
	return refuse(http.StatusConflict, "device %.16s is enrolled already: renew it (POST /v1/people/renew), or make a new key if it lapsed", device)
}

// touchesFor checks a touch key's device: in force, the same person's,
// and not itself a touch key.
func (x *Index) touchesFor(p *schema.DevicePredicate, now time.Time) error {
	if p.TouchFor == "" {
		return nil
	}
	d := x.inForce(int64(len(x.entries)), now)[p.TouchFor]
	switch {
	case d == nil:
		return refuse(http.StatusForbidden, "a touch key confirms for a device in force, and %.16s isn't", p.TouchFor)
	case d.person() != p.Person:
		return refuse(http.StatusForbidden, "device %.16s is %s's, not %s's", p.TouchFor, d.person(), p.Person)
	case d.rec.Predicate.TouchFor != "":
		return refuse(http.StatusForbidden, "device %.16s is itself a touch key", p.TouchFor)
	}
	return nil
}

// booked reports whether an entry of this kind books record.
func (x *Index) booked(kind, record string) bool { return x.find(kind, record) != nil }

// find is the entry of this kind that books record.
func (x *Index) find(kind, record string) *Entry {
	for _, e := range x.entries {
		if e.Kind == kind && e.Record == record {
			return e
		}
	}
	return nil
}

// settingAt is the sign-in provider setting in force at position pos.
func (x *Index) settingAt(pos int64) *Setting {
	var s *Setting
	for _, e := range x.entries[:min(pos, int64(len(x.entries)))] {
		if e.Kind == KindSetting {
			s = &Setting{Provider: Provider{Issuer: e.Issuer, ClientID: e.ClientID}, Grade: e.Grade, Position: e.Position, Record: e.Record}
		}
	}
	return s
}

// setting is the sign-in provider in force now. The install's preset is
// booked the first time it is read, and again when the install changes
// it, until an admin's signed act sets the provider; from then the act's
// setting stands, whatever the install says, as the farm admin's
// telemetry setting does.
func (i *Issuer) setting(ctx context.Context) (*Setting, error) {
	x := i.Index
	stands := func() (*Setting, bool) {
		s := x.settingAt(int64(len(x.entries)))
		return s, i.Provider.Issuer == "" || s != nil && (s.Grade != GradeInstall || s.Provider == i.Provider)
	}
	x.mu.Lock()
	err := x.sync(ctx)
	s, ok := stands()
	x.mu.Unlock()
	if err != nil || ok {
		return s, err
	}
	_, err = x.append(ctx, i.now(), func() ([]pending, error) {
		if _, ok := stands(); ok {
			return nil, nil
		}
		return []pending{{entry: &Entry{Kind: KindSetting, Issuer: i.Provider.Issuer, ClientID: i.Provider.ClientID, Grade: GradeInstall}}}, nil
	})
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.settingAt(int64(len(x.entries))), nil
}

// ownerRecord is a one-person farm's owner key's first record, the one
// the installer path made, or nil.
func (x *Index) ownerRecord() *schema.Device {
	for _, e := range x.entries {
		if d := x.devices[e.Record]; e.Kind == KindDevice && d != nil && d.Predicate.Evidence == schema.EvidenceInstaller && d.Predicate.TouchFor == "" {
			return d
		}
	}
	return nil
}
