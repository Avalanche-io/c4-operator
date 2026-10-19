package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vars(m map[string]string) env {
	return env{getenv: func(k string) string { return m[k] }, mountinfo: "/nonexistent"}
}

func TestProgress(t *testing.T) {
	out := t.TempDir()
	e := vars(map[string]string{"C4_OUTPUT": out})
	if c, err := run([]string{"progress", "0.4"}, nil, e); c != 0 || err != nil {
		t.Fatal(c, err)
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "progress"))
	if string(b) != "0.4\n" {
		t.Fatalf("progress %q", b)
	}
	for _, bad := range []string{"1.5", "-0.1", "half"} {
		if c, _ := run([]string{"progress", bad}, nil, e); c != 2 {
			t.Errorf("progress %s accepted", bad)
		}
	}
}

func TestPreviewOfAPNG(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(t.TempDir(), "sm0140.1024.png")
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	png.Encode(&buf, img)
	os.WriteFile(src, buf.Bytes(), 0o644)
	if c, err := run([]string{"preview", src, "1024"}, nil, vars(map[string]string{"C4_OUTPUT": out})); c != 0 || err != nil {
		t.Fatal(c, err)
	}
	f, err := os.Open(filepath.Join(out, "preview", "1024.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := png.Decode(f)
	if err != nil || got.Bounds().Dx() != 960 || got.Bounds().Dy() != 540 {
		t.Fatalf("preview %v %v", got.Bounds(), err)
	}
}

func TestPreviewOfAnEXRUsesOIIO(t *testing.T) {
	out := t.TempDir()
	tools := t.TempDir()
	fake := filepath.Join(tools, "oiiotool")
	// a stand-in that records its arguments and writes its -o file
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > \""+tools+"/args\"\nfor a; do last=\"$a\"; done\necho png > \"$last\"\n"), 0o755)
	e := vars(map[string]string{"C4_OUTPUT": out})
	e.oiiotool = fake
	if c, err := run([]string{"preview", "/show/render/sm0140.1001.exr", "1001"}, nil, e); c != 0 || err != nil {
		t.Fatal(c, err)
	}
	args, _ := os.ReadFile(filepath.Join(tools, "args"))
	if !strings.Contains(string(args), "ocio://studio-config-latest") || !strings.Contains(string(args), "--resize 960x0") {
		t.Fatalf("oiiotool %s", args)
	}
	if _, err := os.Stat(filepath.Join(out, "preview", "1001.png")); err != nil {
		t.Fatal(err)
	}
}

func TestYieldRequested(t *testing.T) {
	info := filepath.Join(t.TempDir(), "annotations")
	os.WriteFile(info, []byte("c4.avalanche.io/attempt=\"1\"\n"), 0o644)
	e := vars(map[string]string{"C4_PODINFO": info})
	if c, _ := run([]string{"yield-requested"}, nil, e); c != 1 {
		t.Fatal("yield before it was asked")
	}
	os.WriteFile(info, []byte("c4.avalanche.io/attempt=\"1\"\nc4.avalanche.io/yield=\"glassworks/gw010-light-v007 needs gpu\"\n"), 0o644)
	if c, _ := run([]string{"yield-requested"}, nil, e); c != 0 {
		t.Fatal("yield not seen")
	}
	if c, _ := run([]string{"yield-requested"}, nil, vars(nil)); c != 1 {
		t.Fatal("a unit with no podinfo was asked to yield")
	}
}

func TestResumeDir(t *testing.T) {
	var out bytes.Buffer
	run([]string{"resume-dir"}, &out, vars(map[string]string{"C4_OUTPUT": "/work"}))
	if out.Len() != 0 {
		t.Fatalf("a first attempt printed %q", out.String())
	}
	run([]string{"resume-dir"}, &out, vars(map[string]string{"C4_OUTPUT": "/work", "C4_RESUME_VIEW": "c4abc"}))
	if out.String() != "/work\n" {
		t.Fatalf("resume %q", out.String())
	}
}

func TestOutputFromMounts(t *testing.T) {
	mi := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(mi, []byte(strings.Join([]string{
		`1 0 0:1 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/containerd/x`,
		`2 1 0:2 / /show rw,relatime - overlay c4 rw,lowerdir=/var/lib/c4/layers/a:/var/lib/c4/layers/b,upperdir=/var/lib/c4/uppers/p`,
		`3 1 0:3 / /ref ro,relatime - overlay c4 ro,lowerdir=/var/lib/c4/layers/c`,
		`4 1 0:4 / /etc/c4/podinfo ro - tmpfs tmpfs ro`,
	}, "\n")+"\n"), 0o644)
	e := env{getenv: func(string) string { return "" }, mountinfo: mi}
	var out bytes.Buffer
	if c, err := run([]string{"output"}, &out, e); c != 0 || err != nil || out.String() != "/show\n" {
		t.Fatalf("output %q %d %v", out.String(), c, err)
	}
	if got := unescape(`/my\040show`); got != "/my show" {
		t.Fatalf("unescape %q", got)
	}
}

// Each safe point is numbered and timed in .c4/safepoint; the command says
// continue until the farm asks for a soft stop, then stop, with its reason
// and deadline, and exit status 10.
func TestSafepoint(t *testing.T) {
	out := t.TempDir()
	info := filepath.Join(t.TempDir(), "annotations")
	os.WriteFile(info, []byte("c4.avalanche.io/attempt=\"1\"\n"), 0o644)
	e := vars(map[string]string{"C4_OUTPUT": out, "C4_PODINFO": info})
	var buf bytes.Buffer
	if c, err := run([]string{"safepoint"}, &buf, e); c != 0 || err != nil || buf.String() != "continue\n" {
		t.Fatalf("first safe point: %d %v %q", c, err, buf.String())
	}
	buf.Reset()
	if c, _ := run([]string{"safepoint", "--checkpoint", "step-4000"}, &buf, e); c != 0 {
		t.Fatalf("second safe point: %d", c)
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "safepoint"))
	if !strings.HasPrefix(string(b), "seq 2\nat ") || !strings.HasSuffix(string(b), "checkpoint step-4000\n") {
		t.Fatalf("safepoint file %q", b)
	}
	os.WriteFile(info, []byte("c4.avalanche.io/yield=\"reclaim-nominal\"\nc4.avalanche.io/yield-by=\"2026-10-19T20:10:00Z\"\n"), 0o644)
	buf.Reset()
	c, err := run([]string{"safepoint"}, &buf, e)
	if c != stopCode || err != nil || buf.String() != "stop: reclaim-nominal (by 2026-10-19T20:10:00Z)\n" {
		t.Fatalf("under a soft stop: %d %v %q", c, err, buf.String())
	}
	if b, _ := os.ReadFile(filepath.Join(out, ".c4", "safepoint")); !strings.HasPrefix(string(b), "seq 3\n") {
		t.Fatalf("the stopping safe point is recorded too: %q", b)
	}
}
