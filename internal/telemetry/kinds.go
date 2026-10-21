// Package telemetry is what C4 writes about how it behaved, for someone
// outside the moment (DESIGN.md section 6.9): its records cannot hold the
// work. A record's fields are only the closed kinds below: enums with a
// table, counts and integers, reals, scaled money, durations, times
// relative to the record's own epoch, keyed pseudonyms with their rank,
// and the build. None is a string, a byte slice, an error or a map, and a
// test fails the build if one ever is (structure_test.go). Every record
// leaves through Encode, the one encoder; nothing here reads a file,
// copies a log or formats a string into an output.
package telemetry

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// Record is a root type an exporter writes. Only this package's record
// types implement it.
type Record interface{ telemetryRecord() }

// Int is a count or a signed integer the policy reads (slots, units,
// limits; negative where the policy means "none").
type Int int64

// Real is a number the policy reads that is not money: a weight, a boost,
// work in slot-seconds, a rate, bytes per second.
type Real float64

// MarshalJSON writes a Real, and an infinity as null rather than failing.
func (r Real) MarshalJSON() ([]byte, error) {
	f := float64(r)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return []byte("null"), nil
	}
	return json.Marshal(f)
}

// Money is an amount of money multiplied by the bundle's secret factor,
// which is never in the bundle: the policy compares money only linearly,
// so one factor on all of it keeps every decision.
type Money float64

// MarshalJSON writes Money as a Real.
func (m Money) MarshalJSON() ([]byte, error) { return Real(m).MarshalJSON() }

// Nanos is a duration, exact, in nanoseconds (the policy's own resolution).
type Nanos int64

// DurationOf is a duration as a record holds it.
func DurationOf(d time.Duration) Nanos { return Nanos(d) }

// Duration is a Nanos as a time.Duration.
func (n Nanos) Duration() time.Duration { return time.Duration(n) }

// RelTime is a time as nanoseconds after the record's own epoch (its
// snapshot's now), or none. No date, zone or wall-clock time is ever
// written: the policy reads time only as "zero means none" and as
// differences, so shifting every time by one offset changes nothing.
type RelTime struct {
	ns  int64
	set bool
}

// RelTimeOf is t relative to epoch; a zero t stays none.
func RelTimeOf(t, epoch time.Time) RelTime {
	if t.IsZero() {
		return RelTime{}
	}
	return RelTime{ns: int64(t.Sub(epoch)), set: true}
}

// At is the time this is from epoch; none is the zero time.
func (r RelTime) At(epoch time.Time) time.Time {
	if !r.set {
		return time.Time{}
	}
	return epoch.Add(time.Duration(r.ns))
}

// None says the time is none.
func (r RelTime) None() bool { return !r.set }

// MarshalJSON writes the offset in nanoseconds, or null for none.
func (r RelTime) MarshalJSON() ([]byte, error) {
	if !r.set {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatInt(r.ns, 10)), nil
}

// UnmarshalJSON reads what MarshalJSON writes.
func (r *RelTime) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*r = RelTime{}
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*r = RelTime{ns: n, set: true}
	return nil
}

// Permille is a share in thousandths.
type Permille uint16

// Log2 is a bucket index: the bucket holds values in [2^(n-1), 2^n), with
// 0 for zero.
type Log2 uint8

// Log2Of is v's bucket.
func Log2Of(v float64) Log2 {
	if v < 1 {
		return 0
	}
	b := int(math.Floor(math.Log2(v))) + 1
	if b > 255 {
		b = 255
	}
	return Log2(b)
}
