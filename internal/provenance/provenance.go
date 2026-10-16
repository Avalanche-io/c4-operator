// Package provenance makes and checks task records (DESIGN.md section 9):
// in-toto Statements (v1) with a task predicate, in DSSE envelopes signed
// with Ed25519, stored as C4 objects next to the content they describe.
//
// A record says which unit of which job, on which node, from which images,
// mounting which input views, produced which output view and which files.
// The node daemon makes it at capture, outside the pod, with a key no pod
// can read, so a task cannot forge its own record (the SLSA Build L3
// isolation property). Every digest in it is a C4 ID, which is a SHA-512,
// so a record can be checked against the bytes with no other index.
package provenance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4"
)

// Types and URIs.
const (
	StatementType = "https://in-toto.io/Statement/v1"
	PredicateType = "https://cccc.io/provenance/task/v1"
	PayloadType   = "application/vnd.in-toto+json"
)

// Statement is an in-toto Statement v1.
type Statement struct {
	Type          string        `json:"_type"`
	Subject       []Subject     `json:"subject"`
	PredicateType string        `json:"predicateType"`
	Predicate     TaskPredicate `json:"predicate"`
}

// Subject is an artifact the statement is about.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// SubjectFor names an artifact by its C4 ID, with the SHA-512 digest the
// ID encodes, so tools that know only in-toto digests can match it.
func SubjectFor(name string, id c4.ID) Subject {
	return Subject{Name: name, Digest: map[string]string{"c4": id.String(), "sha512": hex.EncodeToString(id[:])}}
}

// TaskPredicate is the task record: what ran, where, on what, making what.
type TaskPredicate struct {
	Builder    Builder    `json:"builder"`
	Invocation Invocation `json:"invocation"`
	Inputs     []Input    `json:"inputs"`
	Output     Output     `json:"output"`
	Runtime    Runtime    `json:"runtime"`
	// AI is the job's declared use of generative models, if any
	// (section 9.4), carried from the WorkJob's annotation verbatim.
	AI json.RawMessage `json:"ai,omitempty"`
}

// Builder is the component that observed the task and signed the record.
type Builder struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Invocation identifies the task.
type Invocation struct {
	Project   string `json:"project"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	PodUID    string `json:"podUID"`
	Job       string `json:"job,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Attempt   string `json:"attempt,omitempty"`
	Class     string `json:"class,omitempty"`
}

// Input is a view the task mounted.
type Input struct {
	Materialization string `json:"materialization"`
	View            string `json:"view"`
}

// Output is what capture stored.
type Output struct {
	View      string `json:"view"`
	Changeset string `json:"changeset,omitempty"`
	Added     int    `json:"added"`
	Modified  int    `json:"modified"`
	Deleted   int    `json:"deleted"`
	Bytes     int64  `json:"bytes"`
	// FilesListed is how many changed files are subjects; a changeset with
	// more lists the rest only through its own subject.
	FilesListed int `json:"filesListed"`
}

// Runtime is where and from what the task ran.
type Runtime struct {
	Node       string    `json:"node"`
	Images     []Image   `json:"images"`
	StartedOn  time.Time `json:"startedOn"`
	FinishedOn time.Time `json:"finishedOn"`
}

// Image is a container image by digest.
type Image struct {
	Container string `json:"container"`
	Image     string `json:"image"`
	Digest    string `json:"digest,omitempty"`
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one DSSE signature.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// pae is DSSE's pre-authentication encoding.
func pae(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("DSSEv1 ")
	b.WriteString(strconv.Itoa(len(payloadType)))
	b.WriteByte(' ')
	b.WriteString(payloadType)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(payload)))
	b.WriteByte(' ')
	b.Write(payload)
	return b.Bytes()
}

// KeyID names a public key: "c4-" and the first 16 hex digits of its
// SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "c4-" + hex.EncodeToString(h[:8])
}

// Sign wraps a statement in a signed envelope.
func Sign(st *Statement, key ed25519.PrivateKey) ([]byte, error) {
	st.Type, st.PredicateType = StatementType, PredicateType
	payload, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, pae(PayloadType, payload))
	env := Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: KeyID(key.Public().(ed25519.PublicKey)), Sig: base64.StdEncoding.EncodeToString(sig)}}}
	return json.Marshal(env)
}

// ErrUntrusted means no trusted key signed the record.
var ErrUntrusted = errors.New("no trusted key signed this record")

// Verify checks an envelope's signature against trusted keys by key ID and
// returns its statement and the key that signed it.
func Verify(data []byte, keys map[string]ed25519.PublicKey) (*Statement, string, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, "", fmt.Errorf("not a DSSE envelope: %w", err)
	}
	if env.PayloadType != PayloadType {
		return nil, "", fmt.Errorf("payload type %q, want %q", env.PayloadType, PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, "", err
	}
	signer := ""
	for _, s := range env.Signatures {
		pub, ok := keys[s.KeyID]
		if !ok {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		if ed25519.Verify(pub, pae(env.PayloadType, payload), sig) {
			signer = s.KeyID
			break
		}
	}
	if signer == "" {
		return nil, "", ErrUntrusted
	}
	var st Statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, "", err
	}
	if st.Type != StatementType || st.PredicateType != PredicateType {
		return nil, "", fmt.Errorf("statement type %q, predicate %q", st.Type, st.PredicateType)
	}
	return &st, signer, nil
}

// ParseKey reads a base64 Ed25519 seed or private key.
func ParseKey(text string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace([]byte(text))))
	if err != nil {
		return nil, err
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("an Ed25519 key is %d or %d bytes, not %d", ed25519.SeedSize, ed25519.PrivateKeySize, len(b))
}

// ParsePublic reads a base64 Ed25519 public key.
func ParsePublic(text string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace([]byte(text))))
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("an Ed25519 public key is %d bytes, not %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}
