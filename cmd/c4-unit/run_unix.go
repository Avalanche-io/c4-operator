//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// ownGroup starts the tool in a process group of its own, so a stop
// reaches everything it started.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends s to the tool's process group.
func signalGroup(p *os.Process, s os.Signal) {
	if sig, ok := s.(syscall.Signal); ok {
		_ = syscall.Kill(-p.Pid, sig)
	}
}

// exitCode is the tool's exit status: 128 and the signal for one a signal
// ended, as a shell reports it.
func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if c := ps.ExitCode(); c >= 0 {
		return c
	}
	if err != nil {
		return 1
	}
	return 0
}
