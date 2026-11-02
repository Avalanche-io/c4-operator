package farmbook

import "time"

// Devices in force: one rule, which the farm book (Book.InForce) and the
// people's index (people.Index.DevicesAt, CheckAct) both use, so the two
// never say different things of one device. Times are the book's: when
// each entry counts from (its seal, or an adopted entry's own time),
// never the writer's clock or an act's own signedAt.

// DeviceEvent is a device entry (an enrolment or a renewal) or a removal,
// in book order.
type DeviceEvent struct {
	Removal bool
	// Device is the device; a removal with none removes every device of
	// Person enrolled before it.
	Device, Person string
	// TouchFor is a touch key's device.
	TouchFor string
	Position int64
	// Time is when the entry counts from.
	Time time.Time
	// Ref is the caller's own reference to the entry.
	Ref int
}

// DeviceState is one device as the events before a position leave it.
type DeviceState struct {
	Device, Person, TouchFor string
	// First and Last are the positions it was enrolled and last renewed
	// at, LastTime when that renewal counts from, and Ref its event's.
	First, Last int64
	LastTime    time.Time
	Ref         int
	Removed     bool
}

// DeviceStates replays events, in book order, into each device's state. A
// device removed stays removed: a removed key never comes back.
func DeviceStates(events []DeviceEvent) map[string]*DeviceState {
	devs := map[string]*DeviceState{}
	for _, e := range events {
		if e.Removal {
			for _, d := range devs {
				if d.Device == e.Device || e.Device == "" && d.Person == e.Person {
					d.Removed = true
				}
			}
			continue
		}
		d := devs[e.Device]
		if d == nil {
			d = &DeviceState{Device: e.Device, First: e.Position}
			devs[e.Device] = d
		}
		if d.Removed {
			continue
		}
		d.Person, d.TouchFor, d.Last, d.LastTime, d.Ref = e.Person, e.TouchFor, e.Position, e.Time, e.Ref
	}
	return devs
}

// DevicesInForce is the devices in force at time at: not removed, renewed
// within DeviceLapse of at, and, for a touch key, its device in force too.
func DevicesInForce(devs map[string]*DeviceState, at time.Time) map[string]*DeviceState {
	out := map[string]*DeviceState{}
	for id, d := range devs {
		if !d.Removed && at.Sub(d.LastTime) <= DeviceLapse {
			out[id] = d
		}
	}
	for id, d := range out {
		if d.TouchFor != "" && out[d.TouchFor] == nil {
			delete(out, id)
		}
	}
	return out
}

// Agents' grants in force (PEOPLE-KEYS-2 5.2, P5): one rule, which the farm
// book (InForceAt, ForceAgentGrant) and the people's index both use. A
// grant is open from its entry until its end is booked, and lapses
// AgentLapse after its last use, or after the grant itself where the agent
// has made no act under it. Times are the book's, as for devices.

// GrantEvent is one entry of an agent's grant, in book order: the grant
// (KindAgentGrant), the agent's use of it (KindAgentUse) or its end
// (KindGrantEnd).
type GrantEvent struct {
	Kind, Grant string
	// Time is when the entry counts from.
	Time time.Time
	// Ref is the caller's own reference to the entry.
	Ref int
}

// GrantState is one grant as the events leave it: the grant's own event
// (Ref), when it was last used, and whether it ended.
type GrantState struct {
	Grant string
	Ref   int
	Last  time.Time
	Ended bool
}

// GrantStates replays events, in book order, into each grant's state. A
// use or an end before the grant counts for nothing, and a grant ended
// stays ended: a fresh grant is a new one.
func GrantStates(events []GrantEvent) map[string]*GrantState {
	gs := map[string]*GrantState{}
	for _, e := range events {
		g := gs[e.Grant]
		switch {
		case e.Kind == KindAgentGrant && g == nil:
			gs[e.Grant] = &GrantState{Grant: e.Grant, Ref: e.Ref, Last: e.Time}
		case g == nil || g.Ended:
		case e.Kind == KindAgentUse && e.Time.After(g.Last):
			g.Last = e.Time
		case e.Kind == KindGrantEnd:
			g.Ended = true
		}
	}
	return gs
}

// GrantInForce reports whether a grant is in force at time at: booked, not
// ended, and used within AgentLapse of at.
func GrantInForce(g *GrantState, at time.Time) bool {
	return g != nil && !g.Ended && at.Sub(g.Last) <= AgentLapse
}
