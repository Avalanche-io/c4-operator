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
