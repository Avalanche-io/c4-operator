package farmcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ReleasesAPI answers with the latest published release of c4-operator.
var ReleasesAPI = "https://api.github.com/repos/Avalanche-io/c4-operator/releases/latest"

// ErrNoRelease is a development build asking for release assets before any
// release is published.
var ErrNoRelease = errors.New("no c4-operator release is published yet")

// ReleaseFor is the release whose assets (samples, install.yaml) a build
// uses: its own version, or for a development build ("dev", a commit) the
// latest published release, with dev set so the caller can say so.
func ReleaseFor(ctx context.Context, hc *http.Client, version string) (tag string, dev bool, err error) {
	if strings.HasPrefix(version, "v") {
		return version, false, nil
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesAPI, nil)
	if err != nil {
		return "", true, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("asking for the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", true, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return "", true, fmt.Errorf("asking for the latest release: %s", resp.Status)
	}
	var r struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || !strings.HasPrefix(r.Tag, "v") {
		return "", true, fmt.Errorf("the latest release names no version tag (%q)", r.Tag)
	}
	return r.Tag, true, nil
}
