// Command seed generates the synthetic show the e2e test materializes,
// and computes the tree IDs pods should see.
//
//	seed gen    --out DIR [--frame-mib N]
//	seed expect --src DIR --include a/,b/
//
// Content is deterministic pseudo-random bytes keyed by name, so files
// that share a key are byte-identical: that is how the show carries
// duplicates within and across shots.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/manifest"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seed gen|expect ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2:])
	case "expect":
		err = expect(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

type file struct {
	path string
	key  string
	mib  int
}

// show lays out the synthetic show. f is the frame size in MiB.
//
//	shots/sh010/plates  48 frames
//	shots/sh020/plates  48 frames; the first 24 are byte-identical to
//	                    sh010's last 24 (overlapping handles)
//	shots/sh020/ref     a slate identical to sh020 frame 1025
//	shots/sh030/plates  32 frames nobody asks for
//	editorial           one reference movie (16 frames' worth)
//	weights             one model file (32 frames' worth)
func show(f int) []file {
	var out []file
	for i := 1001; i <= 1048; i++ {
		out = append(out, file{fmt.Sprintf("shots/sh010/plates/sh010.%04d.exr", i), fmt.Sprintf("sh010-%d", i), f})
	}
	for i := 1001; i <= 1048; i++ {
		key := fmt.Sprintf("sh020-%d", i)
		if i <= 1024 {
			key = fmt.Sprintf("sh010-%d", i+24)
		}
		out = append(out, file{fmt.Sprintf("shots/sh020/plates/sh020.%04d.exr", i), key, f})
	}
	out = append(out, file{"shots/sh020/ref/slate.exr", "sh020-1025", f})
	for i := 1001; i <= 1032; i++ {
		out = append(out, file{fmt.Sprintf("shots/sh030/plates/sh030.%04d.exr", i), fmt.Sprintf("sh030-%d", i), f})
	}
	out = append(out, file{"editorial/sh010_ref.mov", "edit-sh010", 16 * f})
	out = append(out, file{"weights/denoise-v3.safetensors", "weights-v3", 32 * f})
	return out
}

func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", "", "output directory")
	frameMiB := fs.Int("frame-mib", 16, "frame size in MiB")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	var total int64
	unique := map[string]int64{}
	for _, f := range show(*frameMiB) {
		p := filepath.Join(*out, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		n := int64(f.mib) << 20
		if err := writeKeyed(p, f.key, n); err != nil {
			return err
		}
		total += n
		unique[f.key] = n
	}
	var u int64
	for _, n := range unique {
		u += n
	}
	fmt.Printf("files=%d bytes=%d unique=%d uniqueBytes=%d\n", len(show(*frameMiB)), total, len(unique), u)
	return nil
}

func writeKeyed(path, key string, n int64) error {
	sum := sha256.Sum256([]byte(key))
	var seed [32]byte
	copy(seed[:], sum[:])
	rng := rand.NewChaCha8(seed)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for written := int64(0); written < n; {
		for i := 0; i+8 <= len(buf); i += 8 {
			binary.LittleEndian.PutUint64(buf[i:], rng.Uint64())
		}
		k := min(int64(len(buf)), n-written)
		if _, err := f.Write(buf[:k]); err != nil {
			f.Close()
			return err
		}
		written += k
	}
	return f.Close()
}

// expect builds the subset an include list selects (hardlinks, so no
// bytes are copied) and scans it with the c4 scanner in content mode.
func expect(args []string) error {
	fs := flag.NewFlagSet("expect", flag.ExitOnError)
	src := fs.String("src", "", "show directory")
	include := fs.String("include", "", "comma-separated include patterns")
	fs.Parse(args)
	var pats []string
	if *include != "" {
		pats = strings.Split(*include, ",")
	}
	f, err := manifest.NewFilter(pats)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(filepath.Clean(*src)), ".expect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	err = filepath.Walk(*src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(*src, p)
		if !f.Includes(filepath.ToSlash(rel)) {
			return nil
		}
		dst := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Link(p, dst)
	})
	if err != nil {
		return err
	}
	id, err := manifest.ScanContentID(tmp)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}
