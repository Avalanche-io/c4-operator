package people_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// up enrols noor's CLI key and touch key as c4 farm up does: from the
// farm's own machine, with the installer token.
func (r *rig) up(cli, touch *dev) (int, string, *people.OwnerCertified) {
	req := people.OwnerEnrolment{Owner: r.enrolment(cli, "noor", "", "mac")}
	if touch != nil {
		te := r.enrolment(touch, "noor", cli.id, "mac")
		req.Touch = &te
	}
	var out people.OwnerCertified
	code, body := r.call("POST", "/v1/people/owner", local, req, &out, people.InstallerHeader, installerToken)
	return code, body, &out
}

// The owner of a one-person farm is enrolled once, on the installer
// token: the owner's key and its touch key, evidence installer. The path
// is refused once an owner key exists, without the token, and, where the
// HUD listens only on a loopback address, from another machine.
func TestInstallerPathOnce(t *testing.T) {
	r := onePerson(t)
	cli, touch := newDev(t), newDev(t)
	req := people.OwnerEnrolment{Owner: r.enrolment(cli, "noor", "", "mac")}
	if code, _ := r.call("POST", "/v1/people/owner", "", req, nil, people.InstallerHeader, installerToken); code != http.StatusForbidden {
		t.Errorf("from another machine: %d", code)
	}
	if code, _ := r.call("POST", "/v1/people/owner", local, req, nil, people.InstallerHeader, strings.Repeat("0", 64)); code != http.StatusForbidden {
		t.Errorf("another token: %d", code)
	}
	code, body, out := r.up(cli, touch)
	if code != http.StatusOK || out.Owner.Device != cli.id || out.Touch == nil || out.Touch.Device != touch.id {
		t.Fatalf("up: %d %s", code, body)
	}
	devs := r.entries(people.KindDevice)
	if len(devs) != 2 {
		t.Fatalf("%d device records", len(devs))
	}
	if code, body, _ := r.up(newDev(t), nil); code != http.StatusConflict || !strings.Contains(body, "enrolled already") {
		t.Errorf("the installer path a second time: %d %s", code, body)
	}
	var cfg people.Config
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if !cfg.OwnerEnrolled || cfg.Owner != "noor" || cfg.Provider != nil {
		t.Errorf("config %+v", cfg)
	}
	// With no provider, the owner's key renews on its own proof.
	r.now = r.now.Add(6 * time.Hour)
	if code, body := r.call("POST", "/v1/people/renew", "", r.enrolment(cli, "noor", "", "mac"), nil); code != http.StatusOK {
		t.Errorf("the owner's key renews: %d %s", code, body)
	}
}

// browserCode books a one-time code with noor's CLI key.
func (r *rig) browserCode(cli *dev, c *people.Certified, code string) (int, string) {
	k := r.key(cli, c)
	status, body, _ := r.book(r.act(k, "noor", "browser-code", r.farmSubject(), map[string]string{"code": people.CodeHash(code)}, false), local)
	return status, body
}

// The owner's key enrols the browser: the CLI books a one-time code, the
// browser presents it with its key within a minute, the CLI reads back
// the key its code enrolled and countersigns it. A code used after its
// minute, from another machine where the HUD listens only on a loopback
// address, or a second time is refused,
// and only the key the installer path enrolled books a code.
func TestBrowserCode(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	_, _, owner := r.up(cli, nil)
	code := codeOf(t)
	if status, body := r.browserCode(cli, owner.Owner, code); status != http.StatusOK {
		t.Fatalf("book the code: %d %s", status, body)
	}
	browser := newDev(t)
	e := r.enrolment(browser, "noor", "", "browser")
	e.Code = code
	if status, _ := r.call("POST", "/v1/people/browser", "", e, nil); status != http.StatusForbidden {
		t.Errorf("from another machine: %d", status)
	}
	var c people.Certified
	if status, body := r.call("POST", "/v1/people/browser", local, e, &c, "Origin", "http://"+local); status != http.StatusOK {
		t.Fatalf("enrol the browser: %d %s", status, body)
	}
	var st people.CodeState
	r.call("GET", "/v1/people/codes/"+people.CodeHash(code), "", nil, &st)
	if st.State != "enrolled" || st.Device != browser.id || st.Key != base64.RawURLEncoding.EncodeToString(browser.spki) {
		t.Fatalf("read back %+v", st)
	}
	again := r.enrolment(newDev(t), "noor", "", "browser")
	again.Code = code
	if status, body := r.call("POST", "/v1/people/browser", local, again, nil); status != http.StatusForbidden || !strings.Contains(body, "already") {
		t.Errorf("a code used twice: %d %s", status, body)
	}
	k := r.key(cli, owner.Owner)
	status, body, b := r.book(r.act(k, "noor", "countersign-device", deviceSubject(browser.id), map[string]string{"record": st.Record}, false), local)
	if status != http.StatusOK || b.Entry.Kind != people.KindCountersign || b.Entry.Of != st.Record {
		t.Fatalf("countersign: %d %s", status, body)
	}
	var list struct{ Devices []people.DeviceView }
	r.call("GET", "/v1/people/devices?person=noor", "", nil, &list)
	if len(list.Devices) != 2 || !list.Devices[1].Countersigned || list.Devices[1].Evidence != schema.EvidenceOwner {
		t.Errorf("devices %+v", list.Devices)
	}
	// The browser's key books no code of its own.
	kb := r.key(browser, &c)
	if status, _, _ := r.book(r.act(kb, "noor", "browser-code", r.farmSubject(), map[string]string{"code": people.CodeHash(codeOf(t))}, false), local); status != http.StatusForbidden {
		t.Errorf("the browser booked a code: %d", status)
	}
	// A code works for a minute.
	late := codeOf(t)
	r.browserCode(cli, owner.Owner, late)
	r.now = r.now.Add(people.CodeTTL + time.Second)
	el := r.enrolment(newDev(t), "noor", "", "browser")
	el.Code = late
	if status, body := r.call("POST", "/v1/people/browser", local, el, nil); status != http.StatusForbidden || !strings.Contains(body, "lapsed") {
		t.Errorf("a code after its minute: %d %s", status, body)
	}
}

// A one-time code enrols the browser's device key, never a touch key
// (review probes P1 and P1b). Otherwise a page holding the browser key
// would enrol a second silent key as its touch key, and the two would
// sign a confirmed grant CheckAct takes. touchFor is refused whatever it
// names: a device of the person's, or the owner's own touch key.
func TestBrowserCodeEnrolsNoTouchKey(t *testing.T) {
	r := onePerson(t)
	cli, touch := newDev(t), newDev(t)
	_, _, owner := r.up(cli, touch)
	code1 := codeOf(t)
	r.browserCode(cli, owner.Owner, code1)
	b1 := newDev(t)
	e1 := r.enrolment(b1, "noor", "", "browser")
	e1.Code = code1
	var c1 people.Certified
	if s, b := r.call("POST", "/v1/people/browser", local, e1, &c1); s != http.StatusOK {
		t.Fatalf("browser 1: %d %s", s, b)
	}
	for _, touchFor := range []string{b1.id, touch.id, cli.id} {
		code := codeOf(t)
		r.browserCode(cli, owner.Owner, code)
		e := r.enrolment(newDev(t), "noor", touchFor, "browser")
		e.Code = code
		if s, body := r.call("POST", "/v1/people/browser", local, e, nil); s != http.StatusForbidden || !strings.Contains(body, "not a touch key") {
			t.Errorf("a code enrolled a touch key for %.12s: %d %s", touchFor, s, body)
		}
	}
	if n := len(r.entries(people.KindDevice)); n != 3 {
		t.Errorf("%d device records, want the owner's two and one browser", n)
	}
}

// A touch key states hardware, or is a passkey: a key file, or a browser
// key that isn't a passkey, is refused as a touch key unless the farm
// takes software touch keys (a test or file-key farm). A device key may be
// software; only the touch is held to this.
func TestTouchKeyStatesHardwareOrIsAPasskey(t *testing.T) {
	enrol := func(r *rig, owner, touch string) int {
		cli, tk := newDev(t), newDev(t)
		oe, te := r.enrolment(cli, "noor", "", "mac"), r.enrolment(tk, "noor", cli.id, "mac")
		oe.Protection, te.Protection = owner, touch
		code, _ := r.call("POST", "/v1/people/owner", local, people.OwnerEnrolment{Owner: oe, Touch: &te}, nil, people.InstallerHeader, installerToken)
		return code
	}
	for _, p := range []string{schema.ProtectionFile, schema.ProtectionBrowser} {
		if code := enrol(onePerson(t), schema.ProtectionHardware, p); code != http.StatusForbidden {
			t.Errorf("a %s touch key: %d, want 403", p, code)
		}
	}
	if code := enrol(onePerson(t), schema.ProtectionFile, schema.ProtectionHardware); code != http.StatusOK {
		t.Errorf("a software device key with a hardware touch key: %d", code)
	}
	r := onePerson(t)
	r.iss.SoftwareTouchKeys = true
	if code := enrol(r, schema.ProtectionFile, schema.ProtectionFile); code != http.StatusOK {
		t.Errorf("a key-file touch key on a farm that takes them: %d", code)
	}
	var cfg people.Config
	if code, body := r.call("GET", "/v1/people/config", local, nil, &cfg); code != http.StatusOK || !cfg.SoftwareTouchKeys {
		t.Errorf("the config says softwareTouchKeys: %d %s", code, body)
	}
}

// c4 farm touch-key: an owner enrolled with no touch key (up with no one
// at the terminal) enrols their first one on the installer token, from
// the farm's own machine, for the owner's key; once one was ever enrolled
// the route answers 409. Before the owner is enrolled, for another key,
// without the token or from another machine, it is refused.
func TestOwnersFirstTouchKey(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	touch := func(d *dev, touchFor, host, token string) (int, string, *people.Certified) {
		var out people.Certified
		code, body := r.call("POST", "/v1/people/owner/touch", host, r.enrolment(d, "noor", touchFor, "mac"), &out, people.InstallerHeader, token)
		return code, body, &out
	}
	if code, body, _ := touch(newDev(t), cli.id, local, installerToken); code != http.StatusForbidden || !strings.Contains(body, "not enrolled yet") {
		t.Errorf("before the owner: %d %s", code, body)
	}
	if code, body, _ := r.up(cli, nil); code != http.StatusOK {
		t.Fatalf("up: %d %s", code, body)
	}
	var cfg people.Config
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if until, err := time.Parse(time.RFC3339, cfg.TouchKeyUntil); err != nil || !until.Equal(r.now.Add(people.TouchKeyWindow)) {
		t.Errorf("the window's end: %q, want %s", cfg.TouchKeyUntil, r.now.Add(people.TouchKeyWindow))
	}
	tk := newDev(t)
	for name, c := range map[string]func() int{
		"from another machine": func() int { code, _, _ := touch(tk, cli.id, "", installerToken); return code },
		"another token":        func() int { code, _, _ := touch(tk, cli.id, local, strings.Repeat("0", 64)); return code },
		"for another key":      func() int { code, _, _ := touch(tk, newDev(t).id, local, installerToken); return code },
	} {
		if code := c(); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, body, _ := touch(tk, "", local, installerToken); code != http.StatusBadRequest {
		t.Errorf("a device key, not a touch key: %d %s", code, body)
	}
	code, body, out := touch(tk, cli.id, local, installerToken)
	if code != http.StatusOK || out.Device != tk.id {
		t.Fatalf("the first touch key: %d %s", code, body)
	}
	if code, body, _ := touch(newDev(t), cli.id, local, installerToken); code != http.StatusConflict || !strings.Contains(body, "touch key already") {
		t.Errorf("a second: %d %s", code, body)
	}
	cfg = people.Config{}
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if cfg.TouchKeyUntil != "" {
		t.Errorf("a window with a touch key: %q", cfg.TouchKeyUntil)
	}
}

// The installer token enrols the owner's first touch key only within
// TouchKeyWindow of the owner's enrolment; after that, 409, with why.
func TestTouchKeyWindowCloses(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	r.up(cli, nil)
	r.now = r.now.Add(people.TouchKeyWindow)
	// the owner's key stays in force: renewed on its own proof
	if code, body := r.call("POST", "/v1/people/renew", "", r.enrolment(cli, "noor", "", "mac"), nil); code != http.StatusOK {
		t.Fatalf("renewing: %d %s", code, body)
	}
	code, body := r.call("POST", "/v1/people/owner/touch", local, r.enrolment(newDev(t), "noor", cli.id, "mac"), nil, people.InstallerHeader, installerToken)
	if code != http.StatusConflict || !strings.Contains(body, "can't be confirmed on this farm") {
		t.Errorf("after the window: %d %s", code, body)
	}
}

// A removed owner key gets no touch key on the installer token: its
// certificate lives on, but the route needs the key in force.
func TestNoTouchKeyForARemovedOwnerKey(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	_, _, owner := r.up(cli, nil)
	if code, body, _ := r.book(r.act(r.key(cli, owner.Owner), "noor", "remove-device", deviceSubject(cli.id), nil, false), local); code != http.StatusOK {
		t.Fatalf("removing the owner key: %d %s", code, body)
	}
	code, body := r.call("POST", "/v1/people/owner/touch", local, r.enrolment(newDev(t), "noor", cli.id, "mac"), nil, people.InstallerHeader, installerToken)
	if code != http.StatusForbidden || !strings.Contains(body, "not in force") {
		t.Errorf("a touch key for a removed owner key: %d %s", code, body)
	}
}
