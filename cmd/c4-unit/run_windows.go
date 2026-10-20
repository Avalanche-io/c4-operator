package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// ownGroup starts the tool in a console process group of its own, so a
// stop (CTRL_BREAK) reaches it and not the runner.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// signalGroup asks the tool's group to stop (CTRL_BREAK) for SIGTERM and
// SIGINT, and ends the tool and everything it started for anything else.
func signalGroup(p *os.Process, s os.Signal) {
	if s == syscall.SIGTERM || s == os.Interrupt {
		if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid)) == nil {
			return
		}
	}
	// taskkill ends the whole tree; Kill, the tool alone, if it is missing
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run() != nil {
		_ = p.Kill()
	}
}

// exitCode is the tool's exit status.
func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 1
	}
	if c := ps.ExitCode(); c >= 0 {
		return c
	}
	if err != nil {
		return 1
	}
	return 0
}
