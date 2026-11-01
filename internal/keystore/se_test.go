//go:build darwin && cgo && c4se

package keystore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Avalanche-io/c4-operator/schema"
)

// On a Mac, a device key is made in the Secure Enclave, silently: its blob
// is kept 0600, it signs in the farm's one form, and the store reads the
// same key back. Deleting the blob deletes the key. (make test-keystore-se)
func TestTheSecureEnclaveKeepsTheDeviceKey(t *testing.T) {
	if !seAvailable() {
		t.Skip("no Secure Enclave on this Mac")
	}
	s := Store{Dir: filepath.Join(t.TempDir(), "keys")}
	k, err := s.Key(Device)
	if errors.Is(err, ErrLocked) {
		t.Skip("the Mac is locked: a person's key can't be made until it is unlocked")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Remove(Device) })
	if k.Protection() != schema.ProtectionHardware {
		t.Errorf("protection %q", k.Protection())
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "device.se"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the blob: %v %v", fi, err)
	}
	for i := range 32 {
		msg := []byte{byte(i), 'a', 'c', 't'}
		sig, err := k.Sign(msg)
		if err != nil {
			t.Fatal(err)
		}
		verify(t, k, msg, sig)
	}
	if sig, err := k.Sign(nil); err != nil {
		t.Errorf("an empty message: %v", err)
	} else {
		verify(t, k, nil, sig)
	}
	again, err := s.Key(Device)
	if err != nil || !bytes.Equal(again.SPKI(), k.SPKI()) {
		t.Fatalf("read back: %v", err)
	}
	if err := s.Remove(Device); err != nil || s.Has(Device) {
		t.Errorf("removed: %v", err)
	}
}

// The touch key is made in the Secure Enclave too, with no prompt to make
// it. It is never signed with here: each of its signatures asks for Touch
// ID, which no test may.
func TestTheSecureEnclaveMakesTheTouchKey(t *testing.T) {
	if !seAvailable() {
		t.Skip("no Secure Enclave on this Mac")
	}
	s := Store{Dir: filepath.Join(t.TempDir(), "keys")}
	k, err := s.Key(Touch)
	if errors.Is(err, ErrLocked) {
		t.Skip("the Mac is locked: a person's key can't be made until it is unlocked")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Remove(Touch) })
	if k.Protection() != schema.ProtectionHardware || len(k.SPKI()) == 0 {
		t.Errorf("the touch key: %q %d", k.Protection(), len(k.SPKI()))
	}
	device, err := s.Key(Device)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Remove(Device) })
	if bytes.Equal(device.SPKI(), k.SPKI()) {
		t.Error("the device key and the touch key are one key")
	}
}

// A store that keeps key files keeps one even on a Mac with the Secure
// Enclave, when it is told to.
func TestFileOnlyKeepsAFileOnAMac(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	k, err := s.Key(Device)
	if err != nil || k.Protection() != schema.ProtectionFile {
		t.Fatalf("%v %v", k, err)
	}
}

// A workload's key is in the Secure Enclave too, and works after the Mac's
// first unlock, behind a locked screen.
func TestTheSecureEnclaveKeepsAWorkloadsKey(t *testing.T) {
	if !seAvailable() {
		t.Skip("no Secure Enclave on this Mac")
	}
	s := Store{Dir: filepath.Join(t.TempDir(), "keys")}
	k, err := s.Key(Workload("planner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Remove(Workload("planner")) })
	sig, err := k.Sign([]byte("act"))
	if err != nil || k.Protection() != schema.ProtectionHardware {
		t.Fatalf("%v %q", err, k.Protection())
	}
	verify(t, k, []byte("act"), sig)
}
