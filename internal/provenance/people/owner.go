package people

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// A one-person farm (PEOPLE-KEYS-2 3.3, 10.4 item 13). There is no sign-in
// provider: the person is the farm's owner, and their acts rest on their
// own machine. c4 farm up enrols the owner's key and touch key once, with
// the installer token as evidence; the issuer refuses that path once an
// owner key exists. After that the owner's key enrols the browser: the CLI
// makes a one-time code, books its SHA-256 in a signed act (browser-code),
// and opens the HUD with it; the browser presents the code with its key
// within CodeTTL, once; the CLI reads back the key the code enrolled and
// countersigns it (countersign-device). A code someone else used first
// shows as a key the CLI didn't see the browser make.
//
// What protects these routes is the installer token, which only the
// owner's machine holds, and the code: a secret that works once, for a
// minute, and is countersigned. Where a request comes from proves nothing
// a client can set (its Host and Origin). Where the HUD listens only on a
// loopback address, its remote address does mean something, and the
// owner's routes then need the machine's own (LocalOnly).

// CodeTTL is how long a one-time code works.
const CodeTTL = time.Minute

// local reports whether a request may take the owner's routes: from the
// machine's own address where the HUD listens only on a loopback address
// (LocalOnly), and from anywhere the HUD is reached otherwise.
func (i *Issuer) local(r *http.Request) bool {
	if !i.LocalOnly {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// OwnerEnrolment is c4 farm up's request: the owner's key, and the touch
// key made at the terminal beside it, whose touchFor is the owner's key.
type OwnerEnrolment struct {
	Owner Enrolment  `json:"owner"`
	Touch *Enrolment `json:"touch,omitempty"`
}

// OwnerCertified is the owner's key's certificate and its touch key's.
type OwnerCertified struct {
	Owner *Certified `json:"owner"`
	Touch *Certified `json:"touch,omitempty"`
}

// handleOwner enrols a one-person farm's owner on the installer token
// (evidence installer), once.
func (i *Issuer) handleOwner(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), i.now()
	owner := i.owner(ctx)
	switch {
	case !i.local(r):
		fail(w, refuse(http.StatusForbidden, "the owner is enrolled from the farm's own machine (c4 farm up)"))
		return
	case len(i.InstallerToken) == 0 || owner == "":
		fail(w, refuse(http.StatusForbidden, "this is not a one-person farm: its people enrol on a sign-in"))
		return
	case !i.installer(r):
		fail(w, refuse(http.StatusForbidden, "the owner is enrolled with this machine's installer token (%s)", InstallerHeader))
		return
	}
	var req OwnerEnrolment
	if err := decode(r, &req); err != nil {
		fail(w, err)
		return
	}
	type key struct {
		e      *Enrolment
		p      *schema.DevicePredicate
		device string
		pub    *ecdsa.PublicKey
	}
	var keys []key
	for _, e := range []*Enrolment{&req.Owner, req.Touch} {
		if e == nil {
			continue
		}
		if e.IDToken != "" || e.Approval != "" || e.Code != "" {
			fail(w, refuse(http.StatusBadRequest, "the owner's keys are enrolled on the installer token alone"))
			return
		}
		p, pub, device, err := i.predicate(e, now)
		if err != nil {
			fail(w, err)
			return
		}
		if p.Person != owner {
			fail(w, refuse(http.StatusForbidden, "this farm is %s's, and the request is %s's", owner, p.Person))
			return
		}
		p.Evidence = schema.EvidenceInstaller
		keys = append(keys, key{e, p, device, pub})
	}
	switch {
	case keys[0].p.TouchFor != "":
		fail(w, refuse(http.StatusBadRequest, "the owner's key is a device key, not a touch key"))
		return
	case len(keys) == 2 && keys[1].p.TouchFor != keys[0].device:
		fail(w, refuse(http.StatusBadRequest, "the touch key confirms for the owner's key %.16s (touchFor)", keys[0].device))
		return
	}
	x := i.Index
	es, err := x.append(ctx, now, i.sign, func() ([]pending, error) {
		if x.ownerRecord() != nil {
			return nil, refuse(http.StatusConflict, "this farm's owner is enrolled already: the installer token enrols the owner once, and the owner's key enrols the browser (c4 farm open)")
		}
		var ps []pending
		for _, k := range keys {
			if err := x.unseen(k.device); err != nil {
				return nil, err
			}
			rec, err := i.deviceRecord(ctx, k.p, k.device, now)
			if err != nil {
				return nil, err
			}
			ps = append(ps, pending{entry: &Entry{Kind: KindDevice, Device: k.device, Person: owner, Addr: i.addr(r)}, record: rec})
		}
		return ps, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	var certs []*Certified
	for n, k := range keys {
		c, err := i.certify(k.pub, owner, k.device, es[n], now)
		if err != nil {
			fail(w, err)
			return
		}
		certs = append(certs, c)
	}
	out := OwnerCertified{Owner: certs[0]}
	if len(certs) == 2 {
		out.Touch = certs[1]
	}
	writeJSON(w, http.StatusOK, out)
}

// TouchKeyWindow is how long after the owner's enrolment the installer
// token still enrols their first touch key. A one-person farm can't attest
// that a touch key asks for a touch, so the route doesn't stay open for
// the farm's life.
const TouchKeyWindow = 24 * time.Hour

// handleOwnerTouch enrols a one-person farm owner's first touch key on the
// installer token (evidence installer), for a farm up made with no one at
// the terminal to give the touch (c4 farm touch-key): within
// TouchKeyWindow of the owner's enrolment, and only while no touch key was
// ever enrolled for the owner's key; after either, 409. A later touch key
// comes from a touch on this one.
func (i *Issuer) handleOwnerTouch(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), i.now()
	owner := i.owner(ctx)
	switch {
	case !i.local(r):
		fail(w, refuse(http.StatusForbidden, "the owner's touch key is enrolled from the farm's own machine (c4 farm touch-key)"))
		return
	case len(i.InstallerToken) == 0 || owner == "":
		fail(w, refuse(http.StatusForbidden, "this is not a one-person farm: a touch key is enrolled on a step-up sign-in"))
		return
	case !i.installer(r):
		fail(w, refuse(http.StatusForbidden, "the owner's touch key is enrolled with this machine's installer token (%s)", InstallerHeader))
		return
	}
	var e Enrolment
	if err := decode(r, &e); err != nil {
		fail(w, err)
		return
	}
	if e.IDToken != "" || e.Approval != "" || e.Code != "" {
		fail(w, refuse(http.StatusBadRequest, "the owner's first touch key is enrolled on the installer token alone"))
		return
	}
	p, pub, device, err := i.predicate(&e, now)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case p.Person != owner:
		fail(w, refuse(http.StatusForbidden, "this farm is %s's, and the request is %s's", owner, p.Person))
		return
	case p.TouchFor == "":
		fail(w, refuse(http.StatusBadRequest, "a touch key confirms for the owner's key (touchFor)"))
		return
	}
	p.Evidence = schema.EvidenceInstaller
	x := i.Index
	es, err := x.append(ctx, now, i.sign, func() ([]pending, error) {
		oe := x.ownerEntry()
		switch {
		case oe == nil:
			return nil, refuse(http.StatusForbidden, "this farm's owner is not enrolled yet: c4 farm up enrols them")
		case p.TouchFor != oe.Device:
			return nil, refuse(http.StatusForbidden, "the touch key confirms for the owner's key %.16s, and the request names %.16s", oe.Device, p.TouchFor)
		case x.inForce(int64(len(x.entries)), now)[oe.Device] == nil:
			// a removed or lapsed owner key gets no touch key
			return nil, refuse(http.StatusForbidden, "the owner's key %.16s is not in force: a removed key gets no touch key", oe.Device)
		case x.touchKeyFor(oe.Device):
			return nil, refuse(http.StatusConflict, "the owner's key has a touch key already: the installer token enrols the first one only, and a touch on it enrols another")
		case !now.Before(x.touchKeyUntil(oe)):
			return nil, refuse(http.StatusConflict, "the installer token enrols the owner's first touch key only within %s of the owner's enrolment, which ended at %s: acts that need a touch can't be confirmed on this farm until c4 farm touch-key --reopen, at a terminal, opens a fresh window",
				TouchKeyWindow, x.touchKeyUntil(oe).UTC().Format(time.RFC3339))
		}
		if err := x.unseen(device); err != nil {
			return nil, err
		}
		if err := x.touchesFor(p, now); err != nil {
			return nil, err
		}
		rec, err := i.deviceRecord(ctx, p, device, now)
		if err != nil {
			return nil, err
		}
		return []pending{{entry: &Entry{Kind: KindDevice, Device: device, Person: owner, Addr: i.addr(r)}, record: rec}}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	c, err := i.certify(pub, owner, device, es[0], now)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ownerEntry is the index entry that enrolled the owner's key, or nil.
func (x *Index) ownerEntry() *Entry {
	for _, e := range x.entries {
		if d := x.devices[e.Record]; e.Kind == KindDevice && d != nil && d.Predicate.Evidence == schema.EvidenceInstaller && d.Predicate.TouchFor == "" {
			return e
		}
	}
	return nil
}

// touchKeyUntil is when the owner's first-touch-key window ends: within
// TouchKeyWindow of the owner's enrolment, or of the last reopening; a
// time in the past when the entry's time can't be read.
func (x *Index) touchKeyUntil(oe *Entry) time.Time {
	from := oe
	if r := x.reopened(oe.Device); r != nil {
		from = r
	}
	at, err := time.Parse(time.RFC3339, from.At)
	if err != nil {
		return time.Time{}
	}
	return at.Add(TouchKeyWindow)
}

// reopened is the last reopening of device's touch-key window, or nil.
func (x *Index) reopened(device string) *Entry {
	var last *Entry
	for _, e := range x.entries {
		if e.Kind == KindTouchWindow && e.Device == device {
			last = e
		}
	}
	return last
}

// handleTouchReopen opens one fresh TouchKeyWindow for a one-person farm
// owner's first touch key (c4 farm touch-key --reopen, which rotates the
// installer token first and sends the new one): on the installer token,
// from the farm's own machine, only while the owner key never had a touch
// key. The reopening is booked, so the owner's page and the CLI show it.
func (i *Issuer) handleTouchReopen(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), i.now()
	owner := i.owner(ctx)
	switch {
	case !i.local(r):
		fail(w, refuse(http.StatusForbidden, "the touch-key window is reopened from the farm's own machine (c4 farm touch-key --reopen)"))
		return
	case len(i.InstallerToken) == 0 || owner == "":
		fail(w, refuse(http.StatusForbidden, "this is not a one-person farm: a touch key is enrolled on a step-up sign-in"))
		return
	case !i.installer(r):
		fail(w, refuse(http.StatusForbidden, "the touch-key window is reopened with this machine's installer token (%s)", InstallerHeader))
		return
	}
	x := i.Index
	var until time.Time
	_, err := x.append(ctx, now, i.sign, func() ([]pending, error) {
		oe := x.ownerEntry()
		switch {
		case oe == nil:
			return nil, refuse(http.StatusForbidden, "this farm's owner is not enrolled yet: c4 farm up enrols them")
		case x.inForce(int64(len(x.entries)), now)[oe.Device] == nil:
			return nil, refuse(http.StatusForbidden, "the owner's key %.16s is not in force: a removed key gets no touch key", oe.Device)
		case x.touchKeyFor(oe.Device):
			return nil, refuse(http.StatusConflict, "the owner's key has a touch key already: a touch on it enrols another")
		}
		until = now.Add(TouchKeyWindow)
		return []pending{{entry: &Entry{Kind: KindTouchWindow, Device: oe.Device, Person: owner, Addr: i.addr(r)}}}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, TouchWindow{Until: until.UTC().Format(time.RFC3339)})
}

// TouchWindow answers a reopening: when the fresh window ends.
type TouchWindow struct {
	Until string `json:"until"`
}

// touchKeyFor reports whether a touch key was ever enrolled for device.
func (x *Index) touchKeyFor(device string) bool {
	for _, e := range x.entries {
		if d := x.devices[e.Record]; e.Kind == KindDevice && d != nil && d.Predicate.TouchFor == device {
			return true
		}
	}
	return false
}

// CodeHash is how a one-time code is booked and read back: the SHA-256 of
// the code as the CLI made it, hex.
func CodeHash(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// handleBrowser enrols the browser the owner's CLI opened with a one-time
// code (evidence owner): within CodeTTL of the code's
// booking, once.
func (i *Issuer) handleBrowser(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), i.now()
	if !i.local(r) {
		fail(w, refuse(http.StatusForbidden, "a one-time code works on the farm's own machine: open the HUD there (c4 farm open)"))
		return
	}
	var e Enrolment
	if err := decode(r, &e); err != nil {
		fail(w, err)
		return
	}
	if e.Code == "" || e.IDToken != "" || e.Approval != "" {
		fail(w, refuse(http.StatusBadRequest, "a browser presents the one-time code it was opened with (code), and nothing else"))
		return
	}
	// A code enrols the browser's device key, never a touch key: a touch
	// key comes only from a step-up sign-in, c4 farm up, or a touch on
	// another of the person's touch keys (PEOPLE-KEYS-2 4.3), or anything
	// that can use the browser key could enrol a second key that never
	// asks, and sign confirmed acts with the two.
	if e.Request.TouchFor != "" {
		fail(w, refuse(http.StatusForbidden, "a one-time code enrols a device key, not a touch key: a touch key is enrolled on a step-up sign-in, at c4 farm up, or on a touch from another touch key"))
		return
	}
	p, pub, device, err := i.predicate(&e, now)
	if err != nil {
		fail(w, err)
		return
	}
	hash := CodeHash(e.Code)
	p.Evidence, p.Code = schema.EvidenceOwner, hash
	x := i.Index
	es, err := x.append(ctx, now, i.sign, func() ([]pending, error) {
		c := x.code(hash)
		switch {
		case c == nil:
			return nil, refuse(http.StatusForbidden, "that code is not this farm's: open the HUD with c4 farm open")
		case now.After(c.expires()):
			return nil, refuse(http.StatusForbidden, "that code lapsed at %s: a code works for %s; run c4 farm open again", c.Expires, CodeTTL)
		case x.codeUsed(hash) != nil:
			return nil, refuse(http.StatusForbidden, "that code enrolled a browser already: run c4 farm open again")
		case p.Person != c.Person:
			return nil, refuse(http.StatusForbidden, "that code is %s's, and the request is %s's", c.Person, p.Person)
		}
		if err := x.unseen(device); err != nil {
			return nil, err
		}
		rec, err := i.deviceRecord(ctx, p, device, now)
		if err != nil {
			return nil, err
		}
		return []pending{{entry: &Entry{Kind: KindDevice, Device: device, Person: p.Person, Addr: i.addr(r)}, record: rec}}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	c, err := i.certify(pub, p.Person, device, es[0], now)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// CodeState is what the CLI reads back for its code: waiting, enrolled
// (with the key the code enrolled, which the CLI compares with the one it
// saw the browser make) or lapsed.
type CodeState struct {
	State    string `json:"state"`
	Expires  string `json:"expires"`
	Device   string `json:"device,omitempty"`
	Record   string `json:"record,omitempty"`
	Position int64  `json:"position,omitempty"`
	Key      string `json:"key,omitempty"`
	Label    string `json:"label,omitempty"`
}

// handleCode answers GET /v1/people/codes/{sha256 of the code}.
func (i *Issuer) handleCode(w http.ResponseWriter, r *http.Request) {
	if _, err := caller(r); err != nil && !i.installer(r) {
		fail(w, refuse(http.StatusUnauthorized, "a code is read back with this machine's installer token (%s), or signed in", InstallerHeader))
		return
	}
	hash := r.PathValue("code")
	x := i.Index
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(r.Context()); err != nil {
		fail(w, err)
		return
	}
	c := x.code(hash)
	if c == nil {
		fail(w, refuse(http.StatusNotFound, "no code %.16s is booked", hash))
		return
	}
	out := CodeState{State: "waiting", Expires: c.Expires}
	if e := x.codeUsed(hash); e != nil {
		d := x.devices[e.Record]
		out = CodeState{State: "enrolled", Expires: c.Expires, Device: e.Device, Record: e.Record, Position: e.Position, Key: d.Predicate.Key, Label: d.Predicate.Label}
	} else if i.now().After(c.expires()) {
		out.State = "lapsed"
	}
	writeJSON(w, http.StatusOK, out)
}

// code is the booked code with this hash.
func (x *Index) code(hash string) *Entry {
	for _, e := range x.all() {
		if e.Kind == KindCode && e.Code == hash {
			return e
		}
	}
	return nil
}

// codeUsed is the device entry a code enrolled, or nil.
func (x *Index) codeUsed(hash string) *Entry {
	for _, e := range x.all() {
		if d := x.devices[e.Record]; e.Kind == KindDevice && d != nil && d.Predicate.Code == hash && d.Predicate.Renews == "" {
			return e
		}
	}
	return nil
}
