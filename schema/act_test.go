package schema

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// TestCanonicalNumbers holds Canonical to RFC 8785's number examples
// (appendix B), each an IEEE 754 double by its bits.
func TestCanonicalNumbers(t *testing.T) {
	for bits, want := range map[uint64]string{
		0x0000000000000000: "0",
		0x8000000000000000: "0",
		0x0000000000000001: "5e-324",
		0x8000000000000001: "-5e-324",
		0x7fefffffffffffff: "1.7976931348623157e+308",
		0xffefffffffffffff: "-1.7976931348623157e+308",
		0x4340000000000000: "9007199254740992",
		0xc340000000000000: "-9007199254740992",
		0x4430000000000000: "295147905179352830000",
		0x44b52d02c7e14af5: "9.999999999999997e+22",
		0x44b52d02c7e14af6: "1e+23",
		0x44b52d02c7e14af7: "1.0000000000000001e+23",
		0x444b1ae4d6e2ef4e: "999999999999999700000",
		0x444b1ae4d6e2ef4f: "999999999999999900000",
		0x444b1ae4d6e2ef50: "1e+21",
		0x3eb0c6f7a0b5ed8c: "9.999999999999997e-7",
		0x3eb0c6f7a0b5ed8d: "0.000001",
		0x41b3de4355555553: "333333333.3333332",
		0x41b3de4355555554: "333333333.33333325",
		0x41b3de4355555555: "333333333.3333333",
		0x41b3de4355555556: "333333333.3333334",
		0x41b3de4355555557: "333333333.33333343",
		0xbecbf647612f3696: "-0.0000033333333333333333",
		0x43143ff3c1cb0959: "1424953923781206.2",
	} {
		if got := esNumber(math.Float64frombits(bits)); got != want {
			t.Errorf("%#016x: got %s, want %s", bits, got, want)
		}
	}
}

// TestCanonical holds Canonical to RFC 8785's worked examples: its
// section 3.2.2 document, and section 3.2.3's keys sorted by UTF-16 code
// units, where an emoji comes before U+FB33 though its UTF-8 sorts after.
func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		`{"numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001], "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/", "literals": [null, true, false]}`:                                          `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`,
		`{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh", "1": "One", "\ud83d\ude00": "Emoji: Grinning Face", "\u0080": "Control", "\u00f6": "Latin Small Letter O With Diaeresis"}`: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		`{"b": [], "a": {"d": {}, "c": "<&>\u2028"}}`: "{\"a\":{\"c\":\"<&>\u2028\",\"d\":{}},\"b\":[]}",
	} {
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s:\n got  %s\n want %s", in, got, want)
		}
	}
	for _, bad := range []string{`{"a": 1, "a": 2}`, "\"\xff\"", `{"a": 1} {}`} {
		if _, err := Canonical([]byte(bad)); err == nil {
			t.Errorf("%q: canonical, want refused", bad)
		}
	}
}

func TestParamsID(t *testing.T) {
	a, err := ParamsID(json.RawMessage(`{"reason": "plate", "frames": [1001, 1012]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParamsID(json.RawMessage(`{"frames":[1001,1012.0],"reason":"plate"}`))
	if a != b {
		t.Errorf("the same parameters, two IDs: %s and %s", a, b)
	}
	if id, err := ParamsID(json.RawMessage(`"` + a + `"`)); err != nil || id != a {
		t.Errorf("a C4 ID names itself: %s, %v", id, err)
	}
	if _, err := ParamsID(json.RawMessage(`"plate"`)); err == nil {
		t.Error("a string that isn't a C4 ID was taken as params")
	}
}

// TestActChainIsChains: an act's chain reuses chain/1's definitions, copied
// so the schema stands alone; the copies must not drift.
func TestActChainIsChains(t *testing.T) {
	var act, chain, device struct {
		Defs map[string]any `json:"$defs"`
	}
	for b, into := range map[*[]byte]any{&actSchema: &act, &chainSchema: &chain, &deviceSchema: &device} {
		if err := json.Unmarshal(*b, into); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"party", "id", "grade", "by", "for", "delegation", "credential", "deploymentName"} {
		if !reflect.DeepEqual(act.Defs[d], chain.Defs[d]) {
			t.Errorf("act-1's $defs.%s differs from chain-1's: run mkactschemas.py", d)
		}
	}
	for _, d := range []string{"party", "id"} {
		if !reflect.DeepEqual(device.Defs[d], chain.Defs[d]) {
			t.Errorf("device-1's $defs.%s differs from chain-1's", d)
		}
	}
}

// TestDeviceRecords checks every passing device record as the issuer will:
// the subject is the key's device ID, and the proof is the key's own
// signature over its enrolment request, raw r||s with a low S.
func TestDeviceRecords(t *testing.T) {
	ids := map[string]bool{}
	read := 0
	for _, c := range cases(t) {
		if c.Format != "device" || !c.Pass {
			continue
		}
		d, errs := ParseDevice(c.Doc)
		if errs != nil {
			t.Fatalf("%s: %+v", c.Name, errs)
		}
		p := d.Predicate
		spki, err := p.PublicKey()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		id := DeviceID(spki)
		if d.Subject[0].Digest["c4"] != id || d.Subject[0].Name != "device:"+id {
			t.Errorf("%s: the subject is %+v, the key's device ID %s", c.Name, d.Subject[0], id)
		}
		k, err := x509.ParsePKIXPublicKey(spki)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			t.Fatalf("%s: the key is %T, not P-256", c.Name, k)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(p.Proof)
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if s.Cmp(new(big.Int).Rsh(elliptic.P256().Params().N, 1)) > 0 {
			t.Errorf("%s: the proof's S is high", c.Name)
		}
		h := sha256.Sum256(p.ProofMessage())
		if !ecdsa.Verify(pub, h[:], r, s) {
			t.Errorf("%s: the proof does not verify over %s", c.Name, p.ProofMessage())
		}
		if p.Evidence == EvidenceIDToken {
			payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(p.IDToken, ".")[1])
			var claims struct{ Nonce string }
			if json.Unmarshal(payload, &claims); claims.Nonce != id {
				t.Errorf("%s: the ID token's nonce is %q, not the device ID", c.Name, claims.Nonce)
			}
		}
		ids[id] = true
		read++
	}
	if read == 0 {
		t.Fatal("no device records read")
	}
	// Each person's act names a device the fixtures enrolled, and a touch
	// key names one.
	for _, c := range cases(t) {
		switch {
		case c.Format == "act" && c.Pass:
			a, _ := ParseAct(c.Doc)
			if a.Predicate.Device != "" && !ids[a.Predicate.Device] {
				t.Errorf("act %s names device %s, which no record enrols", c.Name, a.Predicate.Device)
			}
		case c.Format == "device" && c.Pass:
			d, _ := ParseDevice(c.Doc)
			if d.Predicate.TouchFor != "" && !ids[d.Predicate.TouchFor] {
				t.Errorf("touch key %s is for device %s, which no record enrols", c.Name, d.Predicate.TouchFor)
			}
		}
	}
}

func TestActFixturesRead(t *testing.T) {
	b, err := fs.ReadFile(Fixtures(), "act/pass/agent-holds.json")
	if err != nil {
		t.Fatal(err)
	}
	a, errs := ParseAct(b)
	if errs != nil {
		t.Fatal(errs)
	}
	p := a.Predicate
	if a.PredicateType != ActPredicateType || p.Chain.By.Agent != "assistant" || p.Chain.Under[0].Kind != "sign" || p.Signer != identityBase+"agent/assistant" {
		t.Errorf("read as %+v", a)
	}
	if id, err := ParamsID(p.Params); err != nil || !strings.HasPrefix(id, "c4") {
		t.Errorf("params ID %q, %v", id, err)
	}
}
