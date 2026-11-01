package peopletest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
)

// post sends body to the rig's server over HTTP, as a client outside the
// process does.
func post(t *testing.T, f *peopletest.Farm, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(f.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// A studio farm's server enrols a browser on a token from its fake
// provider's /sso/token, and a touch key on a step-up one, over HTTP.
func TestStudioEnrolsOverHTTP(t *testing.T) {
	f := peopletest.Studio(t)
	d := peopletest.NewDevice(t)
	token := func(nonce string, stepUp bool) string {
		q := url.Values{"sub": {peopletest.DanaSub}, "nonce": {nonce}}
		if stepUp {
			q.Set("stepUp", "1")
		}
		resp, err := http.Get(f.Server.URL + "/sso/token?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ IDToken string }
		json.NewDecoder(resp.Body).Decode(&out)
		return out.IDToken
	}
	e := f.Enrolment(t, d, "dana", "", "browser")
	e.IDToken = token(d.ID, false)
	var c people.Certified
	if code := post(t, f, "/v1/people/enroll", e, &c); code != http.StatusOK || c.Identity != provenance.UserIdentity("dana") || c.Farm != f.ID() {
		t.Fatalf("enrol: %d %+v", code, c)
	}
	touch := peopletest.NewDevice(t)
	et := f.Enrolment(t, touch, "dana", d.ID, "mac")
	et.IDToken = token(touch.ID, true)
	if code := post(t, f, "/v1/people/enroll", et, nil); code != http.StatusOK {
		t.Fatalf("touch key: %d", code)
	}
}

// A one-person farm's server enrols the browser a fresh code opened, over
// HTTP, and reads the code back as enrolled.
func TestOnePersonEnrolsABrowserOverHTTP(t *testing.T) {
	o := peopletest.OnePerson(t)
	if o.Owner.Cert == nil || o.Touch.Cert == nil {
		t.Fatal("the owner's keys are not enrolled")
	}
	code := o.NewCode(t)
	b := peopletest.NewDevice(t)
	e := o.Enrolment(t, b, "noor", "", "browser")
	e.Code = code
	if status := post(t, o.Farm, "/v1/people/browser", e, nil); status != http.StatusOK {
		t.Fatalf("browser: %d", status)
	}
	resp, err := http.Get(o.URL + "/v1/people/codes/" + people.CodeHash(code))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st people.CodeState
	json.NewDecoder(resp.Body).Decode(&st)
	if st.State != "enrolled" || st.Device != b.ID {
		t.Errorf("read back %+v", st)
	}
}
