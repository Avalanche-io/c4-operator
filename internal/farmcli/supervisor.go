package farmcli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/power"
)

// AgentLabel names the machine's agent service: a launchd agent on macOS,
// a systemd user unit on Linux.
const AgentLabel = "io.avalanche.c4.agent"

// AgentRunArgs are `c4 farm agent run`'s: the machine's agent duties, as
// the service runs them.
type AgentRunArgs struct {
	// AgentBin is c4-agent (default c4-agent on PATH).
	AgentBin string
	// Slots are what the runner offers while the battery allows.
	Slots int
	// SlotsFile is where the power reader writes and the runner reads
	// (default ~/.c4/agents/slots).
	SlotsFile string
	// Backoff is the first wait before a stopped runner is started again
	// (default 1s); it doubles to a minute.
	Backoff time.Duration
	// ServiceFile is the service's definition, removed when the
	// enrollment lapses (default this machine's); Log is the log, moved
	// aside past LogLimit at start (default ~/.c4/agents/agent.log, 10 MiB).
	ServiceFile, Log string
	LogLimit         int64
}

// enrollAgain is what the runner says when its machine's enrollment has
// lapsed: restarting it cannot help, and the person runs c4 farm up.
const enrollAgain = "enroll it again"

// AgentRun is `c4 farm agent run`: the power reader, writing the slots
// file, and c4-agent runner, reading it, kept running together. A runner
// that stops is started again, waiting longer each time; one whose
// enrollment has lapsed is not, and the run ends saying so.
func AgentRun(ctx context.Context, o Out, a AgentRunArgs) error {
	bin := a.AgentBin
	if bin == "" {
		p, err := exec.LookPath("c4-agent")
		if err != nil {
			return errors.New("c4-agent is not on PATH; brew install mrjoshuak/tap/c4 installs it beside c4 farm")
		}
		bin = p
	}
	if a.SlotsFile == "" {
		var err error
		if a.SlotsFile, err = DefaultSlotsFile(); err != nil {
			return err
		}
	}
	if a.Slots <= 0 {
		a.Slots = 4
	}
	backoff := a.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	if a.ServiceFile == "" || a.Log == "" {
		svc := &AgentService{}
		if svc.fillPaths() == nil {
			a.ServiceFile = cmp.Or(a.ServiceFile, svc.path())
			a.Log = cmp.Or(a.Log, svc.log())
		}
	}
	rotate(a.Log, cmp.Or(a.LogLimit, 10<<20))
	// the power reader and the runner's output write at once
	o.W = &syncWriter{w: o.W}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// the power reader stops with the run: cancel it, then wait for it
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := AgentPower(ctx, o, PowerArgs{SlotsFile: a.SlotsFile, Policy: power.DefaultPolicy(a.Slots)}); err != nil {
			o.printf("power     %v\n", err)
		}
	}()
	wait := backoff
	for {
		started := time.Now()
		lapsed, err := runRunner(ctx, o, bin, a.SlotsFile, a.Slots)
		if ctx.Err() != nil {
			return nil
		}
		if lapsed {
			o.printf("runner    this machine's enrollment has lapsed: run c4 farm up to enroll it again\n")
			// the service is not started again at the next sign-in: its
			// definition goes, and up makes it again after enrolling
			if a.ServiceFile != "" {
				os.Remove(a.ServiceFile)
			}
			return nil
		}
		if time.Since(started) > time.Minute {
			wait = backoff // it ran a while: start again soon
		}
		o.printf("runner    stopped (%v); starting it again in %s\n", err, Span(wait))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// rotate moves a log past its limit aside (log.1, one kept), so the next
// writes start a new file.
func rotate(log string, limit int64) {
	if fi, err := os.Stat(log); err == nil && fi.Size() > limit {
		os.Rename(log, log+".1")
	}
}

// syncWriter is a writer two goroutines may share.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// runRunner runs c4-agent runner once, its output passed through, and
// reports whether it ended because the machine must be enrolled again.
func runRunner(ctx context.Context, o Out, bin, slotsFile string, slots int) (bool, error) {
	// an enrolled runner takes its node, owner and kubeconfig from its
	// enrollment, and its slots from the file the power reader writes
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(ctx, bin, "runner", "--slots-file", slotsFile)
	// asked to stop, the runner finishes its turn: SIGTERM, then a grace
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	lapsed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			o.printf("runner    %s\n", line)
			if strings.Contains(line, enrollAgain) && !lapsed {
				// a runner whose enrollment lapsed only says so, again and
				// again: stop it now rather than wait for an exit
				lapsed = true
				stop()
			}
		}
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	return lapsed, err
}

// AgentService installs, removes and reports the machine's agent service.
type AgentService struct {
	// FarmBin is c4-farm, by absolute path, which the service runs (default
	// this executable).
	FarmBin string
	// AgentBin is c4-agent, by absolute path (default c4-agent on PATH),
	// given to the service, whose PATH is the system's.
	AgentBin string
	Slots    int
	// Home is the home folder (default $HOME); OS is runtime.GOOS.
	Home, OS string
	// Ctl runs launchctl or systemctl (default exec).
	Ctl func(ctx context.Context, name string, args ...string) error
}

// fillPaths settles the OS and home folder the service's files are under.
func (s *AgentService) fillPaths() error {
	if s.OS == "" {
		s.OS = runtime.GOOS
	}
	if s.OS != "darwin" && s.OS != "linux" {
		return fmt.Errorf("the agent service runs on macOS and Linux; on %s, run c4 farm agent run yourself", s.OS)
	}
	if s.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		s.Home = h
	}
	return nil
}

func (s *AgentService) fill() error {
	if err := s.fillPaths(); err != nil {
		return err
	}
	if s.FarmBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		s.FarmBin = exe
	}
	if s.AgentBin == "" {
		p, err := exec.LookPath("c4-agent")
		if err != nil {
			return errors.New("c4-agent is not on PATH; brew install mrjoshuak/tap/c4 installs it beside c4 farm")
		}
		if s.AgentBin, err = filepath.Abs(p); err != nil {
			return err
		}
	}
	if s.Slots <= 0 {
		s.Slots = 4
	}
	if s.Ctl == nil {
		s.Ctl = func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return nil
}

// path is where the service's definition lives.
func (s *AgentService) path() string {
	if s.OS == "darwin" {
		return filepath.Join(s.Home, "Library", "LaunchAgents", AgentLabel+".plist")
	}
	return filepath.Join(s.Home, ".config", "systemd", "user", "c4-agent.service")
}

func (s *AgentService) log() string { return filepath.Join(s.Home, ".c4", "agents", "agent.log") }

// definition is the service's file: it runs c4 farm agent run, keeps it
// running unless it ends cleanly (an enrollment that has lapsed), and
// logs to ~/.c4/agents/agent.log.
func (s *AgentService) definition() string {
	args := []string{s.FarmBin, "agent", "run", "--agent-bin", s.AgentBin, "--slots", strconv.Itoa(s.Slots)}
	if s.OS == "darwin" {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + AgentLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
		for _, a := range args {
			b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
		}
		b.WriteString(`  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>` + xmlEscape(s.envPath()) + `</string></dict>
  <key>ProcessType</key><string>Background</string>
  <key>Nice</key><integer>10</integer>
  <key>StandardOutPath</key><string>` + xmlEscape(s.log()) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(s.log()) + `</string>
</dict>
</plist>
`)
		return b.String()
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return `[Unit]
Description=C4 farm agent: this machine's power reader and host runner

[Service]
Environment=` + strconv.Quote("PATH="+s.envPath()) + `
ExecStart=` + strings.Join(quoted, " ") + `
Restart=on-failure
RestartSec=5
Nice=10
StandardOutput=append:` + s.log() + `
StandardError=append:` + s.log() + `

[Install]
WantedBy=default.target
`
}

// envPath is the service's PATH: launchd and systemd give a service the
// system's alone, so claude and c4-agent are found where they are here,
// then where Homebrew and the system put things.
func (s *AgentService) envPath() string {
	var dirs []string
	add := func(d string) {
		if d != "" && d != "." && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	add(filepath.Dir(s.AgentBin))
	add(filepath.Dir(s.FarmBin))
	if p, err := exec.LookPath("claude"); err == nil {
		add(filepath.Dir(p))
	}
	for _, d := range []string{filepath.Join(s.Home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		add(d)
	}
	return strings.Join(dirs, ":")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Start installs the service and starts it, replacing one installed
// before: c4 farm agent start.
func (s *AgentService) Start(ctx context.Context, o Out) error {
	real := s.Ctl == nil
	if err := s.fill(); err != nil {
		return err
	}
	// launchd and systemd act on the account's own session: a service
	// installed from another HOME (a test's, a script's temp folder) would
	// run there, in the real session, so it is refused unless named
	if u, err := user.Current(); real && err == nil && u.HomeDir != "" && filepath.Clean(s.Home) != filepath.Clean(u.HomeDir) && os.Getenv("C4_AGENT_SERVICE_HOME") != s.Home {
		return fmt.Errorf("HOME is %s, not this account's home (%s): the service would run your session's agents from there; set C4_AGENT_SERVICE_HOME=%s to mean it", s.Home, u.HomeDir, s.Home)
	}
	if err := os.MkdirAll(filepath.Dir(s.log()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path()), 0o755); err != nil {
		return err
	}
	// a service running as defined is left alone: starting it again would
	// stop the runner under live work
	target := "gui/" + strconv.Itoa(os.Getuid())
	if b, err := os.ReadFile(s.path()); err == nil && string(b) == s.definition() {
		running := s.Ctl(ctx, "systemctl", "--user", "is-active", "--quiet", "c4-agent.service") == nil
		if s.OS == "darwin" {
			running = s.Ctl(ctx, "launchctl", "print", target+"/"+AgentLabel) == nil
		}
		if running {
			o.printf("agent     running already, as defined (%s)\n", s.path())
			return nil
		}
	}
	if s.OS == "darwin" {
		s.Ctl(ctx, "launchctl", "bootout", target+"/"+AgentLabel) // not loaded is fine
		if err := os.WriteFile(s.path(), []byte(s.definition()), 0o644); err != nil {
			return err
		}
		if err := s.Ctl(ctx, "launchctl", "bootstrap", target, s.path()); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(s.path(), []byte(s.definition()), 0o644); err != nil {
			return err
		}
		if err := s.Ctl(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := s.Ctl(ctx, "systemctl", "--user", "enable", "--now", "c4-agent.service"); err != nil {
			return err
		}
	}
	o.printf("agent     started: the power reader and the host runner, kept running (%s)\n", s.path())
	o.printf("log       %s\n", s.log())
	return nil
}

// Stop stops the service and removes it: c4 farm agent stop.
func (s *AgentService) Stop(ctx context.Context, o Out) error {
	if err := s.fill(); err != nil {
		return err
	}
	if _, err := os.Stat(s.path()); os.IsNotExist(err) {
		o.printf("agent     not installed\n")
		return nil
	}
	if s.OS == "darwin" {
		s.Ctl(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+AgentLabel)
	} else {
		s.Ctl(ctx, "systemctl", "--user", "disable", "--now", "c4-agent.service")
	}
	if err := os.Remove(s.path()); err != nil {
		return err
	}
	o.printf("agent     stopped and removed\n")
	return nil
}
