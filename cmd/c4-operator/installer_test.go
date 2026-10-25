package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Development viewers fail closed: --hud-dev-viewers needs the installer
// token's file, or --hud-dev-viewers-open to say anyone is any viewer.
// The file's token is hex, read without its trailing newline.
func TestInstallerTokenFailsClosed(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	os.WriteFile(good, []byte("6f3a1c9e2b7d4f80a1c3e5b7d9f1a3c5\n"), 0o600)
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("6f3a\n"), 0o600)
	notHex := filepath.Join(dir, "nothex")
	os.WriteFile(notHex, []byte(strings.Repeat("z", 32)), 0o600)

	if _, err := installerToken("wrangler=*", "", false); err == nil || !strings.Contains(err.Error(), "--hud-dev-viewers needs --hud-installer-token-file") {
		t.Errorf("dev viewers with neither: %v", err)
	}
	if tok, err := installerToken("wrangler=*", "", true); err != nil || tok != nil {
		t.Errorf("open dev viewers: %q %v", tok, err)
	}
	if tok, err := installerToken("wrangler=*", good, false); err != nil || string(tok) != "6f3a1c9e2b7d4f80a1c3e5b7d9f1a3c5" {
		t.Errorf("the token: %q %v", tok, err)
	}
	for _, f := range []string{short, notHex, filepath.Join(dir, "missing")} {
		if _, err := installerToken("wrangler=*", f, false); err == nil {
			t.Errorf("%s: no error", filepath.Base(f))
		}
	}
	if _, err := installerToken("", good, false); err == nil {
		t.Error("a token without dev viewers")
	}
	if tok, err := installerToken("", "", false); err != nil || tok != nil {
		t.Errorf("no dev viewers: %q %v", tok, err)
	}
}
