package farmcli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A person's keys on this machine, as the CLI enrols and uses them
// (PEOPLE-KEYS-2 3, K4): its keys are internal/keystore's; their
// certificates, which the people's issuer gives, are kept beside them.

// PeopleConfig is the people's issuer's config: the farm's ID, its sign-in
// provider, and a one-person farm's owner.
func PeopleConfig(ctx context.Context, c *Client) (*people.Config, error) {
	var cfg people.Config
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/config", nil, &cfg); err != nil {
		return nil, fmt.Errorf("the farm's people's issuer: %w", err)
	}
	return &cfg, nil
}

// deviceKind is this machine as a device record names it.
func deviceKind() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	}
	return "linux"
}

// deviceID is a key's device ID: the C4 ID of its public key.
func deviceID(k keystore.Key) string { return schema.DeviceID(k.SPKI()) }

// enrolment is k's request to be person's key at the farm, signed with k
// now: a device key, or a touch key for the device key touchFor names.
func enrolment(k keystore.Key, farm, person, touchFor string) (people.Enrolment, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return people.Enrolment{}, err
	}
	q := people.Request{Farm: farm, Person: person, Key: base64.RawURLEncoding.EncodeToString(k.SPKI()), TouchFor: touchFor,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: time.Now().UTC().Format(time.RFC3339)}
	p := schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt}
	sig, err := k.Sign(p.ProofMessage())
	if err != nil {
		return people.Enrolment{}, err
	}
	host, _ := os.Hostname()
	label := "c4 on " + host
	if touchFor != "" {
		label += " (touch)"
	}
	return people.Enrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig), Protection: k.Protection(),
		Kind: deviceKind(), Label: label}, nil
}

// certPath is where a key's certificate is kept: beside it.
func certPath(st keystore.Store, kind keystore.Kind) string {
	return filepath.Join(st.Dir, string(kind)+".cert")
}

// saveCert keeps a key's certificate, readable only by the person.
func saveCert(st keystore.Store, kind keystore.Kind, c *people.Certified) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(certPath(st, kind), b, 0o600)
}

// LoadCert is a key's certificate as the issuer gave it, or nil when it
// has none.
func LoadCert(st keystore.Store, kind keystore.Kind) (*people.Certified, error) {
	b, err := os.ReadFile(certPath(st, kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c people.Certified
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", certPath(st, kind), err)
	}
	return &c, nil
}

// deviceCert is a certificate as provenance signs an act with it.
func deviceCert(c *people.Certified) provenance.DeviceCert {
	return provenance.DeviceCert{Identity: c.Identity, Device: c.Device, Chain: c.Chain}
}

// EnrolOwner is c4 farm up's step on a one-person farm (3.3): the owner's
// device key and touch key, made here at the terminal if they aren't, and
// enrolled on the installer token, once; their certificates are kept
// beside them. A machine that makes no touch key (no Secure Enclave)
// enrols the device key alone. A farm whose owner is enrolled already
// answers with the certificates this machine kept.
func EnrolOwner(ctx context.Context, c *Client, st keystore.Store, owner string) (*people.OwnerCertified, error) {
	cfg, err := PeopleConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	if cfg.OwnerEnrolled {
		dev, err := LoadCert(st, keystore.Device)
		if err != nil || dev == nil {
			return nil, fmt.Errorf("the farm's owner key is enrolled, and this machine has no certificate for it: %v", err)
		}
		touch, _ := LoadCert(st, keystore.Touch)
		return &people.OwnerCertified{Owner: dev, Touch: touch}, nil
	}
	dev, err := st.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	devEnrol, err := enrolment(dev, cfg.Farm, owner, "")
	if err != nil {
		return nil, err
	}
	body := people.OwnerEnrolment{Owner: devEnrol}
	touch, err := st.Key(keystore.Touch)
	switch {
	case errors.Is(err, keystore.ErrNoTouch):
		// touch-list acts go unsigned until a touch key is enrolled
	case err != nil:
		return nil, err
	default:
		t, err := enrolment(touch, cfg.Farm, owner, deviceID(dev))
		if err != nil {
			return nil, err
		}
		body.Touch = &t
	}
	var out people.OwnerCertified
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/owner", body, &out); err != nil {
		return nil, fmt.Errorf("enrolling the owner's key: %w", err)
	}
	if out.Owner == nil {
		return nil, errors.New("the farm's issuer certified no owner key")
	}
	if err := saveCert(st, keystore.Device, out.Owner); err != nil {
		return nil, err
	}
	if out.Touch != nil {
		if err := saveCert(st, keystore.Touch, out.Touch); err != nil {
			return nil, err
		}
	}
	return &out, nil
}
