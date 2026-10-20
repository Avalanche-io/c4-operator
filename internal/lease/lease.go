// Package lease is the holder's side of a holder-transfer CredentialLease
// (proposals agent-jobs 09): the holder's agent, on the holder's own
// machine, takes the holder's current Claude Code sign-in and keeps the
// lease's Secret holding its short-lived access token, renewed before it
// expires, until the lease ends or the agent stops, when it deletes the
// Secret. The refresh token never leaves the machine: the holder's own
// Claude Code keeps refreshing the sign-in, and the agent copies each new
// access token.
//
// The credential is read and written inside this package only. Nothing
// here prints, logs or returns it, and errors never quote it.
package lease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Source reads the holder's current sign-in: Claude Code's credentials
// document ({"claudeAiOauth": {...}}).
type Source interface {
	Read(ctx context.Context) ([]byte, error)
	// Where says where it reads from, without the credential.
	Where() string
}

// ClaudeSource is where Claude Code keeps the sign-in of config dir dir:
// on macOS the login keychain item "Claude Code-credentials", suffixed for
// a config dir other than ~/.claude with the first 8 hex digits of the
// SHA-256 of its path; elsewhere <dir>/.credentials.json.
func ClaudeSource(dir string) (Source, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	dir = filepath.Clean(dir)
	if runtime.GOOS != "darwin" {
		return FileSource(filepath.Join(dir, ".credentials.json")), nil
	}
	service := "Claude Code-credentials"
	if dir != filepath.Join(home, ".claude") {
		sum := sha256.Sum256([]byte(dir))
		service += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return KeychainSource{Service: service, Account: os.Getenv("USER")}, nil
}

// FileSource reads a credentials file.
type FileSource string

func (f FileSource) Read(ctx context.Context) ([]byte, error) {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return nil, fmt.Errorf("the sign-in at %s: %w", string(f), err)
	}
	return b, nil
}

func (f FileSource) Where() string { return string(f) }

// KeychainSource reads a macOS keychain item's secret with security(1).
type KeychainSource struct{ Service, Account string }

func (k KeychainSource) Read(ctx context.Context) ([]byte, error) {
	args := []string{"find-generic-password", "-s", k.Service, "-w"}
	if k.Account != "" {
		args = append(args, "-a", k.Account)
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// security's stderr names the item, never its secret
		return nil, fmt.Errorf("the sign-in in keychain item %q: %v: %s", k.Service, err, bytes.TrimSpace(errb.Bytes()))
	}
	return bytes.TrimSpace(out.Bytes()), nil
}

func (k KeychainSource) Where() string { return "keychain item " + k.Service }

// ErrNoSignIn is a credentials document without a sign-in.
var ErrNoSignIn = errors.New("no Claude sign-in in it")

// ShortLived is the part of Claude Code's credentials document a lease
// carries: the access token, its expiry and what it is for, without the
// refresh token. It returns the document to write and the token's expiry.
func ShortLived(doc []byte) ([]byte, time.Time, error) {
	var in struct {
		OAuth map[string]json.RawMessage `json:"claudeAiOauth"`
	}
	if json.Unmarshal(doc, &in) != nil || in.OAuth == nil {
		return nil, time.Time{}, ErrNoSignIn
	}
	var ms int64
	if json.Unmarshal(in.OAuth["expiresAt"], &ms) != nil || ms <= 0 || len(in.OAuth["accessToken"]) < 3 {
		return nil, time.Time{}, ErrNoSignIn
	}
	keep := map[string]json.RawMessage{}
	for _, k := range []string{"accessToken", "expiresAt", "scopes", "subscriptionType", "rateLimitTier"} {
		if v, ok := in.OAuth[k]; ok {
			keep[k] = v
		}
	}
	out, err := json.Marshal(map[string]any{"claudeAiOauth": keep})
	if err != nil {
		return nil, time.Time{}, errors.New("could not write the lease's credential")
	}
	return out, time.UnixMilli(ms).UTC(), nil
}
