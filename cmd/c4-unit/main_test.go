package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Each safe point is numbered and timed in .c4/safepoint. The unit then
// waits for the farm's decision on it; a pending soft stop is advisory,
// and is followed only when no decision arrives in time.
func TestSafepoint(t *testing.T) {
	out, ctl := t.TempDir(), t.TempDir()
	info := filepath.Join(t.TempDir(), "annotations")
	os.WriteFile(info, []byte("c4.avalanche.io/yield=\"reclaim-nominal\"\nc4.avalanche.io/yield-by=\"2026-10-19T20:10:00Z\"\n"), 0o644)
	clock := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	e := vars(map[string]string{"C4_OUTPUT": out, "C4_PODINFO": info, "C4_CONTROL": ctl})
	e.now = func() time.Time { return clock }
	e.sleep = func(d time.Duration) { clock = clock.Add(d) }
	decide := func(seq int, do, why string) {
		os.WriteFile(filepath.Join(ctl, "safepoint.decision"), []byte(fmt.Sprintf(`{"seq":%d,"do":%q,"why":%q}`, seq, do, why)), 0o644)
	}
	var buf bytes.Buffer
	// The farm says continue, though a soft stop was asked for: capacity
	// came back.
	decide(1, "continue", "gw010-light no longer needs the slots")
	if c, err := run([]string{"safepoint", "--checkpoint", "step-100"}, &buf, e); c != 0 || err != nil || buf.String() != "continue: gw010-light no longer needs the slots\n" {
		t.Fatalf("continue decision: %d %v %q", c, err, buf.String())
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "safepoint"))
	if !strings.HasPrefix(string(b), "seq 1\nat ") || !strings.HasSuffix(string(b), "checkpoint step-100\n") {
		t.Fatalf("safepoint file %q", b)
	}
	// The farm says stop.
	decide(2, "stop", "reclaim-nominal for gw010-light")
	buf.Reset()
	if c, _ := run([]string{"safepoint"}, &buf, e); c != stopCode || buf.String() != "stop: reclaim-nominal for gw010-light\n" {
		t.Fatalf("stop decision: %d %q", c, buf.String())
	}
	// No decision for safe point 3 within the wait (the file still holds
	// the answer to 2): the pending soft stop stands.
	buf.Reset()
	start := clock
	if c, _ := run([]string{"safepoint", "--wait", "5s"}, &buf, e); c != stopCode || !strings.HasPrefix(buf.String(), "stop: reclaim-nominal (by 2026-10-19T20:10:00Z) [no decision in 5s]") {
		t.Fatalf("fallback: %d %q", c, buf.String())
	}
	if clock.Sub(start) < 5*time.Second {
		t.Fatalf("waited %v, not the 5s asked", clock.Sub(start))
	}
	// Without a control directory, nor a pending stop: continue at once.
	buf.Reset()
	if c, _ := run([]string{"safepoint"}, &buf, vars(map[string]string{"C4_OUTPUT": out})); c != 0 || buf.String() != "continue [no decision channel]\n" {
		t.Fatalf("no farm: %d %q", c, buf.String())
	}
}

func TestMeasure(t *testing.T) {
	out := t.TempDir()
	e := vars(map[string]string{"C4_OUTPUT": out})
	if c, err := run([]string{"measure", "--of", "379", "--name", "rules sited", "--detail", "18 pending", "361"}, nil, e); c != 0 || err != nil {
		t.Fatal(c, err)
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "measure.json"))
	var got struct {
		Value, Of    int64
		Name, Detail string
	}
	if err := json.Unmarshal(b, &got); err != nil || got.Value != 361 || got.Of != 379 || got.Name != "rules sited" || got.Detail != "18 pending" {
		t.Fatalf("measure %s %v", b, err)
	}
	for _, bad := range [][]string{{"measure"}, {"measure", "-3"}, {"measure", "lots"}} {
		if c, _ := run(bad, nil, e); c != 2 {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestUsageAdds(t *testing.T) {
	out := t.TempDir()
	e := vars(map[string]string{"C4_OUTPUT": out})
	for _, args := range [][]string{
		{"usage", "--model", "opus", "--input", "1000", "--output", "200", "--cost", "0.5", "--cost-source", "price-table"},
		{"usage", "--model", "opus", "--input", "500", "--output", "100", "--cache-read", "4000", "--cost", "0.25"},
		{"usage", "--model", "haiku", "--input", "300"},
	} {
		if c, err := run(args, nil, e); c != 0 || err != nil {
			t.Fatal(c, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "usage.json"))
	var u struct {
		Models []struct {
			Model                                      string
			InputTokens, OutputTokens, CacheReadTokens int64
			CostUSD                                    *float64
		}
		CostSource string
	}
	if err := json.Unmarshal(b, &u); err != nil || len(u.Models) != 2 || u.CostSource != "price-table" {
		t.Fatalf("usage %s %v", b, err)
	}
	o, h := u.Models[0], u.Models[1]
	if o.Model != "opus" || o.InputTokens != 1500 || o.OutputTokens != 300 || o.CacheReadTokens != 4000 || o.CostUSD == nil || *o.CostUSD != 0.75 ||
		h.Model != "haiku" || h.InputTokens != 300 || h.CostUSD != nil {
		t.Fatalf("usage %s", b)
	}
	if c, _ := run([]string{"usage", "--input", "5"}, nil, e); c != 2 {
		t.Fatal("usage without a model accepted")
	}
}

// A unit asks for a wait at a safe point: the wait goes into
// .c4/safepoint as one line of JSON, and the farm's stop means it waits;
// a continue means the farm refused it. A resumed attempt reads the
// decision.
func TestWait(t *testing.T) {
	out, ctl := t.TempDir(), t.TempDir()
	clock := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	e := vars(map[string]string{"C4_OUTPUT": out, "C4_CONTROL": ctl})
	e.now = func() time.Time { return clock }
	e.sleep = func(d time.Duration) { clock = clock.Add(d) }
	os.WriteFile(filepath.Join(ctl, "safepoint.decision"), []byte(`{"seq":1,"do":"stop","why":"waits for person:owner"}`), 0o644)
	var buf bytes.Buffer
	c, err := run([]string{"wait", "--server", "person:owner", "--ask", "split the\nchapter?", "--choices", "split,keep", "--reply", "--expect", "2h", "--checkpoint", "turn-12"}, &buf, e)
	if c != stopCode || err != nil || buf.String() != "waits: waits for person:owner\n" {
		t.Fatalf("wait: %d %v %q", c, err, buf.String())
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "safepoint"))
	want := "checkpoint turn-12\n" + `wait {"ask":"split the\nchapter?","choices":["split","keep"],"expect":7200,"reply":true,"server":"person:owner"}` + "\n"
	if !strings.HasPrefix(string(b), "seq 1\nat ") || !strings.HasSuffix(string(b), want) {
		t.Fatalf("safepoint file %q", b)
	}
	os.WriteFile(filepath.Join(ctl, "safepoint.decision"), []byte(`{"seq":2,"do":"continue","why":"unit draft asks for a wait: unknown field"}`), 0o644)
	buf.Reset()
	if c, _ := run([]string{"wait", "--server", "person:owner"}, &buf, e); c != 1 || !strings.HasPrefix(buf.String(), "refused: ") {
		t.Fatalf("refused: %d %q", c, buf.String())
	}
	if c, _ := run([]string{"wait", "--ask", "who?"}, &buf, e); c != 2 {
		t.Fatalf("no server: %d", c)
	}
	buf.Reset()
	if c, _ := run([]string{"decision"}, &buf, e); c != 1 || buf.Len() != 0 {
		t.Fatalf("no decision: %d %q", c, buf.String())
	}
	d := `{"seq":1,"do":"split","actor":"owner"}`
	if c, _ := run([]string{"decision"}, &buf, vars(map[string]string{"C4_DECISION": d})); c != 0 || buf.String() != d+"\n" {
		t.Fatalf("decision: %d %q", c, buf.String())
	}
}

// A unit reads the messages sent to it while it runs: those after the last
// it read, each once.
func TestSteer(t *testing.T) {
	out, ctl := t.TempDir(), t.TempDir()
	e := vars(map[string]string{"C4_OUTPUT": out, "C4_CONTROL": ctl})
	var buf bytes.Buffer
	if c, err := run([]string{"steer"}, &buf, e); c != 1 || err != nil || buf.Len() != 0 {
		t.Fatalf("no messages: %d %v %q", c, err, buf.String())
	}
	os.WriteFile(filepath.Join(ctl, "steer"), []byte(`[{"seq":1,"at":"2026-10-19T20:00:00Z","actor":"dana","text":"skip the lighting pass"},
		{"seq":2,"at":"2026-10-19T20:05:00Z","actor":"dana","text":"and use the v3 plates"}]`), 0o644)
	if c, err := run([]string{"steer"}, &buf, e); c != 0 || err != nil ||
		buf.String() != "dana (2026-10-19T20:00:00Z): skip the lighting pass\ndana (2026-10-19T20:05:00Z): and use the v3 plates\n" {
		t.Fatalf("steer: %d %v %q", c, err, buf.String())
	}
	if b, _ := os.ReadFile(filepath.Join(out, ".c4", "steer-seen")); string(b) != "2\n" {
		t.Fatalf("steer-seen %q", b)
	}
	buf.Reset()
	if c, _ := run([]string{"steer"}, &buf, e); c != 1 || buf.Len() != 0 {
		t.Fatalf("read again: %d %q", c, buf.String())
	}
	os.WriteFile(filepath.Join(ctl, "steer"), []byte(`[{"seq":3,"at":"2026-10-19T20:10:00Z","actor":"dana","text":"done?"}]`), 0o644)
	if c, _ := run([]string{"steer", "--json"}, &buf, e); c != 0 || !strings.HasPrefix(buf.String(), `{"seq":3,`) {
		t.Fatalf("json: %d %q", c, buf.String())
	}
}
