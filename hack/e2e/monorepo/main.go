// Command monorepo builds a synthetic source monorepo for the import-git
// measurements: services/<name>/ with Go-like sources and tests,
// libs/common/, tools/ with executable scripts, a few symlinks, and binary
// assets (services/<name>/testdata/*.exr) committed through the real c4git
// filter. Its history has mostly small commits to one service, some to the
// shared library, and a few that change assets. Everything, including
// commit dates and so the commit SHAs, follows from the seed.
//
//	go run ./hack/e2e/monorepo -out /tmp/mono -c4git bin/c4git
//
// The c4git store is the repository's .c4/store; C4_STORE is set to it
// for every git command so the user's global c4 store is never touched.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type gen struct {
	dir   string
	env   []string
	rng   *rand.Rand
	when  time.Time
	files map[string]bool // tracked paths
	svcs  []string
	// assetMin and assetMax bound asset sizes in bytes.
	assetMin, assetMax int
}

func main() {
	out := flag.String("out", "", "directory to create (must not exist)")
	c4git := flag.String("c4git", "bin/c4git", "c4git binary")
	seed := flag.Uint64("seed", 1, "seed")
	commits := flag.Int("commits", 400, "commits after the initial one")
	services := flag.Int("services", 150, "services in the initial tree")
	assetMin := flag.Int("asset-min-mb", 1, "smallest asset, MB")
	assetMax := flag.Int("asset-max-mb", 4, "largest asset, MB")
	flag.Parse()
	if *out == "" {
		log.Fatal("--out is required")
	}
	if _, err := os.Stat(*out); err == nil {
		log.Fatalf("%s exists", *out)
	}
	bin, err := filepath.Abs(*c4git)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		log.Fatalf("c4git: %v (GOBIN=$PWD/bin go install github.com/Avalanche-io/c4git/cmd/c4git@latest)", err)
	}
	dir, err := filepath.Abs(*out)
	if err != nil {
		log.Fatal(err)
	}
	g := &gen{
		dir:      dir,
		rng:      rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15)),
		when:     time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC),
		files:    map[string]bool{},
		assetMin: *assetMin << 20, assetMax: *assetMax << 20,
	}
	g.env = append(os.Environ(),
		"PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"C4_STORE="+filepath.Join(dir, ".c4", "store"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Synthetic", "GIT_AUTHOR_EMAIL=synthetic@example.com",
		"GIT_COMMITTER_NAME=Synthetic", "GIT_COMMITTER_EMAIL=synthetic@example.com",
	)
	start := time.Now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	g.run("git", "init", "-q", "-b", "main", ".")
	g.run(bin, "init")
	// c4git init configures the filter as "c4git clean %f"; use the
	// absolute binary so PATH does not matter to later git commands.
	g.run("git", "config", "filter.c4.clean", bin+" clean %f")
	g.run("git", "config", "filter.c4.smudge", bin+" smudge %f")

	g.initial(*services)
	for i := range *commits {
		g.step(i)
	}
	n := strings.TrimSpace(g.output("git", "rev-list", "--count", "HEAD"))
	head := strings.TrimSpace(g.output("git", "rev-parse", "HEAD"))
	fmt.Printf("%s: %s commits, %d tracked files, head %s, built in %s\n", dir, n, len(g.files), head, time.Since(start).Round(time.Millisecond))
}

func (g *gen) run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = g.dir
	cmd.Env = append(g.env, "GIT_AUTHOR_DATE="+g.when.Format(time.RFC3339), "GIT_COMMITTER_DATE="+g.when.Format(time.RFC3339))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, stderr.String())
	}
}

func (g *gen) output(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = g.dir
	cmd.Env = g.env
	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return string(out)
}

// write puts content at p and tracks it.
func (g *gen) write(p string, data []byte, mode os.FileMode) {
	full := filepath.Join(g.dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		log.Fatal(err)
	}
	g.files[p] = true
}

func (g *gen) symlink(target, p string) {
	full := filepath.Join(g.dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		log.Fatal(err)
	}
	g.files[p] = true
}

func (g *gen) remove(p string) {
	if err := os.RemoveAll(filepath.Join(g.dir, p)); err != nil {
		log.Fatal(err)
	}
	for f := range g.files {
		if f == p || strings.HasPrefix(f, p+"/") {
			delete(g.files, f)
		}
	}
}

// commit stages the given paths (additions, changes and deletions) and
// commits them, one hour after the previous commit.
func (g *gen) commit(msg string, paths ...string) {
	g.when = g.when.Add(time.Hour)
	args := append([]string{"add", "-A", "--"}, paths...)
	g.run("git", args...)
	g.run("git", "commit", "-q", "-m", msg)
}

var words = strings.Fields(`account adapter batch buffer cache channel client codec config
context cursor decoder digest encoder event filter frame handler index layer ledger
lease limiter loader mapper merger metric node packet parser planner pool queue reader
record region render replica request resolver router sampler scheduler session shard
signal snapshot source span stage store stream task tensor ticket token tracer upload
validator vector window worker writer`)

func (g *gen) word() string { return words[g.rng.IntN(len(words))] }

func title(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// goSource returns a plausible Go file of a few dozen to a few hundred
// lines.
func (g *gen) goSource(pkg string, test bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	if test {
		b.WriteString("import \"testing\"\n\n")
	} else {
		b.WriteString("import (\n\t\"context\"\n\t\"errors\"\n\t\"fmt\"\n)\n\n")
	}
	for range 2 + g.rng.IntN(10) {
		name := title(g.word()) + title(g.word())
		if test {
			fmt.Fprintf(&b, "func Test%s(t *testing.T) {\n", name)
			for range 2 + g.rng.IntN(8) {
				fmt.Fprintf(&b, "\tif got, want := %s(%d), %d; got != want {\n\t\tt.Fatalf(\"got %%v, want %%v\", got, want)\n\t}\n", g.word(), g.rng.IntN(1000), g.rng.IntN(1000))
			}
			b.WriteString("}\n\n")
			continue
		}
		fmt.Fprintf(&b, "// %s handles a %s for the %s.\n", name, g.word(), g.word())
		fmt.Fprintf(&b, "func %s(ctx context.Context, %s string, n int) error {\n", name, g.word())
		for range 3 + g.rng.IntN(14) {
			switch g.rng.IntN(3) {
			case 0:
				fmt.Fprintf(&b, "\tif n > %d {\n\t\treturn errors.New(%q)\n\t}\n", g.rng.IntN(4096), g.word()+" "+g.word())
			case 1:
				fmt.Fprintf(&b, "\tfor i := range n {\n\t\t_ = fmt.Sprint(i, %q)\n\t}\n", g.word())
			default:
				fmt.Fprintf(&b, "\tn += %d // %s\n", g.rng.IntN(64), g.word())
			}
		}
		b.WriteString("\treturn ctx.Err()\n}\n\n")
	}
	return []byte(b.String())
}

func (g *gen) script() []byte {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	for range 3 + g.rng.IntN(12) {
		fmt.Fprintf(&b, "echo %s-%s %d\n", g.word(), g.word(), g.rng.IntN(100))
	}
	return []byte(b.String())
}

// asset returns incompressible bytes of a random size in the asset range.
func (g *gen) asset() []byte {
	n := g.assetMin + g.rng.IntN(g.assetMax-g.assetMin+1)
	var key [32]byte
	for i := range key {
		key[i] = byte(g.rng.Uint32())
	}
	r := rand.NewChaCha8(key)
	buf := make([]byte, n)
	r.Read(buf)
	copy(buf, "v/1\x01") // the OpenEXR magic number, for realism
	return buf
}

func (g *gen) newService(i int) string {
	name := fmt.Sprintf("svc-%03d-%s", i, g.word())
	g.svcs = append(g.svcs, name)
	base := "services/" + name
	pkg := strings.ReplaceAll(strings.Split(name, "-")[2], "-", "")
	for range 3 + g.rng.IntN(5) {
		w := g.word()
		g.write(fmt.Sprintf("%s/internal/%s/%s.go", base, w, w), g.goSource(w, false), 0o644)
		g.write(fmt.Sprintf("%s/internal/%s/%s_test.go", base, w, w), g.goSource(w, true), 0o644)
	}
	g.write(base+"/main.go", g.goSource("main", false), 0o644)
	g.write(base+"/main_test.go", g.goSource("main", true), 0o644)
	g.write(base+"/BUILD", []byte(fmt.Sprintf("go_binary(name = %q, deps = [\"//libs/common\"])\n", pkg)), 0o644)
	g.write(base+"/README.md", []byte("# "+name+"\n\nOwns the "+g.word()+" path.\n"), 0o644)
	if g.rng.IntN(4) == 0 {
		for k := range 1 + g.rng.IntN(2) {
			g.write(fmt.Sprintf("%s/testdata/plate_%d.exr", base, k), g.asset(), 0o644)
		}
	}
	return base
}

func (g *gen) initial(services int) {
	for i := range services {
		g.newService(i)
	}
	for range 30 {
		w := g.word()
		g.write("libs/common/"+w+"/"+w+".go", g.goSource(w, false), 0o644)
		g.write("libs/common/"+w+"/"+w+"_test.go", g.goSource(w, true), 0o644)
	}
	g.write("libs/common/config/default.yaml", []byte("log_level: info\nregion: us-west-2\n"), 0o644)
	for _, s := range []string{"build", "lint", "fmt", "release", "bootstrap", "test-shard"} {
		g.write("tools/"+s+".sh", g.script(), 0o755)
	}
	g.write("tools/README.md", []byte("Build and CI scripts.\n"), 0o644)
	g.symlink("build.sh", "tools/b")
	g.symlink("../../libs/common/config/default.yaml", "services/"+g.svcs[0]+"/config.yaml")
	g.write("go.mod", []byte("module example.com/mono\n\ngo 1.26\n"), 0o644)
	g.write("README.md", []byte("# mono\n\nA synthetic monorepo.\n"), 0o644)
	g.commit("initial import", ".")
}

// hot picks a service with a skew, so a few services see most changes.
func (g *gen) hot() string {
	n := len(g.svcs)
	i := int(float64(n) * g.rng.Float64() * g.rng.Float64())
	return "services/" + g.svcs[i]
}

// under lists tracked regular files under dir with suffix, sorted.
func (g *gen) under(dir, suffix string) []string {
	var out []string
	for f := range g.files {
		if strings.HasPrefix(f, dir+"/") && strings.HasSuffix(f, suffix) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func (g *gen) pick(list []string) string { return list[g.rng.IntN(len(list))] }

func pkgOf(p string) string {
	base := filepath.Base(filepath.Dir(p))
	if strings.HasPrefix(base, "svc-") {
		return "main"
	}
	return base
}

func (g *gen) step(i int) {
	r := g.rng.Float64()
	switch {
	case r < 0.78:
		g.serviceChange(i)
	case r < 0.90:
		g.libChange(i)
	case r < 0.94:
		g.toolChange(i)
	case r < 0.975:
		g.assetChange(i)
	case r < 0.99:
		base := g.newService(len(g.svcs))
		g.commit(fmt.Sprintf("%d: add %s", i, filepath.Base(base)), base)
	default:
		if len(g.svcs) < 10 {
			g.serviceChange(i)
			return
		}
		k := 1 + g.rng.IntN(len(g.svcs)-1) // never the one with the symlink
		base := "services/" + g.svcs[k]
		g.svcs = append(g.svcs[:k], g.svcs[k+1:]...)
		g.remove(base)
		g.commit(fmt.Sprintf("%d: remove %s", i, filepath.Base(base)), base)
	}
}

func (g *gen) serviceChange(i int) {
	base := g.hot()
	var paths []string
	srcs := g.under(base, ".go")
	for range 1 + g.rng.IntN(4) {
		p := g.pick(srcs)
		g.write(p, g.goSource(pkgOf(p), strings.HasSuffix(p, "_test.go")), 0o644)
		paths = append(paths, p)
	}
	switch x := g.rng.IntN(100); {
	case x < 10:
		w := g.word()
		p := fmt.Sprintf("%s/internal/%s/%s_%d.go", base, w, w, i)
		g.write(p, g.goSource(w, false), 0o644)
		paths = append(paths, p)
	case x < 15 && len(srcs) > 4:
		p := g.pick(srcs)
		g.remove(p)
		paths = append(paths, p)
	case x < 18 && len(srcs) > 4:
		p := g.pick(srcs)
		data, err := os.ReadFile(filepath.Join(g.dir, p))
		if err != nil {
			log.Fatal(err)
		}
		np := strings.TrimSuffix(p, ".go") + "_moved.go"
		if !g.files[np] {
			g.remove(p)
			g.write(np, data, 0o644)
			paths = append(paths, p, np)
		}
	}
	g.commit(fmt.Sprintf("%d: %s: update %s", i, filepath.Base(base), g.word()), paths...)
}

func (g *gen) libChange(i int) {
	var paths []string
	srcs := g.under("libs/common", ".go")
	for range 1 + g.rng.IntN(3) {
		p := g.pick(srcs)
		g.write(p, g.goSource(pkgOf(p), strings.HasSuffix(p, "_test.go")), 0o644)
		paths = append(paths, p)
	}
	if g.rng.IntN(8) == 0 {
		w := g.word()
		p := fmt.Sprintf("libs/common/%s/%s_%d.go", w, w, i)
		g.write(p, g.goSource(w, false), 0o644)
		paths = append(paths, p)
	}
	g.commit(fmt.Sprintf("%d: common: %s", i, g.word()), paths...)
}

func (g *gen) toolChange(i int) {
	p := g.pick(g.under("tools", ".sh"))
	g.write(p, g.script(), 0o755)
	g.commit(fmt.Sprintf("%d: tools: %s", i, filepath.Base(p)), p)
}

func (g *gen) assetChange(i int) {
	assets := g.under("services", ".exr")
	if len(assets) > 0 && g.rng.IntN(3) > 0 {
		p := g.pick(assets)
		g.write(p, g.asset(), 0o644)
		g.commit(fmt.Sprintf("%d: regenerate %s", i, p), p)
		return
	}
	base := g.hot()
	p := fmt.Sprintf("%s/testdata/plate_%d.exr", base, i)
	g.write(p, g.asset(), 0o644)
	g.commit(fmt.Sprintf("%d: add %s", i, p), p)
}
