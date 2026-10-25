package localfarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
// Running it again makes nothing new. hud, when the c4-agent takes it, is
// the farm's HUD, where the runner offers the machine to its holder.
func EnrollMachine(ctx context.Context, agentBin, owner, hud string, slots int) (Enrolled, error) {
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
	if hud != "" {
		args = append(args, "--hud", hud)
	}
	out, err := exec.CommandContext(ctx, agentBin, args...).Output()
	if err != nil {
		return Enrolled{}, fmt.Errorf("enroll: %s", agentError(err))
	}
	var e Enrolled
	if err := json.Unmarshal(out, &e); err != nil {
		return Enrolled{}, fmt.Errorf("enroll: %w", err)
	}
	return e, nil
}

// agentError is what c4-agent said on failing, else how it failed.
func agentError(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(ee.Stderr)), "c4-agent: "))
	}
	return err.Error()
}

// Offer is an enrolled machine's offer to its holder, as c4-agent
// enroll-offer --json reports it: the code the farm gave its runner.
type Offer struct {
	Node    string    `json:"node"`
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
	Message string    `json:"message"`
}

// OfferWait is how long OfferMachine waits for the runner just started
// to have its HUD token, which the offer is made with.
var OfferWait = 30 * time.Second

// OfferMachine has this enrolled machine offer itself to its holder, once,
// for up to accept on the person's behalf: the runner's first start makes
// the token the offer needs, so a machine whose runner has not yet made it
// is asked again for a little while.
func OfferMachine(ctx context.Context, agentBin string) (Offer, error) {
	deadline := time.Now().Add(OfferWait)
	for {
		out, err := exec.CommandContext(ctx, agentBin, "enroll-offer", "--json").Output()
		if err == nil {
			var o Offer
			if err := json.Unmarshal(out, &o); err != nil {
				return Offer{}, fmt.Errorf("enroll-offer: %w", err)
			}
			return o, nil
		}
		msg := agentError(err)
		if !strings.Contains(msg, "no HUD token yet") || time.Now().After(deadline) {
			return Offer{}, errors.New(strings.TrimPrefix(msg, "enroll-offer: "))
		}
		select {
		case <-ctx.Done():
			return Offer{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
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
