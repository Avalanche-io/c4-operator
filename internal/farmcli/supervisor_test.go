package farmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// agent start installs one service that runs c4 farm agent run with
// absolute paths, replacing one before it; agent stop removes it.
func TestAgentServiceStartStop(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			var calls []string
			svc := &AgentService{FarmBin: "/usr/local/bin/c4-farm", AgentBin: "/usr/local/bin/c4-agent", Home: t.TempDir(), OS: goos,
				Ctl: func(_ context.Context, name string, args ...string) error {
					calls = append(calls, name+" "+strings.Join(args, " "))
					return nil
				}}
			var out bytes.Buffer
			if err := svc.Start(context.Background(), Out{W: &out}); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(svc.path())
			if err != nil {
				t.Fatal(err)
			}
			def := string(b)
			for _, want := range []string{"/usr/local/bin/c4-farm", "agent", "run", "--agent-bin", "/usr/local/bin/c4-agent", filepath.Join(svc.Home, ".c4", "agents", "agent.log")} {
				if !strings.Contains(def, want) {
					t.Errorf("no %q in\n%s", want, def)
				}
			}
			joined := strings.Join(calls, "\n")
			switch goos {
			case "darwin":
				if !strings.Contains(def, "<key>SuccessfulExit</key><false/>") || !strings.Contains(joined, "launchctl bootout gui/") ||
					!strings.Contains(joined, "launchctl bootstrap gui/") {
					t.Errorf("launchd: %s\n%s", joined, def)
				}
			case "linux":
				if !strings.Contains(def, "Restart=on-failure") || !strings.Contains(joined, "systemctl --user enable --now c4-agent.service") {
					t.Errorf("systemd: %s\n%s", joined, def)
				}
			}
			out.Reset()
			if err := svc.Stop(context.Background(), Out{W: &out}); err != nil || !strings.Contains(out.String(), "stopped and removed") {
				t.Fatalf("stop: %v %s", err, out.String())
			}
			if _, err := os.Stat(svc.path()); !os.IsNotExist(err) {
				t.Error("the service file is still there")
			}
		})
	}
}

// agent run starts the runner again when it stops, and stops trying once
// the runner says the machine must be enrolled again.
func TestAgentRunRestartsTheRunner(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "c4-agent")
	slots := filepath.Join(dir, "slots")
	// the runner reads what the power reader writes: wait for it, as the
	// real one does, so the run cannot end before the reader has written
	os.WriteFile(bin, []byte(`#!/bin/sh
i=0; while [ ! -f `+slots+` ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done
n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+count+`
if [ $n -lt 2 ]; then echo "runner up, then it fell over"; exit 1; fi
echo "the machine's token has expired: enroll it again (c4 farm up)"; exit 1
`), 0o755)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := AgentRun(ctx, Out{W: &out}, AgentRunArgs{AgentBin: bin, SlotsFile: slots, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(count); strings.TrimSpace(string(b)) != "2" {
		t.Errorf("the runner ran %s times", b)
	}
	if s := out.String(); !strings.Contains(s, "starting it again") || !strings.Contains(s, "run c4 farm up to enroll it again") {
		t.Errorf("output:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "slots")); err != nil {
		t.Errorf("the power reader wrote no slots file: %v", err)
	}
}

// A runner whose enrollment lapsed says so and keeps running, as the real
// one does: agent run stops it at once and takes the service's definition
// away, so it is not started again at the next sign-in.
func TestAgentRunStopsALapsedRunner(t *testing.T) {
	dir := t.TempDir()
	slots, svc := filepath.Join(dir, "slots"), filepath.Join(dir, "io.avalanche.c4.agent.plist")
	os.WriteFile(svc, []byte("<plist/>"), 0o644)
	bin := filepath.Join(dir, "c4-agent")
	os.WriteFile(bin, []byte("#!/bin/sh\nwhile :; do echo \"the machine's token has expired: enroll it again (c4 farm up)\"; sleep 1; done\n"), 0o755)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if err := AgentRun(ctx, Out{W: &out}, AgentRunArgs{AgentBin: bin, SlotsFile: slots, Backoff: time.Millisecond, ServiceFile: svc, Log: filepath.Join(dir, "agent.log")}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second || !strings.Contains(out.String(), "run c4 farm up to enroll it again") {
		t.Errorf("after %s:\n%s", time.Since(start), out.String())
	}
	if _, err := os.Stat(svc); !os.IsNotExist(err) {
		t.Error("the service's definition is still there")
	}
}

// The service finds claude and c4-agent under launchd and systemd, whose
// PATH is the system's alone; starting a service running as defined
// leaves it alone.
func TestAgentServicePathAndRestart(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		var calls []string
		svc := &AgentService{FarmBin: "/opt/c4/bin/c4-farm", AgentBin: "/opt/c4/bin/c4-agent", Home: t.TempDir(), OS: goos,
			Ctl: func(_ context.Context, name string, args ...string) error {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return nil
			}}
		var out bytes.Buffer
		if err := svc.Start(context.Background(), Out{W: &out}); err != nil {
			t.Fatal(err)
		}
		if def := svc.definition(); !strings.Contains(def, "/opt/c4/bin:") || !strings.Contains(def, "/usr/bin:/bin") ||
			!(strings.Contains(def, "<key>HOME</key><string>"+svc.Home+"</string>") || strings.Contains(def, strconv.Quote("HOME="+svc.Home))) {
			t.Errorf("%s: no PATH in\n%s", goos, def)
		}
		calls = nil
		out.Reset()
		if err := svc.Start(context.Background(), Out{W: &out}); err != nil || !strings.Contains(out.String(), "running already") {
			t.Errorf("%s: second start: %v\n%s", goos, err, out.String())
		}
		for _, c := range calls {
			if strings.Contains(c, "bootout") || strings.Contains(c, "bootstrap") || strings.Contains(c, "enable") {
				t.Errorf("%s: restarted a running service: %v", goos, calls)
			}
		}
	}
}

// A service installed from a HOME that is not the account's (a test's or
// script's temp folder) would run in the real session from there: refused
// before anything is written or loaded, unless named.
func TestAgentServiceRefusesAForeignHome(t *testing.T) {
	home := t.TempDir()
	svc := &AgentService{FarmBin: "/f", AgentBin: "/a", Home: home, OS: runtime.GOOS}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service here")
	}
	err := svc.Start(context.Background(), Out{W: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "not this account's home") {
		t.Fatalf("foreign home: %v", err)
	}
	if _, err := os.Stat(svc.path()); !os.IsNotExist(err) {
		t.Error("the service file was written")
	}
}
