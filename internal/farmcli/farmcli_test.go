package farmcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func eastern(t *testing.T) Clock {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 14, 10, 0, 0, ny) // a Thursday
	return Clock{Now: func() time.Time { return now }, Loc: ny}
}

func TestParseWhen(t *testing.T) {
	c := eastern(t)
	ny := c.Loc
	for in, want := range map[string]time.Time{
		"18:00":                     time.Date(2026, 10, 22, 18, 0, 0, 0, ny),
		"09:00":                     time.Date(2026, 10, 23, 9, 0, 0, 0, ny), // passed today: tomorrow
		"fri 18:00":                 time.Date(2026, 10, 23, 18, 0, 0, 0, ny),
		"Thursday 09:00":            time.Date(2026, 10, 29, 9, 0, 0, 0, ny), // a week on
		"+3h":                       time.Date(2026, 10, 22, 17, 10, 0, 0, ny),
		"+90m":                      time.Date(2026, 10, 22, 15, 40, 0, 0, ny),
		"2026-10-23T18:00:00-07:00": time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC),
		"2026-10-23T22:00:00Z":      time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC),
	} {
		got, err := c.ParseWhen(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	if got, err := c.ParseWhen("suggested"); err != nil || !got.IsZero() {
		t.Errorf("suggested: %v %v", got, err)
	}
	for _, bad := range []string{"25:00", "noon", "fri"} {
		if _, err := c.ParseWhen(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTimesAreLocalWithUTCBeside(t *testing.T) {
	c := eastern(t)
	at := time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC) // 18:00 EDT
	if got := c.AtUTC(at); got != "18:00 EDT today (22:00 UTC)" {
		t.Errorf("AtUTC %q", got)
	}
	if got := c.At(at.Add(24 * time.Hour)); got != "Fri 18:00 EDT" {
		t.Errorf("At tomorrow %q", got)
	}
	if got := c.At(at.Add(10 * 24 * time.Hour)); got != "Sun 1 Nov 17:00 EST" {
		t.Errorf("At later %q", got)
	}
	c.UTC = true
	if got := c.AtUTC(at); got != "22:00 UTC today" {
		t.Errorf("--utc %q", got)
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Second: "40s", 8 * time.Minute: "8m", 3*time.Hour + 8*time.Minute: "3h 8m",
		3 * time.Hour: "3h", 14 * time.Hour: "14h", 72 * time.Hour: "3d", -31 * time.Minute: "-31m",
	} {
		if got := Span(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestTypes(t *testing.T) {
	r, _ := LookupType("render")
	spec, err := r.Spec(Options{Asset: "sm0140/sm0140.usda", Frames: "1001-1048", Renderer: "karma-xpu"})
	if err != nil {
		t.Fatal(err)
	}
	args := spec.Template.Spec.Containers[0].Args[0]
	if spec.Params["order"] != "binary" || spec.Params["renderer"] != "karma-xpu" || !strings.Contains(args, "--renderer BRAY_HdKarmaXPU") ||
		!strings.Contains(args, "--imageWidth 1920") || *spec.Template.Spec.SecurityContext.RunAsUser != 65532 {
		t.Fatalf("render spec %v\n%s", spec.Params, args)
	}
	if _, err := r.Spec(Options{Asset: "a.usda"}); err == nil {
		t.Fatal("a render without frames was accepted")
	}
	if _, err := r.Spec(Options{Asset: "a.usda", Frames: "1-4", Renderer: "mantra"}); err == nil {
		t.Fatal("an unknown renderer was accepted")
	}
	sim, _ := LookupType("sim")
	spec, err = sim.Spec(Options{Asset: "fog.blend", Frames: "1001-1120"})
	if err != nil || spec.Params["chunk"] != "120" || spec.Params["order"] != "sequential" {
		t.Fatalf("sim: one unit over the range: %v %v", spec.Params, err)
	}
	tr, _ := LookupType("transcode")
	if _, err := tr.Spec(Options{Asset: "plates", Frames: "1-48", Set: map[string]string{"codec": "dnxhr-hqx"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Spec(Options{Asset: "plates", Frames: "1-48", Set: map[string]string{"bitrate": "9"}}); err == nil {
		t.Fatal("an unknown --set was accepted")
	}
	lora, _ := LookupType("lora")
	spec, _ = lora.Spec(Options{Asset: "."})
	if spec.Class != "ml" || spec.Pools[0] != "gpu" || spec.Template.Spec.Containers[0].WorkingDir != "/work" {
		t.Fatalf("lora %+v", spec)
	}
	if _, err := LookupType("blender"); err == nil {
		t.Fatal("an unknown type was found")
	}
}

func TestFindRoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sm0140", "lights"), 0o755)
	os.WriteFile(filepath.Join(dir, ".c4project"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "sm0140", "sm0140.usda"), []byte("x"), 0o644)
	root, rel, err := FindRoot(filepath.Join(dir, "sm0140", "sm0140.usda"), "")
	if err != nil || root != dir || rel != "sm0140/sm0140.usda" {
		t.Fatalf("root %s rel %s %v", root, rel, err)
	}
	root, rel, err = FindRoot(filepath.Join(dir, "sm0140"), filepath.Join(dir, "sm0140"))
	if err != nil || rel != "." {
		t.Fatalf("a folder asset: %s %s %v", root, rel, err)
	}
	if _, _, err := FindRoot(filepath.Join(dir, "sm0140", "sm0140.usda"), filepath.Join(dir, "sm0140", "lights")); err == nil {
		t.Fatal("an asset outside its root was accepted")
	}
}

func TestConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "farm.yaml")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Resolve(""); err == nil || !strings.Contains(err.Error(), "c4 farm up") {
		t.Fatalf("empty config: %v", err)
	}
	c.Farms["c4-local"] = Farm{HUD: "http://127.0.0.1:30080", As: "artist", Project: "saltmarsh", Local: true}
	c.Farms["studio"] = Farm{HUD: "https://hud.example", TokenFile: "~/.c4/studio.token"}
	c.Current = "c4-local"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadConfig(p)
	name, f, err := c2.Resolve("")
	if err != nil || name != "c4-local" || f.As != "artist" {
		t.Fatalf("resolve %s %+v %v", name, f, err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", info.Mode())
	}
	if _, _, err := c2.Resolve("nope"); err == nil {
		t.Fatal("an unknown farm resolved")
	}
}
