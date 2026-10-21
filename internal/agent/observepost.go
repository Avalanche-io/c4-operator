package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ObservedReport is what observing a machine reports: the machine as its
// person knows it, when, and its sessions.
type ObservedReport struct {
	Machine    string    `json:"machine"`
	ObservedAt time.Time `json:"observedAt"`
	// Owners are the runner's owners (--owner), when a runner reports: the
	// farm shows the rows to those of them who own a pool its node is in,
	// and to no one else.
	Owners   []string   `json:"owners,omitempty"`
	Sessions []Observed `json:"sessions"`
}

// Machine is the name a person knows this machine by: its computer name on
// macOS ("Sam's MacBook Pro"), else its host name.
func Machine() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				return name
			}
		}
	}
	name, _ := os.Hostname()
	return name
}

// defaultObserveEvery is how often a runner reports the machine's sessions.
const defaultObserveEvery = time.Minute

// postObserved reports the machine's sessions to the farm, as this runner's
// node: POST /api/v1/nodes/<node>/observed on the HUD, with the runner's own
// token. The farm shows them to the owners of the pools this node is in,
// and to no one else.
func (r *Runner) postObserved(ctx context.Context) error {
	obs, err := r.Harness.Observe(ctx, r.ObserveConfigDir, false, r.now())
	if err != nil {
		return err
	}
	// paths on the machine stay on it: the folder's name says which
	// project a session is in
	for i := range obs {
		obs[i].Transcript, obs[i].Cwd = "", filepath.Base(obs[i].Cwd)
	}
	body, _ := json.Marshal(ObservedReport{Machine: Machine(), ObservedAt: r.now().UTC(), Owners: r.Owners, Sessions: obs})
	u := strings.TrimRight(r.HUD, "/") + "/api/v1/nodes/" + url.PathEscape(r.Node) + "/observed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.HUDToken != nil {
		if t := r.HUDToken(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the farm refused this machine's sessions: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var ans struct {
		ShownTo []string `json:"shownTo"`
		Why     string   `json:"why"`
	}
	if json.Unmarshal(b, &ans) == nil && ans.ShownTo != nil && len(ans.ShownTo) == 0 && !r.observedUnseen {
		r.observedUnseen = true
		why := ans.Why
		if why == "" {
			why = "its node is in no pool its owners own"
		}
		r.logf("this machine's sessions are shown to no one: %s", why)
	}
	return nil
}

// observeTick reports the machine's sessions when they are due.
func (r *Runner) observeTick(ctx context.Context) {
	if !r.Observe || r.HUD == "" {
		return
	}
	every := r.ObserveEvery
	if every <= 0 {
		every = defaultObserveEvery
	}
	if !r.observedAt.IsZero() && r.now().Sub(r.observedAt) < every {
		return
	}
	r.observedAt = r.now()
	err := r.postObserved(ctx)
	switch {
	case err != nil && (r.observedErr == "" || r.observedErr != err.Error()):
		r.observedErr = err.Error()
		r.logf("reporting this machine's sessions: %v", err)
	case err == nil && r.observedErr != "":
		r.observedErr = ""
		r.logf("reporting this machine's sessions again")
	}
}
