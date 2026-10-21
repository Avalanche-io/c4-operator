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
// grade, whom the decision is by, and its chain: by the tracker's user,
// for the person the people map names, under that person's confirmation
// of the login (people.go), or marked unaccountable when there is none
// yet. An unconfirmed login's decision still goes through: the record
// says what is missing.
func (s *Server) relayed(ctx context.Context, v *api.Viewer, rec *jobRec, wait *c4v1.WorkWait, via *api.Relay) (string, string, *schema.Chain, error) {
	refuse := func(format string, a ...any) error { return &errGrant{http.StatusForbidden, fmt.Sprintf(format, a...)} }
	if !s.isSync(v) {
		return "", "", nil, refuse("only a tracker's sync relays decisions; a person decides as themselves")
	}
	if via.Tracker == "" || via.By == "" || via.Person == "" || via.Source == "" {
		return "", "", nil, &errGrant{http.StatusBadRequest, "a relayed decision names its tracker, source, the tracker user (by) and whom the tracker maps them to (person)"}
	}
	if via.Evidence != "" {
		if _, err := c4.Parse(via.Evidence); err != nil {
			return "", "", nil, &errGrant{http.StatusBadRequest, fmt.Sprintf("evidence %q is not a C4 ID", via.Evidence)}
		}
	}
	if got := rec.job.Labels[c4v1.LabelTracker]; got != via.Tracker {
		return "", "", nil, refuse("job %s is not tracker %s's work", rec.job.Name, via.Tracker)
	}
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: rec.job.Namespace, Name: via.Tracker}, &tr); err != nil {
		if noGrants(err) {
			return "", "", nil, &errGrant{http.StatusNotImplemented, "this farm has no trackers installed"}
		}
		return "", "", nil, refuse("no tracker %s in %s", via.Tracker, rec.job.Namespace)
	}
	mapped, ok := tr.Spec.People[via.By]
	if !ok || mapped != via.Person {
		return "", "", nil, refuse("tracker %s does not map @%s to %s", via.Tracker, via.By, via.Person)
	}
	grade := CustodyRelayed
	if strings.HasPrefix(mapped, "party:") {
		grade = CustodyRelayedParty
	}
	if !strings.HasPrefix(wait.Server, "person:") && !strings.HasPrefix(wait.Server, "party:") {
		return "", "", nil, refuse("the wait is for %s, which a tracker cannot serve", wait.Server)
	}
	// The person the map names decides as a person signed in would
	// (whomay.go); a party the map names speaks as a member of it.
	who := Decider{User: mapped}
	if p, ok := strings.CutPrefix(mapped, "party:"); ok {
		who = Decider{User: mapped, Parties: []string{p}}
	}
	if err := MayControl("decide", wait.Server, who); err != nil {
		return "", "", nil, refuse("@%s is %s: %v", via.By, mapped, err)
	}
	chain := &schema.Chain{Act: "decide", By: schema.Actor{Tracker: via.Tracker, User: via.By, Grade: schema.GradeRelayed},
		For: schema.For{Party: strings.TrimPrefix(mapped, "party:"), Grade: schema.GradeRelayed}}
	if tp := s.confirmedLogin(ctx, rec.job.Namespace, via.Tracker, via.By, mapped); tp != nil {
		d := schema.Delegation{Kind: "people-map", Record: tp.Spec.Accepted.Record}
		chain.Under, chain.Accountable = []schema.Delegation{d}, true
	} else {
		chain.Gap = fmt.Sprintf("%s has not yet confirmed that the login %s on tracker %s is them", whoIs(mapped), via.By, via.Tracker)
	}
	return grade, fmt.Sprintf("%s via tracker %s (@%s)", mapped, via.Tracker, via.By), chain, nil
}
