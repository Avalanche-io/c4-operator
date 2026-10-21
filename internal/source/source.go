// Package source is a public source of trees pinned at a commit, as an
// import reads it (c4 farm import hf:...): a revision resolved once to a
// commit, the files at that commit with the source's own hashes, and each
// file's bytes stored in C4 storage only once they match those hashes.
// The hubs (internal/source/huggingface) say how to reach each one; this
// package says what they hand back and how bytes are checked on the way in.
package source

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4"
)

// Snapshot is a repository at one commit.
type Snapshot struct {
	// Source names the hub ("huggingface"), Endpoint its address.
	Source, Endpoint string
	// RepoType is model or dataset; Repo is owner/name.
	RepoType, Repo string
	// Ref is what was asked (main, a tag, a commit); Commit what it meant.
	Ref, Commit string
	// Gated is how the hub gates the repository: "" for not at all,
	// "auto" or "manual" when a person with access must lend a token.
	Gated   string
	Private bool
	// License is the repository's own metadata: information, as a Docker
	// image's labels carry it, never a gate.
	License string
	// Files are the files at Commit the import takes, by path.
	Files []File
}

// File is one file at the commit, with the hashes the hub publishes.
type File struct {
	Path string
	Size int64
	// SHA256 is a large (LFS) file's hash; GitBlob the git blob SHA-1 every
	// file has. A file is checked by SHA256 when it has one, else GitBlob.
	SHA256, GitBlob string
	// XetHash is recorded, never checked: checking it needs Xet's chunker.
	XetHash string
}

// Fetched is a file in C4 storage under its ID.
type Fetched struct {
	File
	ID c4.ID
	// Stored is false when the store held it already.
	Stored bool
}

// Put stores size bytes from r under id, computed from those same bytes
// (gitimport.Sink's Put). It reports false when the store holds id.
type Put func(ctx context.Context, id c4.ID, size int64, r io.Reader) (stored bool, err error)

var (
	// ErrGated: the repository is gated and the token (or none) has no
	// access; a person with access lends one.
	ErrGated = errors.New("gated")
	// ErrNotFound: no such repository, revision or file, or a private one
	// the token cannot see.
	ErrNotFound = errors.New("not found")
	// ErrMismatch: the bytes are not what the hub's hash says.
	ErrMismatch = errors.New("does not match its source's hash")
	// ErrMoved: the hub answered for another commit than the one pinned.
	ErrMoved = errors.New("answered for another commit")
)

// Filter picks the files an import takes. A pattern without a slash
// matches a file's base name (*.safetensors), one with a slash its whole
// path (onnx/*). Empty Include takes every file.
type Filter struct {
	Include, Exclude []string
}

// Takes says whether the filter takes p.
func (f Filter) Takes(p string) bool {
	if len(f.Include) > 0 && !matchAny(f.Include, p) {
		return false
	}
	return !matchAny(f.Exclude, p)
}

func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		subject := p
		if !strings.Contains(pat, "/") {
			subject = path.Base(p)
		}
		if ok, _ := path.Match(pat, subject); ok {
			return true
		}
	}
	return false
}

// Check reports whether every pattern is well formed.
func (f Filter) Check() error {
	for _, pat := range append(append([]string{}, f.Include...), f.Exclude...) {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
	}
	return nil
}

// Store reads f's bytes from body once, computing its C4 ID and the hub's
// hash in the same pass into a spool file in dir, and puts it in C4
// storage only if the size and the hash match. Bytes that do not match
// never reach the store.
func Store(ctx context.Context, f File, body io.Reader, dir string, put Put) (Fetched, error) {
	if f.SHA256 == "" && f.GitBlob == "" {
		return Fetched{}, fmt.Errorf("%s: the source gave no hash to check it by", f.Path)
	}
	spool, err := os.CreateTemp(dir, "c4-source-*")
	if err != nil {
		return Fetched{}, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	id512 := sha512.New()
	var check hash.Hash
	want := f.SHA256
	if want != "" {
		check = sha256.New()
	} else {
		// git hashes a blob with its header: "blob <size>\0".
		check, want = sha1.New(), f.GitBlob
		check.Write([]byte("blob " + strconv.FormatInt(f.Size, 10) + "\x00"))
	}
	// One byte past the size the source gave is enough to know it lied.
	n, err := io.Copy(io.MultiWriter(spool, id512, check), io.LimitReader(body, f.Size+1))
	if err != nil {
		return Fetched{}, fmt.Errorf("%s: reading: %w", f.Path, err)
	}
	if n != f.Size {
		return Fetched{}, fmt.Errorf("%s: %d bytes, the source says %d: %w", f.Path, n, f.Size, ErrMismatch)
	}
	if got := hex.EncodeToString(check.Sum(nil)); !strings.EqualFold(got, want) {
		return Fetched{}, fmt.Errorf("%s: hash %s, the source says %s: %w", f.Path, got, want, ErrMismatch)
	}
	var id c4.ID
	copy(id[:], id512.Sum(nil))
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return Fetched{}, err
	}
	stored, err := put(ctx, id, n, spool)
	if err != nil {
		return Fetched{}, fmt.Errorf("%s: storing: %w", f.Path, err)
	}
	return Fetched{File: f, ID: id, Stored: stored}, nil
}
