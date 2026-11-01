package provenance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// A browser's touch is a passkey (PEOPLE-KEYS-2 4.2, 10.4 item 20). A
// passkey never signs a payload as given: it signs a WebAuthn assertion,
// authenticatorData || SHA-256(clientDataJSON), in DER, where the client
// data carries a challenge the caller chose. So a passkey signs for a
// message by taking WebAuthnChallenge(msg) as its challenge: a confirmed
// act's touch key for the DSSE payload's PAE, and a passkey's enrolment
// proof for its request (schema.DevicePredicate.ProofMessage). The CLI's
// touch key needs presence but signs raw, as a device key does.

// WebAuthn is a passkey's assertion beside its signature: the
// authenticator data and the client data it signed.
type WebAuthn struct {
	AuthenticatorData []byte `json:"authenticatorData"`
	ClientDataJSON    []byte `json:"clientDataJSON"`
}

// WebAuthnChallenge is the challenge a passkey signs to sign for msg:
// base64url of its SHA-256, without padding.
func WebAuthnChallenge(msg []byte) string {
	h := sha256.Sum256(msg)
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Assertion is what a passkey's verified assertion says beyond the
// message: the origin the browser signed at, and the hash of the relying
// party's ID the authenticator signed for. Assertion.CheckRP holds them to
// the passkey's own device record, or to the farm's HUD.
type Assertion struct {
	Origin   string
	RPIDHash [32]byte
}

// Authenticator data flags (WebAuthn 6.1): the user was present, and was
// verified (a biometric or a PIN).
const (
	flagUP = 0x01
	flagUV = 0x04
)

// VerifyWebAuthn checks a passkey's assertion over msg: the client data is
// a get (webauthn.get), its challenge is WebAuthnChallenge(msg), and it was
// not made in another origin's frame; the authenticator says the person
// was present and verified (UP and UV); and sig, DER, is the key's
// signature over authenticatorData || SHA-256(clientDataJSON). A high S
// is folded to its low form first, as authenticators sign either. The
// signature counter is not read: synced passkeys keep it at zero, and the
// farm takes each act once by its payload instead.
func VerifyWebAuthn(pub *ecdsa.PublicKey, msg []byte, w WebAuthn, sig []byte) (*Assertion, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, errors.New("a passkey's key is ECDSA P-256")
	}
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(w.ClientDataJSON, &cd); err != nil {
		return nil, fmt.Errorf("the passkey's client data is not JSON: %v", err)
	}
	switch want := WebAuthnChallenge(msg); {
	case cd.Type != "webauthn.get":
		return nil, fmt.Errorf("%w: the passkey's client data is a %q, not a webauthn.get", ErrUntrusted, cd.Type)
	case subtle.ConstantTimeCompare([]byte(cd.Challenge), []byte(want)) != 1:
		return nil, fmt.Errorf("%w: the passkey signed challenge %q, not this message's %q", ErrUntrusted, cd.Challenge, want)
	case cd.CrossOrigin:
		return nil, fmt.Errorf("%w: the passkey signed in a frame of another origin than %s", ErrUntrusted, cd.Origin)
	case cd.Origin == "":
		return nil, fmt.Errorf("%w: the passkey's client data names no origin", ErrUntrusted)
	}
	ad := w.AuthenticatorData
	if len(ad) < 37 {
		return nil, fmt.Errorf("%w: the authenticator data is %d bytes, under the 37 it always has", ErrUntrusted, len(ad))
	}
	if flags := ad[32]; flags&flagUP == 0 || flags&flagUV == 0 {
		return nil, fmt.Errorf("%w: the authenticator does not say the person was present and verified (flags %#02x)", ErrUntrusted, flags)
	}
	var rs struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(sig, &rs)
	if err != nil || len(rest) != 0 || rs.R.Sign() <= 0 || rs.S.Sign() <= 0 {
		return nil, fmt.Errorf("%w: a passkey's signature is DER, an r and an s", ErrSignatureForm)
	}
	if rs.S.Cmp(p256HalfOrder) > 0 {
		rs.S.Sub(elliptic.P256().Params().N, rs.S)
	}
	cdHash := sha256.Sum256(w.ClientDataJSON)
	h := sha256.Sum256(append(append([]byte{}, ad...), cdHash[:]...))
	if !ecdsa.Verify(pub, h[:], rs.R, rs.S) {
		return nil, fmt.Errorf("%w: the passkey's signature does not verify", ErrUntrusted)
	}
	a := &Assertion{Origin: cd.Origin}
	copy(a.RPIDHash[:], ad[:32])
	return a, nil
}

// CheckRP holds an assertion to its relying party: the authenticator
// signed for rpID, and the browser was at origin. A passkey made for
// another site, or used from a page at another origin, is refused.
func (a *Assertion) CheckRP(rpID, origin string) error {
	if h := sha256.Sum256([]byte(rpID)); subtle.ConstantTimeCompare(h[:], a.RPIDHash[:]) != 1 {
		return fmt.Errorf("%w: the passkey signed for another relying party than %s", ErrUntrusted, rpID)
	}
	if a.Origin != origin {
		return fmt.Errorf("%w: the passkey signed at %s, not %s", ErrUntrusted, a.Origin, origin)
	}
	return nil
}
