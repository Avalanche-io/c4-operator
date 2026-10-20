// Command toollock writes the tool.lock of each of the release's own tools
// (tools/<name>/), pinning its images at the release's tag by digest, as
// publishing would. The release build runs it before c4-farm is built, so
// the folders c4-farm embeds are the trees c4 farm up publishes as they are.
//
//	go run ./hack/toollock -version v0.1.0 [-dir tools]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/imageref"
	"github.com/Avalanche-io/c4-operator/schema"
)

func main() {
	version := flag.String("version", "", "the release whose images the locks pin (v0.1.0)")
	dir := flag.String("dir", "tools", "the folder of tool folders")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "toollock: -version is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := run(ctx, &imageref.Resolver{}, *dir, *version); err != nil {
		fmt.Fprintln(os.Stderr, "toollock:", err)
		os.Exit(1)
	}
}

type pinner interface {
	Pin(ctx context.Context, ref string) (string, error)
}

func run(ctx context.Context, z pinner, dir, version string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name, "tool.yaml"))
		if err != nil {
			return err
		}
		t, errs := schema.ParseTool(b)
		if len(errs) > 0 {
			return fmt.Errorf("%s: %s: %s", name, errs[0].Path, errs[0].Message)
		}
		lock := schema.Lock{C4: "lock/1", Images: map[string]string{}}
		refs := []string{t.Runs.Image}
		for _, sc := range t.Runs.Sidecars {
			refs = append(refs, sc.Image)
		}
		for _, ref := range refs {
			if ref == "" || lock.Images[ref] != "" {
				continue
			}
			p, err := z.Pin(ctx, schema.ImageRef(ref, version))
			if err != nil {
				return fmt.Errorf("%s: %s: %w", name, ref, err)
			}
			lock.Images[ref] = p
		}
		// the bytes the farm writes when it publishes (internal/hud/server)
		out, _ := json.MarshalIndent(lock, "", "  ")
		out = append(out, '\n')
		if err := os.WriteFile(filepath.Join(dir, name, "tool.lock"), out, 0o644); err != nil {
			return err
		}
		fmt.Printf("%-10s %s\n", name, lock.Images[t.Runs.Image])
	}
	return nil
}
