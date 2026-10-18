package farmcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
)

func archive(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestSamplesFromTheRelease(t *testing.T) {
	data := archive(t, map[string]string{"saltmarsh/sm0140/sm0140.usda": "#usda 1.0\n", "saltmarsh/.c4project": ""})
	id := c4.Identify(bytes.NewReader(data)).String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.1.0/c4-samples-v0.1.0.tar.gz":
			w.Write(data)
		case "/v0.1.0/c4-samples-v0.1.0.tar.gz.c4":
			w.Write([]byte(id + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := SamplesBase
	SamplesBase = srv.URL
	defer func() { SamplesBase = old }()

	dir := t.TempDir()
	var out bytes.Buffer
	if err := GetSamples(context.Background(), Out{W: &out}, SamplesArgs{Version: "v0.1.0", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "saltmarsh", "sm0140", "sm0140.usda")); err != nil || string(b) != "#usda 1.0\n" {
		t.Fatalf("unpacked %q %v", b, err)
	}
	if !strings.Contains(out.String(), "2 files") || !strings.Contains(out.String(), "checked") {
		t.Fatalf("output %s", out.String())
	}
	// a tampered archive is refused before anything is unpacked
	data = archive(t, map[string]string{"saltmarsh/evil.txt": "x"})
	dir2 := t.TempDir()
	if err := GetSamples(context.Background(), Out{W: &out}, SamplesArgs{Version: "v0.1.0", Dir: dir2}); err == nil {
		t.Fatal("an archive that does not match its ID was unpacked")
	}
	if _, err := os.Stat(filepath.Join(dir2, "saltmarsh")); err == nil {
		t.Fatal("something was unpacked")
	}
}

func TestSamplesRefuseEscapesAndLinks(t *testing.T) {
	for name, data := range map[string][]byte{
		"dotdot":   archive(t, map[string]string{"../outside.txt": "x"}),
		"absolute": archive(t, map[string]string{"/etc/c4": "x"}),
		"symlink":  archive(t, nil, &tar.Header{Name: "saltmarsh/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}),
	} {
		f := filepath.Join(t.TempDir(), "a.tar.gz")
		os.WriteFile(f, data, 0o644)
		if err := GetSamples(context.Background(), Out{W: &bytes.Buffer{}}, SamplesArgs{From: f, Dir: t.TempDir()}); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	if err := GetSamples(context.Background(), Out{W: &bytes.Buffer{}}, SamplesArgs{Set: "moth", Dir: t.TempDir()}); err == nil {
		t.Error("an unknown set was fetched")
	}
}
