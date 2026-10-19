package agent

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// A stopped session's process takes a moment to exit: Stop waits for it,
// and gives up, with an error, on one that outlives the limit.
func TestWaitExit(t *testing.T) {
	ctx := context.Background()
	cmd := exec.Command("sleep", "0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap it, as the CLI's daemon is reaped by its own parent
	start := time.Now()
	if err := waitExit(ctx, cmd.Process.Pid, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("returned after %s, before the process exited", d)
	}
	long := exec.Command("sleep", "5")
	if err := long.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { long.Process.Kill(); long.Wait() }()
	if err := waitExit(ctx, long.Process.Pid, 200*time.Millisecond); err == nil {
		t.Fatal("waited past its limit without an error")
	}
	if err := waitExit(ctx, 0, time.Second); err != nil {
		t.Fatal(err)
	}
}
