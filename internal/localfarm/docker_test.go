package localfarm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// With no docker command at all, up says to install one, naming the
// engines plainly; an installed one that does not answer is "not running".
func TestCheckDockerNotInstalled(t *testing.T) {
	defer func(f func(string) (string, error)) { lookDocker = f }(lookDocker)
	lookDocker = func(string) (string, error) { return "", errors.New("not found") }
	err := CheckDocker(context.Background())
	if err == nil || err.Error() != "c4 farm up needs Docker, and none is installed: install Docker Desktop (docker.com), OrbStack or Colima, then run c4 farm up again" {
		t.Errorf("not installed: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "not running") {
		t.Error("not installed is said as not running")
	}
}
