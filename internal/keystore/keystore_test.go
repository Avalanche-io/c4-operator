package keystore

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// verify checks a signature is the key's, in the farm's one form.
func verify(t *testing.T, k Key, msg, sig []byte) {
	t.Helper()
	pub, err := x509.ParsePKIXPublicKey(k.SPKI())
	if err != nil {
		t.Fatal(err)
	}
	if err := provenance.VerifyP256(pub.(*ecdsa.PublicKey), msg, sig); err != nil {
		t.Errorf("the signature: %v", err)
	}
}

// A device key is made the first time it is asked for, as a key file only
// the person can read where there's no Secure Enclave; asked for again it
// is the same key; its signatures are the farm's one form. A key file
// can't need a touch, so it makes no touch key.
func TestAKeyFileIsMadeOnceAndSigns(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if s.Has(Device) {
		t.Fatal("a key before any was made")
	}
	k, err := s.Key(Device)
	if err != nil {
		t.Fatal(err)
	}
	if k.Protection() != schema.ProtectionFile {
		t.Errorf("protection %q", k.Protection())
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "device.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v %v", fi, err)
	}
	if di, err := os.Stat(s.Dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("the keys' directory: %v %v", di, err)
	}
	for i := range 32 {
		msg := []byte{byte(i), 'a', 'c', 't'}
		sig, err := k.Sign(msg)
		if err != nil {
			t.Fatal(err)
		}
		verify(t, k, msg, sig)
	}
	again, err := s.Key(Device)
	if err != nil || !bytes.Equal(again.SPKI(), k.SPKI()) {
		t.Fatalf("asked again: %v, the same key %v", err, err == nil && bytes.Equal(again.SPKI(), k.SPKI()))
	}
	if _, err := s.Key(Touch); !errors.Is(err, ErrNoTouch) {
		t.Errorf("a touch key from a key file: %v", err)
	}
	if err := s.Remove(Device); err != nil || s.Has(Device) {
		t.Errorf("removed: %v, still there %v", err, s.Has(Device))
	}
}

// A store never writes over a key file that is there: two makers at once
// keep one key.
func TestAKeyFileIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "device.key")
	if _, err := makeFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := makeFile(p); !errors.Is(err, os.ErrExist) {
		t.Errorf("a second key over the first: %v", err)
	}
}

// lowS folds a high S down, leaving a low one as it is.
func TestLowS(t *testing.T) {
	n := elliptic.P256().Params().N
	high := make([]byte, 64)
	high[31] = 1
	new(big.Int).Sub(n, big.NewInt(1)).FillBytes(high[32:])
	got := lowS(high)
	if got[63] != 1 || !bytes.Equal(got[:32], high[:32]) {
		t.Errorf("folded: %x", got)
	}
	low := append(make([]byte, 63), 5)
	if !bytes.Equal(lowS(low), low) {
		t.Error("a low S changed")
	}
}

// A workload's key is a key of its own, beside the person's, by its name.
func TestAWorkloadHasAKeyOfItsOwn(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	person, err := s.Key(Device)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.Key(Workload("planner"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(person.SPKI(), agent.SPKI()) || !s.Has(Workload("planner")) || s.Has(Workload("other")) {
		t.Error("the workload's key is not its own")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "workload-planner.key")); err != nil {
		t.Error(err)
	}
}

// A planted touch.key file is never the touch key, and is not replaced:
// a touch key is the Secure Enclave's. Only a file store (C4_KEYS=file)
// reads one.
func TestPlantedTouchKeyFileRefused(t *testing.T) {
	src := Store{Dir: filepath.Join(t.TempDir(), "src"), FileOnly: true}
	if _, err := src.Key(Device); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(src.Dir, "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: filepath.Join(t.TempDir(), "keys")}
	os.MkdirAll(s.Dir, 0o700)
	if err := os.WriteFile(filepath.Join(s.Dir, "touch.key"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Key(Touch); !errors.Is(err, ErrFileKey) || s.Has(Touch) {
		t.Errorf("a planted touch key: %v (has %v)", err, s.Has(Touch))
	}
	if got, _ := os.ReadFile(filepath.Join(s.Dir, "touch.key")); string(got) != string(b) {
		t.Error("the planted file was replaced")
	}
	if _, err := (Store{Dir: s.Dir, FileOnly: true}).Key(Touch); err != nil {
		t.Errorf("C4_KEYS=file reads its touch key file: %v", err)
	}
}

// Two first runs at once make one key: the second uses the first's.
func TestFirstRunsAtOnceMakeOneKey(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	type got struct {
		spki string
		err  error
	}
	keys := make(chan got, 8)
	for range 8 {
		go func() {
			k, err := s.Key(Device)
			if err != nil {
				keys <- got{err: err}
				return
			}
			keys <- got{spki: string(k.SPKI())}
		}()
	}
	seen := map[string]bool{}
	for range 8 {
		g := <-keys
		if g.err != nil {
			// a key half written, read by another first run
			t.Errorf("a first run failed: %v", g.err)
			continue
		}
		seen[g.spki] = true
	}
	if len(seen) > 1 {
		t.Errorf("%d keys from 8 first runs", len(seen))
	}
}

// WriteFile writes whole or not at all, the person's alone, and replaces
// a symlink at its path rather than writing through it.
func TestWriteFileReplacesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	os.WriteFile(target, []byte("theirs"), 0o644)
	p := filepath.Join(dir, "device.cert")
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("mine")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "theirs" {
		t.Errorf("wrote through the symlink: %q", b)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file: %v %v", err, fi.Mode())
	}
}

// Two first runs in two processes make one key, whole: helper processes
// of this test binary start together at a barrier and each makes the key.
func TestFirstRunsInProcessesMakeOneKey(t *testing.T) {
	if os.Getenv("C4_KEYSTORE_HELPER") != "" {
		t.Skip("a helper")
	}
	dir := filepath.Join(t.TempDir(), "keys")
	barrier := filepath.Join(t.TempDir(), "go")
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for range 6 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMakeKey$", "-test.count=1")
		cmd.Env = append(os.Environ(), "C4_KEYSTORE_HELPER="+dir, "C4_KEYSTORE_BARRIER="+barrier)
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, cmd), append(outs, out)
	}
	time.Sleep(200 * time.Millisecond)
	os.WriteFile(barrier, nil, 0o600)
	keys := map[string]bool{}
	for k, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", k, err, outs[k])
		}
		for _, l := range strings.Split(outs[k].String(), "\n") {
			if spki, ok := strings.CutPrefix(l, "spki "); ok {
				keys[spki] = true
			}
		}
	}
	if len(keys) != 1 {
		t.Errorf("%d keys from 6 processes: %v", len(keys), keys)
	}
}

// TestHelperMakeKey is one process of TestFirstRunsInProcessesMakeOneKey.
func TestHelperMakeKey(t *testing.T) {
	dir := os.Getenv("C4_KEYSTORE_HELPER")
	if dir == "" {
		t.Skip("run by TestFirstRunsInProcessesMakeOneKey")
	}
	for {
		if _, err := os.Stat(os.Getenv("C4_KEYSTORE_BARRIER")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	k, err := Store{Dir: dir, FileOnly: true}.Key(Device)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("spki %x\n", k.SPKI())
}
