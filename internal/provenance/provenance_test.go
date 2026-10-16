package provenance

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

func record(t *testing.T) *Statement {
	t.Helper()
	out := c4.Identify(strings.NewReader("frame 1039"))
	return &Statement{
		Subject: []Subject{SubjectFor("renders/sh010/sh010.1039.exr", out)},
		Predicate: TaskPredicate{
			Builder:    Builder{ID: "https://cccc.io/c4-operator/node", Version: "v2"},
			Invocation: Invocation{Project: "show-a", Namespace: "show-a", Pod: "sh010-u38-x", PodUID: "uid", Job: "sh010", Unit: "38", Attempt: "1", Class: "render"},
			Inputs:     []Input{{Materialization: "plates", View: c4.Identify(strings.NewReader("input")).String()}},
			Output:     Output{View: c4.Identify(strings.NewReader("output")).String(), Added: 2},
			Runtime:    Runtime{Node: "worker2", StartedOn: time.Unix(1000, 0).UTC(), FinishedOn: time.Unix(1060, 0).UTC()},
		},
	}
}

func TestSignAndVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	env, err := Sign(record(t), priv)
	if err != nil {
		t.Fatal(err)
	}
	st, signer, err := Verify(env, map[string]ed25519.PublicKey{KeyID(pub): pub})
	if err != nil || signer != KeyID(pub) {
		t.Fatalf("verify: %v (%s)", err, signer)
	}
	if st.Predicate.Invocation.Unit != "38" || st.Subject[0].Digest["sha512"] == "" || st.Type != StatementType {
		t.Fatalf("statement %+v", st)
	}
	// The SHA-512 digest is the one the C4 ID encodes.
	id, _ := c4.Parse(st.Subject[0].Digest["c4"])
	if c4.Identify(strings.NewReader("frame 1039")) != id {
		t.Fatal("subject digest does not name the file")
	}
}

func TestTamperedOrForeignRecordsFail(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	env, _ := Sign(record(t), priv)
	if _, _, err := Verify(env, map[string]ed25519.PublicKey{KeyID(other): other}); err != ErrUntrusted {
		t.Fatalf("a record signed by an unknown key verified: %v", err)
	}
	// Change the node in the payload, keep the signature.
	var e Envelope
	json.Unmarshal(env, &e)
	payload, _ := base64.StdEncoding.DecodeString(e.Payload)
	e.Payload = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(payload), "worker2", "worker9", 1)))
	forged, _ := json.Marshal(e)
	if _, _, err := Verify(forged, map[string]ed25519.PublicKey{KeyID(pub): pub}); err != ErrUntrusted {
		t.Fatalf("a tampered record verified: %v", err)
	}
}

func TestExportsCarryTheDeclaredSource(t *testing.T) {
	st := record(t)
	m := C2PAManifest(st, "c4rec", "renders/sh010/sh010.1039.exr", "c4file")
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), "algorithmicMedia") || strings.Contains(string(b), "ai-disclosure") {
		t.Fatalf("a renderer's frame: %s", b)
	}
	st.Predicate.AI = json.RawMessage(`{"generated":"assisted","model":"denoise-v3","oversight":"human_validated"}`)
	b, _ = json.Marshal(C2PAManifest(st, "c4rec", "renders/sh010/sh010.1039.exr", "c4file"))
	if !strings.Contains(string(b), "compositeWithTrainedAlgorithmicMedia") || !strings.Contains(string(b), "c2pa.ai-disclosure") ||
		!strings.Contains(string(b), "human_validated") {
		t.Fatalf("a model-assisted frame: %s", b)
	}
	o, _ := json.Marshal(OMCAsset(st, "c4rec", "renders/sh010/sh010.1039.exr", "c4file"))
	if !strings.Contains(string(o), `"identifierScope":"c4"`) || !strings.Contains(string(o), `"entityType":"Asset"`) {
		t.Fatalf("OMC asset: %s", o)
	}
}
