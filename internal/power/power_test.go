package power

import (
	"os"
	"path/filepath"
	"testing"
)

// Work stops when the battery, not powered, falls below 25%, and resumes
// only at 90%: a charger arriving at 30% does not resume it, and a
// powered battery below 25% does not stop it.
func TestStopBelowResumeAt(t *testing.T) {
	p := DefaultPolicy(4)
	steps := []struct {
		r     Reading
		slots int
	}{
		{Reading{Percent: 60}, 4},
		{Reading{Percent: 25}, 4},
		{Reading{Percent: 24}, 0},
		{Reading{Percent: 30, Powered: true}, 0},
		{Reading{Percent: 89, Powered: true}, 0},
		{Reading{Percent: 90, Powered: true}, 4},
		{Reading{Percent: 70}, 4},
		{Reading{Percent: 20, Powered: true}, 4},
	}
	var s State
	for i, st := range steps {
		var n int
		var why string
		s, n, why = p.Step(s, st.r)
		if n != st.slots {
			t.Fatalf("step %d (%+v): %d slots (%s), want %d", i, st.r, n, why, st.slots)
		}
	}
	if _, _, why := p.Step(State{Stopped: true}, Reading{Percent: 40, Powered: true}); why != "battery 40%, on power: stopped below 25%, resumes at 90%" {
		t.Errorf("why %q", why)
	}
}

// macOS: a charger attached that delivers nothing (a loose plug) is not
// power.
func TestParseIOReg(t *testing.T) {
	const plugged = `"ExternalConnected" = Yes
  "PowerTelemetryData" = {"SystemPowerIn"=43600,"BatteryPower"=0}
  "MaxCapacity" = 100
  "CurrentCapacity" = 56`
	r, err := parseIOReg(plugged)
	if err != nil || r.Percent != 56 || !r.Powered {
		t.Fatalf("%+v %v", r, err)
	}
	loose := `"ExternalConnected" = Yes
  "PowerTelemetryData" = {"SystemPowerIn"=0}
  "MaxCapacity" = 100
  "CurrentCapacity" = 31`
	if r, _ := parseIOReg(loose); r.Powered || r.Percent != 31 {
		t.Errorf("loose plug %+v", r)
	}
	if _, err := parseIOReg("no battery here"); err != ErrNoBattery {
		t.Errorf("desktop: %v", err)
	}
}

func TestReadSysfs(t *testing.T) {
	dir := t.TempDir()
	put := func(dev, f, v string) {
		os.MkdirAll(filepath.Join(dir, dev), 0o755)
		os.WriteFile(filepath.Join(dir, dev, f), []byte(v+"\n"), 0o644)
	}
	put("BAT0", "type", "Battery")
	put("BAT0", "capacity", "42")
	put("AC", "type", "Mains")
	put("AC", "online", "1")
	if r, err := readSysfs(dir); err != nil || r.Percent != 42 || !r.Powered {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := readSysfs(t.TempDir()); err != ErrNoBattery {
		t.Errorf("no battery: %v", err)
	}
}

// The slots file is written whole, as the runner reads it, and only when
// it changes.
func TestWriteSlotsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "agents", "slots")
	if changed, err := Write(f, 0, "battery 24%: stopped below 25%, resumes at 90%"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if b, _ := os.ReadFile(f); string(b) != "0 battery 24%: stopped below 25%, resumes at 90%\n" {
		t.Errorf("file %q", b)
	}
	if changed, _ := Write(f, 0, "battery 24%: stopped below 25%, resumes at 90%"); changed {
		t.Error("an unchanged write reported a change")
	}
}
