//go:build darwin && cgo && c4se

package keystore

// The Secure Enclave, through c4se's static library (c4se/c4se.swift),
// built for the target's architecture by make keystore-se. A build without
// -tags c4se keeps key files (se_other.go).

/*
#cgo LDFLAGS: -L${SRCDIR}/c4se -lc4se -framework CryptoKit -framework Security -framework Foundation -L/usr/lib/swift
#include <stdint.h>
int32_t c4se_available(void);
int32_t c4se_make(int32_t kind, uint8_t *blob, int32_t cap, uint8_t *pub, char *errbuf, int32_t errcap);
int32_t c4se_public(const uint8_t *blob, int32_t blen, uint8_t *pub, char *errbuf, int32_t errcap);
int32_t c4se_sign(const uint8_t *blob, int32_t blen, const uint8_t *msg, int32_t mlen, uint8_t *sig, char *errbuf, int32_t errcap);
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/Avalanche-io/c4-operator/schema"
)

func seAvailable() bool { return C.c4se_available() == 1 }

// seKey is a key in the Secure Enclave, by its sealed blob.
type seKey struct {
	blob []byte
	spki []byte
}

func makeSE(path string, kind Kind) (Key, error) {
	k := C.int32_t(0)
	if kind == Touch {
		k = 1
	}
	blob, pub, errbuf := make([]byte, 1024), make([]byte, 65), make([]byte, 512)
	n := C.c4se_make(k, (*C.uint8_t)(&blob[0]), C.int32_t(len(blob)), (*C.uint8_t)(&pub[0]),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.int32_t(len(errbuf)))
	if n < 0 {
		return nil, seError(fmt.Sprintf("the Secure Enclave made no %s key", kind), errbuf)
	}
	spki, err := spkiOf(pub)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(path, blob[:n]); err != nil {
		return nil, err
	}
	return &seKey{blob: blob[:n], spki: spki}, nil
}

func loadSE(blob []byte) (Key, error) {
	if len(blob) == 0 {
		return nil, errors.New("the Secure Enclave key's blob is empty")
	}
	pub, errbuf := make([]byte, 65), make([]byte, 512)
	if C.c4se_public((*C.uint8_t)(&blob[0]), C.int32_t(len(blob)), (*C.uint8_t)(&pub[0]),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.int32_t(len(errbuf))) < 0 {
		return nil, fmt.Errorf("the Secure Enclave key: %s", cString(errbuf))
	}
	spki, err := spkiOf(pub)
	if err != nil {
		return nil, err
	}
	return &seKey{blob: blob, spki: spki}, nil
}

func (k *seKey) SPKI() []byte       { return k.spki }
func (k *seKey) Protection() string { return schema.ProtectionHardware }

func (k *seKey) Sign(msg []byte) ([]byte, error) {
	sig, errbuf := make([]byte, 64), make([]byte, 512)
	// never a null pointer, even for an empty message
	buf := append(append([]byte(nil), msg...), 0)
	if C.c4se_sign((*C.uint8_t)(&k.blob[0]), C.int32_t(len(k.blob)), (*C.uint8_t)(&buf[0]), C.int32_t(len(msg)), (*C.uint8_t)(&sig[0]),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.int32_t(len(errbuf))) < 0 {
		return nil, seError("the Secure Enclave did not sign", errbuf)
	}
	return lowS(sig), nil
}

// errSecInteractionNotAllowed is what the Secure Enclave answers for a
// person's key while the Mac is locked.
const errSecInteractionNotAllowed = "Code=-25308"

func seError(what string, errbuf []byte) error {
	why := cString(errbuf)
	if strings.Contains(why, errSecInteractionNotAllowed) {
		return fmt.Errorf("%s: %w", what, ErrLocked)
	}
	return fmt.Errorf("%s: %s", what, why)
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
