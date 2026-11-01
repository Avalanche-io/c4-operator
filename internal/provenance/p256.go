package provenance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"math/big"
)

// A person's device keys are ECDSA P-256 (PEOPLE-KEYS-2 3): no Secure
// Enclave or TPM makes Ed25519 keys, and every browser has P-256. Farm
// parts and the farm CA stay Ed25519. A device's signature is raw r||s,
// 64 bytes, over the SHA-256 of what it signs, with a low S: WebCrypto
// returns r||s, and with S folded into the lower half of the group a
// signature has exactly one form, so no one can rewrite it into a second
// valid one. A client that signs in DER (Go's SignASN1, CryptoKit's DER
// form) converts.

// P256SignatureSize is the length of a device's signature: r||s.
const P256SignatureSize = 64

// ErrSignatureForm means a P-256 signature is not in its one form: raw
// r||s, 64 bytes, with a low S.
var ErrSignatureForm = errors.New("a P-256 signature is raw r||s, 64 bytes, with a low S")

// p256HalfOrder is the largest low S: half the group's order.
var p256HalfOrder = new(big.Int).Rsh(elliptic.P256().Params().N, 1)

// SignP256 signs msg with a device key in its one form: raw r||s, low S.
func SignP256(key *ecdsa.PrivateKey, msg []byte) ([]byte, error) {
	if key.Curve != elliptic.P256() {
		return nil, errors.New("the key is not P-256")
	}
	h := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return nil, err
	}
	if s.Cmp(p256HalfOrder) > 0 {
		s.Sub(key.Curve.Params().N, s)
	}
	out := make([]byte, P256SignatureSize)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out, nil
}

// VerifyP256 checks a device key's signature over msg. It refuses any
// length but 64 and any high S with ErrSignatureForm, and a signature
// the key did not make with ErrUntrusted.
func VerifyP256(pub *ecdsa.PublicKey, msg, sig []byte) error {
	if pub == nil || pub.Curve != elliptic.P256() {
		return errors.New("the key is not P-256")
	}
	if len(sig) != P256SignatureSize {
		return ErrSignatureForm
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if s.Cmp(p256HalfOrder) > 0 {
		return ErrSignatureForm
	}
	h := sha256.Sum256(msg)
	if !ecdsa.Verify(pub, h[:], r, s) {
		return ErrUntrusted
	}
	return nil
}
