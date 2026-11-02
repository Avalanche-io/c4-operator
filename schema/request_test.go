package schema

import (
	"encoding/json"
	"testing"
)

// A client's statement for a request matches that request alone: the same
// method, the target byte for byte, and the same body.
func TestRequestMatches(t *testing.T) {
	body := []byte(`{"node":"laptop-1","units":2}`)
	st := NewRequest(RequestPredicate{Farm: DeviceID([]byte("a farm CA")), Method: "POST", Path: "/api/v1/observed?since=7", Signer: identityBase + "runner/laptop-1",
		Nonce: "AAAAAAAAAAAAAAAAAAAAAA", SignedAt: "2026-11-01T19:20:05Z"}, body)
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	r, errs := ParseRequest(b)
	if errs != nil {
		t.Fatalf("%+v", errs)
	}
	if err := r.Matches("POST", "/api/v1/observed?since=7", body); err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string]func() error{
		"another method":      func() error { return r.Matches("PUT", "/api/v1/observed?since=7", body) },
		"another query":       func() error { return r.Matches("POST", "/api/v1/observed?since=8", body) },
		"the query dropped":   func() error { return r.Matches("POST", "/api/v1/observed", body) },
		"the path re-encoded": func() error { return r.Matches("POST", "/api/v1/observed?since=%37", body) },
		"another body": func() error {
			return r.Matches("POST", "/api/v1/observed?since=7", []byte(`{"node":"laptop-1","units":3}`))
		},
		"no body for one with body": func() error { return r.Matches("POST", "/api/v1/observed?since=7", nil) },
	} {
		if try() == nil {
			t.Errorf("%s: matched", name)
		}
	}
	if BodyDigest(nil) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("the empty body's digest is %s", BodyDigest(nil))
	}
}
