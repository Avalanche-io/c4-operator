package farmbook_test

import (
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
)

// A Party's memberships are in force by the book's one rule: an
// organisation's first make-party opens its first admin's membership and
// the party itself, a second make-party of the name opens neither, an
// add-member opens a person's or a group's, set-purposes opens it again,
// end-member closes it, and one key never opens another.
func TestPartyMembershipsInForce(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var es []*farmbook.Sealed
	book := func(act, subject, person string) *farmbook.Sealed {
		s := &farmbook.Sealed{Entry: &farmbook.Entry{Kind: farmbook.KindControl, Act: act, Subject: subject, Person: person,
			Position: int64(len(es) + 1), Record: "r" + act + subject}, Time: at}
		es = append(es, s)
		return s
	}
	on := func(kind, key string) *farmbook.Sealed {
		t.Helper()
		s, err := farmbook.InForceAt(es, kind, key, at)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	kestrel := farmbook.PartyKey("kestrel")
	dana, noor := farmbook.MemberKey("kestrel", "dana"), farmbook.MemberKey("kestrel", "noor")
	sups := farmbook.GroupKey("kestrel", "lighting-sups")
	if on(farmbook.ForceParty, kestrel) != nil || on(farmbook.ForceMembership, dana) != nil {
		t.Fatal("in force before anything is booked")
	}
	made := book("make-party", kestrel, "dana")
	if on(farmbook.ForceParty, kestrel) != made {
		t.Error("make-party does not put the party in force")
	}
	if on(farmbook.ForceMembership, dana) != made {
		t.Error("make-party does not make its first admin a member")
	}
	if on(farmbook.ForceMembership, farmbook.MemberKey("saltmarsh", "dana")) != nil {
		t.Error("kestrel's first admin is a member of saltmarsh")
	}
	// A second make-party of the name: never a second first admin.
	again := book("make-party", kestrel, "noor")
	if on(farmbook.ForceMembership, noor) != nil || on(farmbook.ForceParty, kestrel) != made {
		t.Errorf("a second make-party counts: noor %v, party %v", on(farmbook.ForceMembership, noor), on(farmbook.ForceParty, kestrel) == again)
	}
	added := book("add-member", noor, "noor")
	if on(farmbook.ForceMembership, noor) != added {
		t.Error("add-member does not open noor's membership")
	}
	changed := book("set-purposes", noor, "noor")
	if on(farmbook.ForceMembership, noor) != changed {
		t.Error("set-purposes does not open it again, with its purposes")
	}
	group := book("add-member", sups, "")
	if on(farmbook.ForceMembership, sups) != group || on(farmbook.ForceMembership, dana) != made {
		t.Error("a group's membership is not its own key")
	}
	book("end-member", noor, "noor")
	if on(farmbook.ForceMembership, noor) != nil {
		t.Error("end-member does not close noor's membership")
	}
	if on(farmbook.ForceMembership, sups) != group || on(farmbook.ForceMembership, dana) != made {
		t.Error("ending noor's membership closed another")
	}
	book("end-member", dana, "dana")
	if on(farmbook.ForceMembership, dana) != nil {
		t.Error("end-member does not close the first admin's membership")
	}
	if on(farmbook.ForceParty, kestrel) != made {
		t.Error("a party ends with its members")
	}
}
