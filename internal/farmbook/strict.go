package farmbook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/Avalanche-io/c4-operator/schema"
)

// Entries are read strictly. Go's JSON decoder matches keys in any case,
// takes the last of two, and ignores keys it doesn't know, so an entry
// checked as one thing could be sealed as another ("act" and "ACT"). An
// entry is taken only as the canonical JSON (RFC 8785) of exactly its
// kind's fields: every key written as the field names it, none unknown,
// none twice, and the bytes the canonical form of what they decode to.
// What is sealed is built again from the checked value, never from the
// bytes written.

// ErrMalformed means an entry isn't the canonical JSON of its kind.
var ErrMalformed = errors.New("not the canonical JSON of an entry")

// Strict decodes raw into v, a pointer to a struct, refusing anything but
// the canonical JSON of exactly that struct.
func Strict(raw []byte, v any) error {
	canon, err := schema.Canonical(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !bytes.Equal(canon, raw) {
		return fmt.Errorf("%w: its bytes aren't canonical", ErrMalformed)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	names := fieldNames(reflect.TypeOf(v).Elem())
	for k := range keys {
		if !names[k] {
			return fmt.Errorf("%w: it has a key %.40q its kind doesn't", ErrMalformed, k)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	again, _, err := canonical(v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !bytes.Equal(again, raw) {
		return fmt.Errorf("%w: it doesn't read back as written", ErrMalformed)
	}
	return nil
}

var names sync.Map // reflect.Type -> map[string]bool

// fieldNames are the JSON names of a struct's fields, as written.
func fieldNames(t reflect.Type) map[string]bool {
	if m, ok := names.Load(t); ok {
		return m.(map[string]bool)
	}
	m := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		m[name] = true
	}
	names.Store(t, m)
	return m
}

// exactFields reads the fields of v named exactly by raw's keys, ignoring
// the rest: how a reader takes an entry's shared fields from an entry of
// any kind without matching a key in another case.
func exactFields(raw []byte, v any) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	rv := reflect.ValueOf(v).Elem()
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" || name == "" {
			continue
		}
		if b, ok := keys[name]; ok {
			if err := json.Unmarshal(b, rv.Field(i).Addr().Interface()); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

var c4Pattern = regexp.MustCompile(`^c4[1-9A-HJ-NP-Za-km-z]{88}$`)

// IsC4ID reports whether s is a C4 ID.
func IsC4ID(s string) bool { return c4Pattern.MatchString(s) }

// Reasons a seal refuses an entry (schema.BookReasons).
const (
	ReasonMissing     = "missing"
	ReasonMalformed   = "malformed"
	ReasonNotPending  = "not-pending"
	ReasonOtherFarm   = "other-farm"
	ReasonUnknownKind = "unknown-kind"
	ReasonUnsigned    = "unsigned"
	ReasonBooked      = "booked"
	ReasonRecordGone  = "record-missing"
	ReasonUnverified  = "unverified"
	ReasonWrongSigner = "wrong-signer"
	ReasonDisagrees   = "disagrees"
	ReasonNotInForce  = "not-in-force"
	ReasonOverLimit   = "over-limit"
	ReasonRejected    = "rejected"
)

// refusal is a checker's refusal with its reason.
type refusal struct {
	reason string
	err    error
}

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

// Refuse is an error a checker refuses an entry with, for reason.
func Refuse(reason string, err error) error { return &refusal{reason: reason, err: err} }

// ReasonOf is the reason an error refuses an entry: its Refuse reason, a
// malformed entry's, or rejected.
func ReasonOf(err error) string {
	var r *refusal
	switch {
	case errors.As(err, &r):
		return r.reason
	case errors.Is(err, ErrMalformed):
		return ReasonMalformed
	}
	return ReasonRejected
}

// Reasons a sealed entry is flagged (schema.BookFlag).
const (
	FlagWindow    = schema.FlagWindow
	FlagNoTime    = schema.FlagNoTime
	FlagUnchecked = schema.FlagUnchecked
)
