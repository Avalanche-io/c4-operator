// Command c4e2e generates the e2e test trees and is the workload inside
// the e2e's pods.
//
//	c4e2e gen-show  --out DIR [--frame-kib N]        the P1 show
//	c4e2e gen-shot  --out DIR --name N --frames N --frame-kib N --seed S
//	c4e2e tree-id   DIR|FILE                          view ID (as c4-operator tree-id), or a file's ID
//	c4e2e verify    --expect ID DIR                   fail unless DIR is that view
//	c4e2e render    --pod NAME [--shot sh010] DIR     a deterministic render into DIR
//	c4e2e read      DIR                               read every file, print bytes and time
//	c4e2e expect    --src DIR --include a/,b/ [--c4m FILE] [--render POD] [--add-frames N --frames-dir D --pod P]
//	                                                  view ID of a subset, optionally after a render
//	c4e2e add-frames --count N --dir REL --pod NAME DIR  add N new frames (keyed by pod) under DIR/REL
//	c4e2e objects   --src DIR --shards s0,s1,s2        each object's shard, as "shard id" lines
//	c4e2e s3get     --list FILE --bucket B --endpoint-fmt http://minio-%s.c4-store:9000
//	                                                  read every listed object straight from S3
//	c4e2e probe     --url URL [--project P] [--token FILE]  one request to a cache tier; prints the status
//	c4e2e frame     --out DIR [--cost 6s] [--heavy A-B:F]    a render unit (P5)
//	c4e2e shard     [--mean 20s] [--flaky P]                 a CI test shard (P5)
//	c4e2e train     --dir DIR [--step 200ms]                 a checkpointing training run (P5)
//
// Content is deterministic pseudo-random bytes keyed by name, so files that
// share a key are byte-identical, and the host can recompute what any pod
// wrote.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/rendezvous"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: c4e2e gen-show|gen-shot|tree-id|verify|render|read ...")
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"gen-show":      genShow,
		"gen-shot":      genShot,
		"tree-id":       treeID,
		"verify":        verify,
		"render":        render,
		"read":          readAll,
		"expect":        expect,
		"add-frames":    addFrames,
		"hold":          hold,
		"objects":       objectsCmd,
		"s3get":         s3get,
		"probe":         probe,
		"frame":         frameCmd,
		"shard":         shardCmd,
		"train":         trainCmd,
		"verify-record": verifyRecord,
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
		return errors.New("usage: c4e2e tree-id DIR|FILE")
	}
	if fi, err := os.Stat(args[0]); err == nil && fi.Mode().IsRegular() {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Println(c4.Identify(f))
		return nil
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

// expect builds the subset of src an include list selects (hardlinks, so
// no bytes are copied, except files a render would change in place, which
// are copied), optionally applies a render or adds frames exactly as a
// pod would, and prints the view ID. This is the host's independent
// computation of what a pod should see and what its capture should be.
func expect(args []string) error {
	fs := flag.NewFlagSet("expect", flag.ExitOnError)
	src := fs.String("src", "", "source directory")
	include := fs.String("include", "", "comma-separated include patterns")
	renderPod := fs.String("render", "", "apply c4e2e render for this pod")
	frames := fs.Int("add-frames", 0, "apply c4e2e add-frames with this count")
	framesDir := fs.String("frames-dir", "", "add-frames directory")
	pod := fs.String("pod", "", "add-frames pod name")
	listing := fs.String("c4m", "", "also write the view's c4m listing here")
	fs.Parse(args)
	var pats []string
	if *include != "" {
		pats = strings.Split(*include, ",")
	}
	f, err := view.NewFilter(pats)
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
		if info.Mode()&os.ModeSymlink != 0 {
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, dst)
		}
		if strings.HasSuffix(rel, "notes.txt") {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, info.Mode().Perm())
		}
		return os.Link(p, dst)
	})
	if err != nil {
		return err
	}
	if *renderPod != "" {
		if err := render([]string{"--pod", *renderPod, tmp}); err != nil {
			return err
		}
	}
	if *frames > 0 {
		if err := addFrames([]string{"--count", fmt.Sprint(*frames), "--dir", *framesDir, "--pod", *pod, tmp}); err != nil {
			return err
		}
	}
	if *listing != "" {
		v, err := view.ScanDir(tmp)
		if err != nil {
			return err
		}
		data, err := v.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(*listing, data, 0o644); err != nil {
			return err
		}
	}
	id, err := view.ScanID(tmp)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// addFrames writes count new frames into DIR/REL, numbered after the
// highest existing frame and keyed by the pod name.
func addFrames(args []string) error {
	fs := flag.NewFlagSet("add-frames", flag.ExitOnError)
	count := fs.Int("count", 3, "frames to add")
	rel := fs.String("dir", "", "directory under DIR")
	pod := fs.String("pod", os.Getenv("POD_NAME"), "keys the new frames")
	kib := fs.Int("frame-kib", 4096, "frame size in KiB")
	fs.Parse(args)
	if fs.NArg() != 1 || *rel == "" {
		return errors.New("usage: c4e2e add-frames --count N --dir REL --pod NAME DIR")
	}
	dir := filepath.Join(fs.Arg(0), filepath.FromSlash(*rel))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := filepath.Base(filepath.Dir(dir))
	last := 0
	for _, e := range ents {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(e.Name(), prefix+"."), "%d.exr", &n); err == nil && n > last {
			last = n
		}
	}
	for i := 1; i <= *count; i++ {
		name := fmt.Sprintf("%s.%04d.exr", prefix, last+i)
		if err := writeKeyed(filepath.Join(dir, name), *pod+"/"+name, int64(*kib)<<10); err != nil {
			return err
		}
	}
	id, err := view.ScanID(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("output %s\n", id)
	return nil
}

// hold sleeps, so a pod stays mounted while the e2e inspects the node.
func hold(args []string) error {
	d := 30 * time.Second
	if len(args) == 1 {
		v, err := time.ParseDuration(args[0])
		if err != nil {
			return err
		}
		d = v
	}
	time.Sleep(d)
	return nil
}

// objectsCmd lists a directory's objects with the shard each lives on, by
// the same rendezvous placement the store uses.
func objectsCmd(args []string) error {
	fs := flag.NewFlagSet("objects", flag.ExitOnError)
	src := fs.String("src", "", "directory")
	shards := fs.String("shards", "s0,s1,s2", "shard names")
	fs.Parse(args)
	v, err := view.ScanDir(*src)
	if err != nil {
		return err
	}
	names := strings.Split(*shards, ",")
	for _, o := range v.Objects() {
		fmt.Println(rendezvous.Rank(names, o.ID[:])[0], o.ID)
	}
	for _, r := range v.Records() {
		fmt.Println(rendezvous.Rank(names, r.ID[:])[0], r.ID)
	}
	return nil
}

// s3get is the naive baseline: what an init container that copies its
// inputs from S3 does, one GET per object per pod.
func s3get(args []string) error {
	fs := flag.NewFlagSet("s3get", flag.ExitOnError)
	list := fs.String("list", "", "file of \"shard id\" lines")
	bucket := fs.String("bucket", "", "bucket")
	prefix := fs.String("prefix", "c4/", "key prefix")
	endpointFmt := fs.String("endpoint-fmt", "http://minio-%s.c4-store:9000", "endpoint, %s is the shard number")
	fs.Parse(args)
	data, err := os.ReadFile(*list)
	if err != nil {
		return err
	}
	creds := storeurl.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
	shards := map[string]storeurl.Shard{}
	start := time.Now()
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, idText, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		sh, ok := shards[name]
		if !ok {
			url := fmt.Sprintf("s3://%s/%s?endpoint=%s", *bucket, *prefix, fmt.Sprintf(*endpointFmt, strings.TrimPrefix(name, "s")))
			sh, err = storeurl.Open(url, creds)
			if err != nil {
				return err
			}
			shards[name] = sh
		}
		id, err := c4.Parse(idText)
		if err != nil {
			return err
		}
		body, _, _, err := sh.Get(context.Background(), id, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", idText, err)
		}
		n, err := io.Copy(io.Discard, body)
		body.Close()
		if err != nil {
			return err
		}
		total += n
	}
	fmt.Printf("read %d bytes from S3 in %s\n", total, time.Since(start).Round(time.Millisecond))
	return nil
}
