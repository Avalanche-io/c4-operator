package schema

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Avalanche-io/c4"
)

// The records of personal keys: an act, signed by its actor's own key, and
// a device record, one of a person's device keys as a farm's people's
// issuer enrolled it. Both are in-toto statements in a DSSE envelope, as
// every C4 record is.
const (
	StatementType       = "https://in-toto.io/Statement/v1"
	ActPredicateType    = "https://cccc.io/provenance/act/v1"
	DevicePredicateType = "https://cccc.io/provenance/device/v1"
)

//go:embed act-1.schema.json
var actSchema []byte

//go:embed device-1.schema.json
var deviceSchema []byte

// ActSchema is the JSON Schema of an act statement (act/v1), as GET
// /schema/act serves it.
func ActSchema() []byte { return slices.Clone(actSchema) }

// DeviceSchema is the JSON Schema of a device record (device/v1), as GET
// /schema/device serves it.
func DeviceSchema() []byte { return slices.Clone(deviceSchema) }

// Subject is a statement's subject: what a record is about, by name and
// digest.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Act is one act as its actor signed it: the statement in a DSSE payload.
type Act struct {
	Type          string       `json:"_type"`
	Subject       []Subject    `json:"subject"`
	PredicateType string       `json:"predicateType"`
	Predicate     ActPredicate `json:"predicate"`
}

// ActPredicate is what an act says. The HUD's control record that carries
// it out must match its subject, Act and Params exactly.
type ActPredicate struct {
	// Farm is the C4 ID of the farm CA's root certificate.
	Farm string `json:"farm"`
	// Act is the verb, the control record's action.
	Act string `json:"act"`
	// Params is the request's parameters as JSON, or the C4 ID of their
	// canonical form. ParamsID compares them.
	Params json.RawMessage `json:"params"`
	// As is the role a person acted in.
	As string `json:"as,omitempty"`
	// Device is a person's device key that signed, by its device ID.
	Device string `json:"device,omitempty"`
	// Nonce is 16 random bytes, base64url.
	Nonce string `json:"nonce"`
	// Signer is the leaf certificate's identity: by itself, or a relay.
	Signer   string   `json:"signer"`
	Chain    ActChain `json:"chain"`
	SignedAt string   `json:"signedAt"`
}

// ActChain is an act's chain as its actor states it: chain/1's by, for,
// under and using. The deployments and whether the act is accountable are
// the control record's.
type ActChain struct {
	By    Actor        `json:"by"`
	For   For          `json:"for"`
	Under []Delegation `json:"under,omitempty"`
	Using []Credential `json:"using,omitempty"`
}

// ActCitation is a control record's act: the act it carried out, by the
// SHA-256 of the DSSE payload as signed (the key the farm takes each act
// once by) and the C4 ID of its bundle.
type ActCitation struct {
	Payload string `json:"payload"`
	Record  string `json:"record"`
}

// ParseAct reads an act statement and checks its shape. Who may sign it,
// and whether its device or grant is in force, are the verifier's.
func ParseAct(b []byte) (*Act, []Error) {
	var a Act
	if errs := parse(b, "act", &a); errs != nil {
		return nil, errs
	}
	return &a, nil
}

// ParamsID names an act's parameters: the C4 ID they are, or the C4 ID of
// their canonical form (RFC 8785). Two acts' parameters, or an act's and
// its control record's request, match when their ParamsIDs are equal.
func ParamsID(params json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(params, &s) == nil {
		if _, err := c4.Parse(s); err != nil {
			return "", fmt.Errorf("params %q is not a C4 ID", s)
		}
		return s, nil
	}
	b, err := Canonical(params)
	if err != nil {
		return "", err
	}
	return c4.Identify(bytes.NewReader(b)).String(), nil
}

// Device is one device record: the statement the people's issuer signs.
type Device struct {
	Type          string          `json:"_type"`
	Subject       []Subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     DevicePredicate `json:"predicate"`
}

// What a device record's key was certified on.
const (
	EvidenceIDToken   = "id-token"
	EvidenceInstaller = "installer"
	EvidenceOwner     = "owner"
	EvidenceApproval  = "approval"
)

// Where a device key lives.
const (
	ProtectionHardware = "hardware"
	ProtectionBrowser  = "software-browser"
	ProtectionFile     = "software-file"
)

// DevicePredicate is what a device record says.
type DevicePredicate struct {
	Farm   string `json:"farm"`
	Person string `json:"person"`
	// Key is the DER SubjectPublicKeyInfo of a P-256 key, base64url.
	Key        string `json:"key"`
	Protection string `json:"protection"`
	Kind       string `json:"kind"`
	Label      string `json:"label,omitempty"`
	// TouchFor makes this a touch key, for that device key.
	TouchFor    string `json:"touchFor,omitempty"`
	Evidence    string `json:"evidence"`
	IDToken     string `json:"idToken,omitempty"`
	ProviderKey string `json:"providerKey,omitempty"`
	Code        string `json:"code,omitempty"`
	Approval    string `json:"approval,omitempty"`
	Renews      string `json:"renews,omitempty"`
	Nonce       string `json:"nonce"`
	RequestedAt string `json:"requestedAt"`
	// Proof is the key's raw r||s signature over ProofMessage, base64url.
	Proof    string `json:"proof"`
	SignedAt string `json:"signedAt"`
}

// ParseDevice reads a device record and checks its shape.
func ParseDevice(b []byte) (*Device, []Error) {
	var d Device
	if errs := parse(b, "device", &d); errs != nil {
		return nil, errs
	}
	return &d, nil
}

// PublicKey is the record's key, its DER SubjectPublicKeyInfo.
func (d *DevicePredicate) PublicKey() ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(d.Key)
}

// ProofMessage is the enrolment request the device's proof signs: the
// canonical JSON of its farm, person, key, touchFor (a touch key's), nonce
// and requestedAt.
func (d *DevicePredicate) ProofMessage() []byte {
	m := map[string]string{"farm": d.Farm, "person": d.Person, "key": d.Key, "nonce": d.Nonce, "requestedAt": d.RequestedAt}
	if d.TouchFor != "" {
		m["touchFor"] = d.TouchFor
	}
	raw, _ := json.Marshal(m)
	b, _ := Canonical(raw) // strings only, so this cannot fail
	return b
}

// DeviceID is the ID of a device key: the C4 ID of its DER
// SubjectPublicKeyInfo. The leaf names it as urn:c4:device:<ID>, an act as
// device, and a device record as its subject.
func DeviceID(spki []byte) string { return c4.Identify(bytes.NewReader(spki)).String() }

// Canonical writes JSON in its canonical form, RFC 8785 (JCS): no
// whitespace, object keys sorted by their UTF-16 code units, strings with
// only the escapes JSON requires, and numbers as ECMAScript writes them. A
// browser gets the same bytes from JSON.stringify of a key-sorted value. A
// key written twice, or text that isn't UTF-8, is refused.
func Canonical(b []byte) ([]byte, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("canonical JSON: the text is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out bytes.Buffer
	if err := canonValue(d, &out); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("canonical JSON: more than one value")
	}
	return out.Bytes(), nil
}

func canonValue(d *json.Decoder, w *bytes.Buffer) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch x := t.(type) {
	case nil:
		w.WriteString("null")
	case bool:
		w.WriteString(strconv.FormatBool(x))
	case string:
		canonString(w, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("%s is not a number JSON can carry", x)
		}
		w.WriteString(esNumber(f))
	case json.Delim:
		switch x {
		case '[':
			w.WriteByte('[')
			for i := 0; d.More(); i++ {
				if i > 0 {
					w.WriteByte(',')
				}
				if err := canonValue(d, w); err != nil {
					return err
				}
			}
			w.WriteByte(']')
		case '{':
			type member struct {
				key string
				val []byte
			}
			var ms []member
			seen := map[string]bool{}
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return err
				}
				k := kt.(string)
				if seen[k] {
					return fmt.Errorf("%q is written twice", k)
				}
				seen[k] = true
				var v bytes.Buffer
				if err := canonValue(d, &v); err != nil {
					return err
				}
				ms = append(ms, member{k, v.Bytes()})
			}
			slices.SortFunc(ms, func(a, b member) int {
				return slices.Compare(utf16.Encode([]rune(a.key)), utf16.Encode([]rune(b.key)))
			})
			w.WriteByte('{')
			for i, m := range ms {
				if i > 0 {
					w.WriteByte(',')
				}
				canonString(w, m.key)
				w.WriteByte(':')
				w.Write(m.val)
			}
			w.WriteByte('}')
		}
		if _, err := d.Token(); err != nil { // the closing delimiter
			return err
		}
	}
	return nil
}

// canonString writes s with only the escapes RFC 8785 requires: the quote,
// the backslash, and control characters (\b \t \n \f \r by name, the rest
// as \u00xx).
func canonString(w *bytes.Buffer, s string) {
	w.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			w.WriteString(`\"`)
		case '\\':
			w.WriteString(`\\`)
		case '\b':
			w.WriteString(`\b`)
		case '\t':
			w.WriteString(`\t`)
		case '\n':
			w.WriteString(`\n`)
		case '\f':
			w.WriteString(`\f`)
		case '\r':
			w.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(w, `\u%04x`, r)
				continue
			}
			w.WriteRune(r)
		}
	}
	w.WriteByte('"')
}

// esNumber writes f as ECMAScript's Number.prototype.toString does: the
// shortest digits that read back as f, in plain notation from 1e-6 up to
// 1e21 and in exponent notation outside it.
func esNumber(f float64) string {
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return "0" // -0 is 0; JSON carries neither NaN nor infinity
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// d.ddde±x: the digits, and n, where f = 0.digits × 10^n.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	n, k := e+1, len(digits)
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	es := "+"
	if n-1 < 0 {
		es = "-"
	}
	m := digits[:1]
	if k > 1 {
		m += "." + digits[1:]
	}
	return sign + m + "e" + es + strconv.Itoa(abs(n-1))
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
