// Package keystore keeps this machine's keys for a farm (PEOPLE-KEYS-2 3):
// a person's device key, made the first time the CLI acts, without asking,
// and their touch key, which needs their presence for each signature. Each
// lives in the best place the platform has, and says which: the Secure
// Enclave on a Mac (hardware: its sealed blob in a file, usable by any
// program running as the person, never off this Mac), and elsewhere, for
// now, a key file only the person's account can read (software, file). A
// device's signature is in the farm's one form, raw r||s with a low S
// (provenance.SignP256).
package keystore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Kind is which of a person's keys on this machine.
type Kind string

const (
	// Device is the person's everyday key: silent, used while the machine
	// is unlocked.
	Device Kind = "device"
	// Touch is their touch key: each signature needs their presence. Only
	// the Secure Enclave makes one; a key file can't need a touch.
	Touch Kind = "touch"
)

// Workload is a key of a workload's own (an agent's, a cron job's): any
// kind but Device and Touch, named for what it signs as (K5). It is
// silent, and works behind a locked screen: in the Secure Enclave, usable
// after the Mac's first unlock.
func Workload(name string) Kind { return Kind("workload-" + name) }

// Key is a person's key on this machine.
type Key interface {
	// SPKI is its public key, DER SubjectPublicKeyInfo.
	SPKI() []byte
	// Sign signs msg (ECDSA over its SHA-256) in the farm's one form.
	Sign(msg []byte) ([]byte, error)
	// Protection is what keeps it, as a device record names it
	// (schema.ProtectionHardware, schema.ProtectionFile).
	Protection() string
}

// ErrNoTouch means this machine can't make a touch key: no Secure Enclave
// in this build or on this Mac.
var ErrNoTouch = errors.New("this machine makes no touch key: a touch needs the Secure Enclave")

// ErrLocked means the Mac is locked: a person's key, which works only
// while it is unlocked, can't be made or used until they unlock it.
var ErrLocked = errors.New("the Mac is locked: a person's key works only while it is unlocked; unlock it and run again")

// Store is a farm's keys on this machine, in Dir (~/.c4/<farm>/keys),
// readable only by the person.
type Store struct {
	Dir string
	// FileOnly keeps a key file even where the Secure Enclave is: for a
	// test, or a machine where the person chose it.
	FileOnly bool
}

// Key is the store's key of this kind, made the first time it is asked
// for: in the Secure Enclave where there is one, else (a device key) a key
// file. A key already made is used where it is; a store never moves a key
// from one place to another.
func (s Store) Key(kind Kind) (Key, error) {
	if k, err := s.load(kind); err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	if !s.FileOnly && seAvailable() {
		return makeSE(s.path(kind, seExt), kind)
	}
	if kind == Touch {
		return nil, ErrNoTouch
	}
	return makeFile(s.path(kind, fileExt))
}

// Has reports whether the store has a key of this kind.
func (s Store) Has(kind Kind) bool {
	_, err := s.load(kind)
	return err == nil
}

// Remove deletes the store's key of this kind. A Secure Enclave key is its
// blob: deleting the file deletes the key.
func (s Store) Remove(kind Kind) error {
	var errs []error
	for _, ext := range []string{seExt, fileExt} {
		if err := os.Remove(s.path(kind, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const (
	seExt   = ".se"
	fileExt = ".key"
)

func (s Store) path(kind Kind, ext string) string { return filepath.Join(s.Dir, string(kind)+ext) }

func (s Store) load(kind Kind) (Key, error) {
	if b, err := os.ReadFile(s.path(kind, seExt)); err == nil {
		return loadSE(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b, err := os.ReadFile(s.path(kind, fileExt))
	if err != nil {
		return nil, err
	}
	return loadFile(b)
}

// writePrivate writes a key's file, readable only by the person, without
// replacing one that is there.
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// fileKey is a P-256 key in a file only the person's account can read.
type fileKey struct {
	key  *ecdsa.PrivateKey
	spki []byte
}

func makeFile(path string) (Key, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return newFileKey(key)
}

func loadFile(b []byte) (Key, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the key file holds no PEM key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("the key file holds no P-256 key")
	}
	return newFileKey(key)
}

func newFileKey(key *ecdsa.PrivateKey) (Key, error) {
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &fileKey{key: key, spki: spki}, nil
}

func (k *fileKey) SPKI() []byte                    { return k.spki }
func (k *fileKey) Sign(msg []byte) ([]byte, error) { return provenance.SignP256(k.key, msg) }
func (k *fileKey) Protection() string              { return schema.ProtectionFile }

// spkiOf is the DER SubjectPublicKeyInfo of an X9.63 uncompressed P-256
// public key.
func spkiOf(x963 []byte) ([]byte, error) {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), x963)
	if err != nil {
		return nil, fmt.Errorf("the key's public key: %w", err)
	}
	return x509.MarshalPKIXPublicKey(pub)
}

// lowS folds a raw r||s signature's S into the lower half of the group,
// the one form a device's signature has.
func lowS(sig []byte) []byte {
	n := elliptic.P256().Params().N
	s := new(big.Int).SetBytes(sig[32:])
	if s.Cmp(new(big.Int).Rsh(n, 1)) > 0 {
		s.Sub(n, s)
		out := append([]byte(nil), sig[:32]...)
		return append(out, s.FillBytes(make([]byte, 32))...)
	}
	return sig
}
