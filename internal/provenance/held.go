package provenance

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// A host unit's key is made and certified by its runner, which keeps it
// in memory and in the unit's directory, readable only by the account the
// runner and the unit run as, so a process the runner starts for the unit
// (c4-unit) signs with it too. The file is the key, then its chain, in
// PEM. The runner renews it; a process that reads it never does.

// EncodeHeld is a held key and its chain (leaf first) as one PEM file.
func EncodeHeld(key ed25519.PrivateKey, chain []*x509.Certificate) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, c := range chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out, nil
}

// HeldSigner reads a held key file (EncodeHeld) as a Signer that signs
// with it until its leaf ends, and never renews it.
func HeldSigner(b []byte) (*Signer, error) {
	var key ed25519.PrivateKey
	var chain []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		switch block.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			ek, ok := k.(ed25519.PrivateKey)
			if !ok {
				return nil, errors.New("a held key is Ed25519")
			}
			key = ek
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			chain = append(chain, c)
		}
	}
	if key == nil || len(chain) == 0 {
		return nil, errors.New("a held key file is a key and its chain")
	}
	if pub, ok := chain[0].PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("the held key is not its leaf's (%s)", CertIdentity(chain[0]))
	}
	return &Signer{key: key, chain: chain, issue: func(context.Context, ed25519.PublicKey) ([]*x509.Certificate, error) {
		return nil, errors.New("a held key is renewed by its holder, not here")
	}}, nil
}
