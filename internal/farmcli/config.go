// Package farmcli is `c4 farm`: the user command for a C4 farm. Every
// verb that changes the farm is one call to the HUD API as the signed-in
// user, so it is checked, signed and written to the book exactly as the
// same action taken in the HUD.
package farmcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Farm is one farm the user can talk to.
type Farm struct {
	HUD       string `json:"hud"`
	As        string `json:"as,omitempty"`        // a development viewer (local farms)
	TokenFile string `json:"tokenFile,omitempty"` // a bearer token (studio farms)
	Project   string `json:"project,omitempty"`   // the default project
	Local     bool   `json:"local,omitempty"`
	// Owner is who owns a local farm and plays every role on it; up keeps
	// it unless --owner changes it.
	Owner string `json:"owner,omitempty"`
	// InstallerTokenFile, on a local farm, is the installer token up made
	// (~/.c4/<farm>/installer-token): the HUD takes a development viewer
	// only from a request that carries it.
	InstallerTokenFile string `json:"installerTokenFile,omitempty"`
}

// Config is ~/.c4/farm.yaml.
type Config struct {
	Current string          `json:"current"`
	Farms   map[string]Farm `json:"farms"`
	// Integrations are the person's answers, by integration (claude, and
	// later each DCC): on or off; one not in the map is unanswered, and is
	// asked about once (c4 farm integrate NAME on|off changes it).
	Integrations map[string]bool `json:"integrations,omitempty"`
	path         string
}

// ConfigPath is where the config lives: $C4_FARM_CONFIG, else
// ~/.c4/farm.yaml.
func ConfigPath() string {
	if p := os.Getenv("C4_FARM_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".c4/farm.yaml"
	}
	return filepath.Join(home, ".c4", "farm.yaml")
}

// LoadConfig reads the config; a missing file is an empty config.
func LoadConfig(path string) (*Config, error) {
	c := &Config{Farms: map[string]Farm{}, path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Farms == nil {
		c.Farms = map[string]Farm{}
	}
	c.path = path
	return c, nil
}

// Save writes the config, readable only by the user.
func (c *Config) Save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}

// Names lists the farms, sorted.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Farms))
	for n := range c.Farms {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve picks the farm a command talks to: name (--farm), else
// $C4_FARM, else the current farm, else the only one.
func (c *Config) Resolve(name string) (string, Farm, error) {
	if name == "" {
		name = os.Getenv("C4_FARM")
	}
	if name == "" {
		name = c.Current
	}
	if name == "" && len(c.Farms) == 1 {
		name = c.Names()[0]
	}
	if name == "" {
		return "", Farm{}, errors.New("no farm yet: run `c4 farm up` for one on this machine, or `c4 farm login HUD-URL` for your studio's")
	}
	f, ok := c.Farms[name]
	if !ok {
		return "", Farm{}, fmt.Errorf("no farm %q; you have: %s", name, strings.Join(c.Names(), ", "))
	}
	return name, f, nil
}

// Token reads the farm's bearer token, if it has one.
// InstallerToken is the local farm's installer token, if it has one.
func (f Farm) InstallerToken() (string, error) {
	if f.InstallerTokenFile == "" {
		return "", nil
	}
	return Farm{TokenFile: f.InstallerTokenFile}.Token()
}

func (f Farm) Token() (string, error) {
	if f.TokenFile == "" {
		return "", nil
	}
	p := f.TokenFile
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("token: %w (sign in again with `c4 farm login`)", err)
	}
	return strings.TrimSpace(string(data)), nil
}
