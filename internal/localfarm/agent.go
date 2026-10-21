package localfarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/cluster"
)

// Enrolled is what c4-agent enroll --json reports: this machine as one of
// the farm's hosts, with its pool, owned by the farm's owner.
type Enrolled struct {
	Node       string   `json:"node"`
	Owner      string   `json:"owner"`
	Pool       string   `json:"pool"`
	Kubeconfig string   `json:"kubeconfig"`
	Made       []string `json:"made"`
	// BlocksEnv is the clean config directory the machine's blocks run
	// with, and BlocksSignedIn whether the person has signed in there.
	BlocksEnv      string `json:"blocksEnv,omitempty"`
	BlocksSignedIn bool   `json:"blocksSignedIn"`
	// BlocksFolder is the one folder the blocks run in, and BlocksTrusted
	// whether Claude Code trusts it in that config directory.
	BlocksFolder  string `json:"blocksFolder,omitempty"`
	BlocksTrusted bool   `json:"blocksTrusted"`
}

// EnrollMachine makes this machine a host runner of the local farm for
// its owner: c4-agent enroll, given the farm admin's kubeconfig for that
// one run and never kept, makes the machine's own account and its pool,
// and leaves the runner a kubeconfig of its own whose tokens it renews.
// Running it again makes nothing new.
func EnrollMachine(ctx context.Context, agentBin, owner string, slots int) (Enrolled, error) {
	if owner == "" {
		return Enrolled{}, fmt.Errorf("enroll: the farm has no owner")
	}
	kc, err := cluster.NewProvider().KubeConfig(Name, false)
	if err != nil {
		return Enrolled{}, err
	}
	dir, err := os.MkdirTemp("", "c4-enroll-")
	if err != nil {
		return Enrolled{}, err
	}
	defer os.RemoveAll(dir)
	admin := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(admin, []byte(kc), 0o600); err != nil {
		return Enrolled{}, err
	}
	args := []string{"enroll", "--owner", owner, "--kubeconfig", admin, "--context", "kind-" + Name, "--json"}
	if slots > 0 {
		args = append(args, "--slots", strconv.Itoa(slots))
	}
	out, err := exec.CommandContext(ctx, agentBin, args...).Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(ee.Stderr)), "c4-agent: "))
		}
		return Enrolled{}, fmt.Errorf("enroll: %s", msg)
	}
	var e Enrolled
	if err := json.Unmarshal(out, &e); err != nil {
		return Enrolled{}, fmt.Errorf("enroll: %w", err)
	}
	return e, nil
}

// RunnerServes says this machine's runner, enrolled under home, serves the
// local farm name: its kubeconfig names that farm's API server. A runner
// enrolled to another farm, or none at all, is not this farm's to stop.
func RunnerServes(home, name string) bool {
	b, err := os.ReadFile(filepath.Join(home, ".c4", "agents", "runner", "kubeconfig"))
	if err != nil {
		return false
	}
	kc, err := cluster.NewProvider().KubeConfig(name, false)
	if err != nil {
		return false
	}
	return sameServer(b, []byte(kc))
}

// sameServer says two kubeconfigs' current clusters are one API server.
func sameServer(a, b []byte) bool {
	server := func(kc []byte) string {
		cfg, err := clientcmd.Load(kc)
		if err != nil {
			return ""
		}
		if ctx := cfg.Contexts[cfg.CurrentContext]; ctx != nil {
			if c := cfg.Clusters[ctx.Cluster]; c != nil {
				return c.Server
			}
		}
		for _, c := range cfg.Clusters {
			return c.Server
		}
		return ""
	}
	sa, sb := server(a), server(b)
	return sa != "" && sa == sb
}
