package node

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadProgress(t *testing.T) {
	upper := t.TempDir()
	if _, ok := ReadProgress(upper); ok {
		t.Fatal("progress without a file")
	}
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	for in, want := range map[string]float64{"0.4\n": 0.4, " 1 ": 1, "1.7": 1, "-2": 0, "0.25\n0.5\n": 0.25} {
		os.WriteFile(filepath.Join(upper, ".c4", "progress"), []byte(in), 0o644)
		got, ok := ReadProgress(upper)
		if !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"half", "", "NaN"} {
		os.WriteFile(filepath.Join(upper, ".c4", "progress"), []byte(bad), 0o644)
		if _, ok := ReadProgress(upper); ok {
			t.Errorf("%q read as progress", bad)
		}
	}
}

func TestReadSafepoint(t *testing.T) {
	upper := t.TempDir()
	if _, ok := ReadSafepoint(upper); ok {
		t.Fatal("a safe point without a file")
	}
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	os.WriteFile(filepath.Join(upper, ".c4", "safepoint"), []byte("seq 7\nat 2026-10-19T20:01:02.5Z\ncheckpoint step-700\n"), 0o644)
	sp, ok := ReadSafepoint(upper)
	if !ok || sp.Seq != 7 || !sp.At.Equal(time.Date(2026, 10, 19, 20, 1, 2, 5e8, time.UTC)) {
		t.Fatalf("%+v %v", sp, ok)
	}
	os.WriteFile(filepath.Join(upper, ".c4", "safepoint"), []byte("garbage\n"), 0o644)
	if _, ok := ReadSafepoint(upper); ok {
		t.Fatal("a malformed safe point read")
	}
}
