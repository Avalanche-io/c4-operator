package farmcli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// A machine is enrolled as someone's by its runner's offer and its
// holder's acceptance, as a device sign-in is: the runner shows a code,
// and the person whose Claude Code sign-in the runner uses gives the farm
// that code, signed in as themselves.

// Enrollment is the farm's answer to an acceptance: the machine's owners
// now, and the signed record of the act.
type Enrollment struct {
	Node    string   `json:"node"`
	Owners  []string `json:"owners"`
	Record  string   `json:"record,omitempty"`
	Message string   `json:"message"`
}

// EnrollConsent refuses an acceptance from inside an agent, as Consent
// refuses an install.
func EnrollConsent(node string) error {
	if InsideAgent() {
		return fmt.Errorf("accepting a machine lets work run on it on your sign-in, so you run it, not an agent: in your own terminal, c4 farm enroll %s CODE", node)
	}
	return nil
}

// EnrollCode is a code as the runner shows it (XXXX-XXXX), from one typed
// in lower case, with a space or without the dash.
func EnrollCode(code string) (string, error) {
	c := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	if len(c) != 8 {
		return "", fmt.Errorf("%q is not a machine's code: its runner shows eight letters, as XXXX-XXXX", code)
	}
	return c[:4] + "-" + c[4:], nil
}

// Enroll accepts node's offer with the code its runner shows, as the
// caller: the machine becomes theirs, or, with forWhom, someone else's
// too, whose jobs then spend the caller's sign-in. Never from inside an
// agent: one that can read the runner's code must not accept the machine
// for its person.
func Enroll(ctx context.Context, c *Client, node, code, forWhom string) (Enrollment, error) {
	if err := EnrollConsent(node); err != nil {
		return Enrollment{}, err
	}
	code, err := EnrollCode(code)
	if err != nil {
		return Enrollment{}, err
	}
	body := map[string]string{"code": code}
	if forWhom != "" {
		body["for"] = forWhom
	}
	var e Enrollment
	if err := c.Do(ctx, "POST", "/nodes/"+url.PathEscape(node)+"/enroll", body, &e); err != nil {
		return Enrollment{}, err
	}
	return e, nil
}
