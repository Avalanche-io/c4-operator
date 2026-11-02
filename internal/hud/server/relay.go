package server

// Relayed decisions (internal/hud/API.md). A person who
// approves in a tracker never signed in to the HUD; the tracker's sync
// relays what they decided. The HUD takes it only from the sync, only for
// a job of that Tracker, and only from a tracker user the Tracker's people
// map makes the person the wait is for (or, for a party's wait, a user it
// maps to that party). The decision is signed with its custody: relayed, or
// relayed-party when the map names a party rather than a person.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Avalanche-io/c4"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Custody grades of a decision.
const (
	CustodyDirect       = "direct"
	CustodyRelayed      = "relayed"
	CustodyRelayedParty = "relayed-party"
	// CustodyStandIn: someone the farm and the project let stand in
	// decided a person's wait for them (whomay.go).
	CustodyStandIn = "stand-in"
)

// relayed checks a relayed decision on wait and returns its custody
// grade, whom the decision is by, its chain, and what the relay's signed
// act must say to be taken for it (relayCheck): by the tracker's user,
// for the person the people map names, under that person's confirmation
// of the login (people.go). An unconfirmed login's decision still goes
// through, graded unattributed, and the record says what is missing
// (PEOPLE-KEYS-2 6.1; refused from K8).
func (s *Server) relayed(ctx context.Context, v *api.Viewer, rec *jobRec, wait *c4v1.WorkWait, via *api.Relay) (string, string, *schema.Chain, *relayCheck, error) {
	refuse := func(format string, a ...any) error { return &errGrant{http.StatusForbidden, fmt.Sprintf(format, a...)} }
	if !s.isSync(v) {
		return "", "", nil, nil, refuse("only a tracker's sync relays decisions; a person decides as themselves")
	}
	if via.Tracker == "" || via.By == "" || via.Person == "" || via.Source == "" {
		return "", "", nil, nil, &errGrant{http.StatusBadRequest, "a relayed decision names its tracker, source, the tracker user (by) and whom the tracker maps them to (person)"}
	}
	if via.Evidence != "" {
		if _, err := c4.Parse(via.Evidence); err != nil {
			return "", "", nil, nil, &errGrant{http.StatusBadRequest, fmt.Sprintf("evidence %q is not a C4 ID", via.Evidence)}
		}
	}
	if got := rec.job.Labels[c4v1.LabelTracker]; got != via.Tracker {
		return "", "", nil, nil, refuse("job %s is not tracker %s's work", rec.job.Name, via.Tracker)
	}
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: rec.job.Namespace, Name: via.Tracker}, &tr); err != nil {
		if noGrants(err) {
			return "", "", nil, nil, &errGrant{http.StatusNotImplemented, "this farm has no trackers installed"}
		}
		return "", "", nil, nil, refuse("no tracker %s in %s", via.Tracker, rec.job.Namespace)
	}
	mapped, ok := tr.Spec.People[via.By]
	if !ok || mapped != via.Person {
		return "", "", nil, nil, refuse("tracker %s does not map @%s to %s", via.Tracker, via.By, via.Person)
	}
	grade := CustodyRelayed
	if strings.HasPrefix(mapped, "party:") {
		grade = CustodyRelayedParty
	}
	if !strings.HasPrefix(wait.Server, "person:") && !strings.HasPrefix(wait.Server, "party:") {
		return "", "", nil, nil, refuse("the wait is for %s, which a tracker cannot serve", wait.Server)
	}
	// The person the map names decides as a person signed in would
	// (whomay.go); a party the map names speaks as a member of it.
	who := Decider{User: mapped}
	if p, ok := strings.CutPrefix(mapped, "party:"); ok {
		who = Decider{User: mapped, Parties: []string{p}}
	}
	if err := MayControl("decide", wait.Server, who); err != nil {
		return "", "", nil, nil, refuse("@%s is %s: %v", via.By, mapped, err)
	}
	chain := &schema.Chain{Act: "decide", By: schema.Actor{Tracker: via.Tracker, User: via.By, Grade: schema.GradeRelayed},
		For: schema.For{Party: strings.TrimPrefix(mapped, "party:"), Grade: schema.GradeRelayed}}
	check := &relayCheck{tracker: via.Tracker, user: via.By, person: strings.TrimPrefix(mapped, "party:"), caller: v.User}
	if sync := tr.Spec.Sync; sync != nil {
		check.signer = provenance.ServiceAccountIdentity(sync.Namespace, sync.ServiceAccount)
	}
	if tp := s.confirmedLogin(ctx, rec.job.Namespace, via.Tracker, via.By, mapped); tp != nil {
		d := schema.Delegation{Kind: "people-map", Record: tp.Spec.Accepted.Record}
		chain.Under, chain.Accountable = []schema.Delegation{d}, true
		check.confirmation = tp.Spec.Accepted.Record
	} else {
		// No one confirmed the login is the person: the tracker's word
		// alone names them, so the decision is no one's the farm knows.
		grade = CustodyUnattributed
		chain.Gap = fmt.Sprintf("%s has not yet confirmed that the login %s on tracker %s is them", whoIs(mapped), via.By, via.Tracker)
	}
	check.grade = grade
	return grade, fmt.Sprintf("%s via tracker %s (@%s)", mapped, via.Tracker, via.By), chain, check, nil
}

// relayCheck is what a relay's signed act must say to be taken for the
// decision it carries (PEOPLE-KEYS-2 2.2, 6.1): signed by the sync its
// Tracker names (spec.sync), which is the caller, by the tracker user,
// for the person the people map names, under that person's confirmation
// of the login when there is one. relayed() makes it; citeAct takes a
// relay's act only with it, so no act is graded relayed around its checks.
type relayCheck struct {
	tracker, user, person string
	// signer is the identity of the Tracker's sync, "" when it names
	// none; caller is the user the request came as.
	signer, caller string
	// confirmation is the people-map record of the person's confirmation,
	// "" while the login is unconfirmed; grade is the decision's custody.
	confirmation, grade string
}

type relayKey struct{}

// withRelay carries a relayed decision's check to citeAct.
func withRelay(ctx context.Context, c *relayCheck) context.Context {
	return context.WithValue(ctx, relayKey{}, c)
}

func relayOf(ctx context.Context) *relayCheck {
	c, _ := ctx.Value(relayKey{}).(*relayCheck)
	return c
}

// check is a relay's act against the decision: its signer and its chain.
func (c *relayCheck) check(identity string, ch schema.ActChain) error {
	switch {
	case c.signer == "":
		return fmt.Errorf("tracker %s names no sync (spec.sync), so no relay signs for it", c.tracker)
	case identity != c.signer:
		return fmt.Errorf("is signed by %s, and tracker %s's sync is %s", identity, c.tracker, c.signer)
	case strings.HasPrefix(c.caller, "system:serviceaccount:") && serviceAccountIdentity(c.caller) != c.signer:
		return fmt.Errorf("is %s's, and %s sent it", c.signer, c.caller)
	case ch.By.Tracker != c.tracker || ch.By.User != c.user:
		return fmt.Errorf("is by @%s on tracker %s, and this decision by @%s on %s", ch.By.User, ch.By.Tracker, c.user, c.tracker)
	case ch.For.Party != c.person:
		return fmt.Errorf("is for %s, and tracker %s maps @%s to %s", ch.For.Party, c.tracker, c.user, c.person)
	}
	for _, d := range ch.Under {
		if d.Kind == "people-map" && d.Record != c.confirmation {
			return fmt.Errorf("names the confirmation %s, and the farm's is %q", d.Record, c.confirmation)
		}
	}
	return nil
}

// serviceAccountIdentity is a service account user's signing identity
// (system:serviceaccount:<ns>:<account>, sa/<ns>/<account>).
func serviceAccountIdentity(user string) string {
	ns, account, _ := strings.Cut(strings.TrimPrefix(user, "system:serviceaccount:"), ":")
	return provenance.ServiceAccountIdentity(ns, account)
}
