// Command c4e2e generates the e2e test trees and is the workload inside
// the e2e's pods.
//
//	c4e2e gen-show  --out DIR [--frame-kib N]        the P1 show
//	c4e2e gen-shot  --out DIR --name N --frames N --frame-kib N --seed S
//	c4e2e tree-id   DIR                               view ID (as c4-operator tree-id)
//	c4e2e verify    --expect ID DIR                   fail unless DIR is that view
//	c4e2e render    --pod NAME [--shot sh010] DIR     a deterministic render into DIR
//	c4e2e read      DIR                               read every file, print bytes and time
//
// Content is deterministic pseudo-random bytes keyed by name, so files that
// share a key are byte-identical, and the host can recompute what any pod
// wrote.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: c4e2e gen-show|gen-shot|tree-id|verify|render|read ...")
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"gen-show": genShow,
		"gen-shot": genShot,
		"tree-id":  treeID,
		"verify":   verify,
		"render":   render,
		"read":     readAll,
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "c4e2e:", err)
		os.Exit(1)
	}
}

type file struct {
	path string
	key  string
	kib  int
	mode os.FileMode
	link string
}

// show is the P1 show. f is the frame size in KiB.
//
//	shots/sh010/plates   48 frames
//	shots/sh020/plates   48 frames; the first 24 equal sh010's last 24
//	shots/sh020/ref      a slate equal to sh020 frame 1025
//	shots/sh030/plates   32 frames nobody asks for
//	editorial            a reference movie (16 frames' worth)
//	weights              a model file (32 frames' worth)
//	tools                an executable script and a symlink to it
func show(f int) []file {
	var out []file
	for i := 1001; i <= 1048; i++ {
		out = append(out, file{path: fmt.Sprintf("shots/sh010/plates/sh010.%04d.exr", i), key: fmt.Sprintf("sh010-%d", i), kib: f})
	}
	for i := 1001; i <= 1048; i++ {
		key := fmt.Sprintf("sh020-%d", i)
		if i <= 1024 {
			key = fmt.Sprintf("sh010-%d", i+24)
		}
		out = append(out, file{path: fmt.Sprintf("shots/sh020/plates/sh020.%04d.exr", i), key: key, kib: f})
	}
	out = append(out, file{path: "shots/sh020/ref/slate.exr", key: "sh020-1025", kib: f})
	for i := 1001; i <= 1032; i++ {
		out = append(out, file{path: fmt.Sprintf("shots/sh030/plates/sh030.%04d.exr", i), key: fmt.Sprintf("sh030-%d", i), kib: f})
	}
	out = append(out,
		file{path: "shots/sh010/notes.txt", key: "notes-sh010", kib: 1},
		file{path: "editorial/sh010_ref.mov", key: "edit-sh010", kib: 16 * f},
		file{path: "weights/denoise-v3.safetensors", key: "weights-v3", kib: 32 * f},
		file{path: "tools/render.sh", key: "render-sh", kib: 1, mode: 0o755},
		file{path: "tools/render-latest", link: "render.sh"},
	)
	return out
}

func genShow(args []string) error {
	fs := flag.NewFlagSet("gen-show", flag.ExitOnError)
	out := fs.String("out", "", "output directory")
	kib := fs.Int("frame-kib", 16384, "frame size in KiB")
	fs.Parse(args)
	if *out == "" {
		return errors.New("--out is required")
	}
	return write(*out, show(*kib))
}

func genShot(args []string) error {
	fs := flag.NewFlagSet("gen-shot", flag.ExitOnError)
	out := fs.String("out", "", "output directory")
	name := fs.String("name", "herd", "shot name")
	frames := fs.Int("frames", 16, "frames")
	kib := fs.Int("frame-kib", 16384, "frame size in KiB")
	seed := fs.String("seed", "1", "content seed (different seeds give different bytes)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("--out is required")
	}
	var files []file
	for i := 1; i <= *frames; i++ {
		files = append(files, file{path: fmt.Sprintf("shots/%s/plates/%s.%04d.exr", *name, *name, 1000+i), key: fmt.Sprintf("%s-%s-%d", *seed, *name, i), kib: *kib})
	}
	return write(*out, files)
}

func write(root string, files []file) error {
	var total int64
	unique := map[string]int64{}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if f.link != "" {
			os.Remove(p)
			if err := os.Symlink(f.link, p); err != nil {
				return err
			}
			continue
		}
		n := int64(f.kib) << 10
		if err := writeKeyed(p, f.key, n); err != nil {
			return err
		}
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		total += n
		unique[f.key] = n
	}
	var u int64
	for _, n := range unique {
		u += n
	}
	fmt.Printf("files=%d bytes=%d unique=%d uniqueBytes=%d\n", len(files), total, len(unique), u)
	return nil
}

// writeKeyed writes n deterministic pseudo-random bytes keyed by key.
func writeKeyed(path, key string, n int64) error {
	sum := sha256.Sum256([]byte(key))
	rng := rand.NewChaCha8(sum)
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

func treeID(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: c4e2e tree-id DIR")
	}
	id, err := view.ScanID(args[0])
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	expect := fs.String("expect", "", "the view ID the directory must be")
	fs.Parse(args)
	if fs.NArg() != 1 || *expect == "" {
		return errors.New("usage: c4e2e verify --expect ID DIR")
	}
	start := time.Now()
	id, err := view.ScanID(fs.Arg(0))
	if err != nil {
		return err
	}
	if id.String() != *expect {
		return fmt.Errorf("tree is %s, expected %s", id, *expect)
	}
	fmt.Printf("verified %s in %s\n", id, time.Since(start).Round(time.Millisecond))
	return nil
}

// render is the P1 workload: it writes three new frames for this pod,
// revises the shot notes, removes the last plate, and replaces the
// editorial directory, so a capture exercises additions, modifications,
// deletions and an opaque directory. Everything depends only on the pod
// name, so the host can recompute the expected output.
func render(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	pod := fs.String("pod", os.Getenv("POD_NAME"), "pod name (keys the output)")
	shot := fs.String("shot", "sh010", "shot")
	kib := fs.Int("frame-kib", 1024, "rendered frame size in KiB")
	fs.Parse(args)
	if fs.NArg() != 1 || *pod == "" {
		return errors.New("usage: c4e2e render --pod NAME DIR")
	}
	root := fs.Arg(0)
	dir := filepath.Join(root, "shots", *shot, "render", *pod)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	for i := 1001; i <= 1003; i++ {
		if err := writeKeyed(filepath.Join(dir, fmt.Sprintf("%s.%04d.exr", *shot, i)), *pod+fmt.Sprint(i), int64(*kib)<<10); err != nil {
			return err
		}
	}
	notes := filepath.Join(root, "shots", *shot, "notes.txt")
	f, err := os.OpenFile(notes, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "rendered by %s\n", *pod)
	f.Close()
	if err := os.Remove(filepath.Join(root, "shots", *shot, "plates", *shot+".1048.exr")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "editorial")); err == nil {
		os.RemoveAll(filepath.Join(root, "editorial"))
		os.MkdirAll(filepath.Join(root, "editorial"), 0o777)
		os.WriteFile(filepath.Join(root, "editorial", "cut-"+*pod+".txt"), []byte("new cut by "+*pod+"\n"), 0o644)
	}
	id, err := view.ScanID(root)
	if err != nil {
		return err
	}
	fmt.Printf("output %s\n", id)
	return nil
}

func readAll(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: c4e2e read DIR")
	}
	start := time.Now()
	var total int64
	err := filepath.WalkDir(args[0], func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := io.Copy(io.Discard, f)
		f.Close()
		total += n
		return err
	})
	if err != nil {
		return err
	}
	fmt.Printf("read %d bytes in %s\n", total, time.Since(start).Round(time.Millisecond))
	return nil
}
