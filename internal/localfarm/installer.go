package localfarm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The installer token is what makes a request the installer's on a local
// farm: development viewers (?as=) are taken only from a request that
// carries it, so job code in the farm's pods, which can reach the HUD but
// never the person's ~/.c4, cannot act as the person or a role. up makes
// it once per farm, keeps it in the person's ~/.c4 and gives the manager a
// copy as a Secret only the manager's pod mounts.
const (
	// InstallerSecret is the Secret in c4-system holding the token, key
	// "token".
	InstallerSecret = "c4-installer-token"
	// installerMount is where the manager's pod mounts it.
	installerMount = "/etc/c4/installer"
	// InstallerHeader carries the token on the CLI's requests.
	InstallerHeader = "X-C4-Installer"
)

// InstallerTokenFile is where the person's copy of farm name's token is
// kept: ~/.c4/<farm>/installer-token, as farm.yaml names it.
func InstallerTokenFile(name string) string {
	return "~/.c4/" + name + "/installer-token"
}

func installerTokenPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".c4", name, "installer-token"), nil
}

// InstallerToken is farm name's token, made the first time: 32 random
// bytes as hex, mode 0600 in a 0700 folder. Kept across ups, so a browser
// signed in to the farm stays signed in.
func InstallerToken(name string) (string, error) {
	p, err := installerTokenPath(name)
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) == 64 {
			return t, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the farm's installer token: %w", err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	// a folder made here is the person's alone; one that is already there
	// keeps its mode, and the file's own 0600 guards the token
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(p)), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Dir(p), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("the farm's installer token: %w", err)
	}
	return t, nil
}

// RemoveInstallerToken takes farm name's token away with the farm.
func RemoveInstallerToken(name string) error {
	p, err := installerTokenPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	os.Remove(filepath.Dir(p)) // only when nothing else is kept there
	return nil
}
