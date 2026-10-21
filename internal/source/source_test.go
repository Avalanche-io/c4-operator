package source_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/source"
)

func TestFilter(t *testing.T) {
	f := source.Filter{Include: []string{"*.safetensors", "*.json", "onnx/*"}, Exclude: []string{"onnx/fp16*"}}
	for p, want := range map[string]bool{
		"model.safetensors": true, "unet/diffusion.safetensors": true, "config.json": true,
		"onnx/model.onnx": true, "onnx/fp16.onnx": false, "pytorch_model.bin": false, "README.md": false,
	} {
		if f.Takes(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
	if !(source.Filter{}).Takes("anything/at/all") {
		t.Error("an empty filter takes every file")
	}
	if err := (source.Filter{Include: []string{"[bad"}}).Check(); err == nil {
		t.Error("a malformed pattern is an error")
	}
}

// Store checks size and hash in the one pass that computes the C4 ID, and
// puts nothing that fails.
func TestStore(t *testing.T) {
	body := []byte("the weights")
	sum := sha256.Sum256(body)
	var put []c4.ID
	p := func(_ context.Context, id c4.ID, size int64, r io.Reader) (bool, error) {
		b, _ := io.ReadAll(r)
		if int64(len(b)) != size {
			return false, errors.New("short")
		}
		put = append(put, id)
		return true, nil
	}
	ok := source.File{Path: "m.safetensors", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	got, err := source.Store(context.Background(), ok, bytes.NewReader(body), t.TempDir(), p)
	if err != nil || got.ID != c4.Identify(bytes.NewReader(body)) || !got.Stored {
		t.Fatalf("stored %+v %v", got, err)
	}
	for name, f := range map[string]source.File{
		"wrong hash": {Path: "a", Size: int64(len(body)), SHA256: strings.Repeat("0", 64)},
		"wrong size": {Path: "b", Size: 3, SHA256: ok.SHA256},
		"wrong blob": {Path: "c", Size: int64(len(body)), GitBlob: strings.Repeat("0", 40)},
	} {
		if _, err := source.Store(context.Background(), f, bytes.NewReader(body), t.TempDir(), p); !errors.Is(err, source.ErrMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := source.Store(context.Background(), source.File{Path: "d", Size: 1}, bytes.NewReader(body), t.TempDir(), p); err == nil {
		t.Error("a file with no hash is refused")
	}
	if len(put) != 1 {
		t.Fatalf("put %d objects; only the matching one belongs in the store", len(put))
	}
}
