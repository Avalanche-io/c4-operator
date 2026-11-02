// Package unitact is how a unit signs what it reports about itself as
// itself (PEOPLE-KEYS 5.1), for c4-unit and for c4-agent's pod blocks.
package unitact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// A unit signs what it asks, answers, needs, withdraws and waits on as
// itself (PEOPLE-KEYS 5.1): for each, c4-unit makes a key, has the farm's
// workload issuer certify it on the pod's own token as this attempt
// (unit/<ns>/<job>/<unit>@<job UID>/<attempt>), signs the act with it and
// drops it. The key is never written anywhere. The act names the unit's
// own pod, by UID, with the job's spec as it was when the pod was made
// ($C4_SPEC), and is for the job's owner, under the job's ownership. Each
// is appended to .c4/acts, which the node daemon (or the host runner)
// relays to the farm, and the farm checks each control against its act.
//
// A unit on a farm with no issuer, or one the farm gave no key
// ($C4_SIGNER unset, a host runner's unit until its runner keeps keys),
// signs nothing: its controls go as before, on its node's word, and the
// farm books them unsigned.

const (
	// Max is the most acts one attempt may sign: the issuer takes that
	// many certificates from a pod at once.
	Max = 48
	// SeenWait is how long needs and withdraw wait for the farm to have
	// their act before the unit ends.
	SeenWait = 30 * time.Second
)

// Unit is the unit signing: its environment, as the queue gives it to
// every unit pod, and its clock, swappable in tests.
type Unit struct {
	// Getenv reads the unit's environment (default os.Getenv).
	Getenv func(string) string
	Now    func() time.Time
	Sleep  func(time.Duration)
	// Issue certifies the unit's key (default the issuer at $C4_SIGNER,
	// on the token at $C4_SIGNER_TOKEN_FILE).
	Issue provenance.IssueFunc
}

func (u Unit) get(k string) string {
	if u.Getenv != nil {
		return u.Getenv(k)
	}
	return os.Getenv(k)
}

// Signs reports whether this unit has a key to sign with.
func (u Unit) Signs() bool { return u.get("C4_SIGNER") != "" || u.Issue != nil }

// Sign signs act with params as this unit and appends it to the
// unit's .c4/acts under out, returning its place there (from 1), or 0
// when the unit has no key.
func (u Unit) Sign(out, act string, params map[string]any) (int, error) {
	if !u.Signs() {
		return 0, nil
	}
	path := filepath.Join(out, ".c4", "acts")
	var acts []json.RawMessage
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &acts); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	if len(acts) >= Max {
		return 0, fmt.Errorf("this attempt has signed %d acts, the most one may", Max)
	}
	missing := []string{}
	for _, k := range []string{"C4_NAMESPACE", "C4_JOB", "C4_JOB_UID", "C4_POD", "C4_POD_UID", "C4_SPEC", "C4_OWNER"} {
		if u.get(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("the unit cannot sign: %s not set", strings.Join(missing, ", "))
	}
	issue := u.Issue
	if issue == nil {
		file := u.get("C4_SIGNER_TOKEN_FILE")
		issue = provenance.RemoteIssuer(u.get("C4_SIGNER"), func() (string, error) {
			b, err := os.ReadFile(file)
			if err != nil || strings.TrimSpace(string(b)) == "" {
				return "", fmt.Errorf("no issuer token at %s", file)
			}
			return strings.TrimSpace(string(b)), nil
		}, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := provenance.NewSigner(ctx, issue)
	if err != nil {
		return 0, fmt.Errorf("the unit's key: %w", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	if u.Now != nil {
		now = u.Now()
	}
	subject := provenance.ActSubject("pod", u.get("C4_NAMESPACE"), u.get("C4_POD"), u.get("C4_POD_UID"), u.get("C4_SPEC"))
	b, err := provenance.SignUnitAct(ctx, s, provenance.UnitAct{Subject: subject, Act: act, Params: raw, Owner: u.get("C4_OWNER")}, now)
	if err != nil {
		return 0, err
	}
	acts = append(acts, b)
	data, err := json.Marshal(acts)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	return len(acts), writeAtomic(path, data)
}

// AwaitSeen waits until the farm has the unit's acts up to n
// ($C4_CONTROL/acts.seen, written by the relay), at most wait. It reports
// whether it has.
func (u Unit) AwaitSeen(n int, wait time.Duration) bool {
	dir := u.get("C4_CONTROL")
	if dir == "" || n <= 0 {
		return false
	}
	now, sleep := time.Now, time.Sleep
	if u.Now != nil {
		now = u.Now
	}
	if u.Sleep != nil {
		sleep = u.Sleep
	}
	until := now().Add(wait)
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "acts.seen")); err == nil {
			if seen, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && seen >= n {
				return true
			}
		}
		if !now().Before(until) {
			return false
		}
		sleep(200 * time.Millisecond)
	}
}

// Report signs a report the unit ends on (needs, withdraw) and waits
// for the farm to have it, saying on w when it can't: unsigned, or
// signed and not yet relayed. The report still goes; the farm decides.
func (u Unit) Report(out, act string, params map[string]any, w io.Writer) {
	n, err := u.Sign(out, act, params)
	switch {
	case err != nil:
		fmt.Fprintf(w, "c4-unit: this %s could not be signed (%v): the farm gets it unsigned, and from K8 refuses it\n", act, err)
	case n == 0:
	case !u.AwaitSeen(n, SeenWait):
		fmt.Fprintf(w, "c4-unit: this %s was signed, and the farm did not confirm having it within %s: if it never arrives, the farm treats it as unsigned\n", act, SeenWait)
	}
}

// Note signs a control that doesn't end the unit (ask, answer, wait)
// before its note is written, so the relay, which carries acts first,
// never shows the farm a note whose act is still to come. One that can't
// be signed goes unsigned, said on w.
func (u Unit) Note(out, act string, params map[string]any, w io.Writer) {
	if _, err := u.Sign(out, act, params); err != nil {
		fmt.Fprintf(w, "c4-unit: this %s could not be signed (%v): the farm gets it unsigned, and from K8 refuses it\n", act, err)
	}
}

// NextNoteSeq is the number the next note in a unit's list file will
// have (appendNote).
func NextNoteSeq(path string) int {
	var notes []json.RawMessage
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &notes) == nil {
		return len(notes) + 1
	}
	return 1
}

func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
