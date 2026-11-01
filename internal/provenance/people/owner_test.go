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

// up enrols noor's CLI key and touch key as c4 farm up does: over
// loopback, with the installer token.
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

// The owner of a one-person farm is enrolled once, over loopback, on the
// installer token: the owner's key and its touch key, evidence installer.
// The path is refused once an owner key exists, off loopback, and without
// the token.
func TestInstallerPathOnce(t *testing.T) {
	r := onePerson(t)
	cli, touch := newDev(t), newDev(t)
	req := people.OwnerEnrolment{Owner: r.enrolment(cli, "noor", "", "mac")}
	if code, _ := r.call("POST", "/v1/people/owner", "", req, nil, people.InstallerHeader, installerToken); code != http.StatusForbidden {
		t.Errorf("off loopback: %d", code)
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
// browser presents it with its key within a minute over loopback, the
// CLI reads back the key its code enrolled and countersigns it. A code
// used after its minute, from off loopback or a second time is refused,
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
		t.Errorf("off loopback: %d", status)
	}
	if status, _ := r.call("POST", "/v1/people/browser", local, e, nil, "Origin", "https://pages.example"); status != http.StatusForbidden {
		t.Errorf("from another site's page: %d", status)
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
