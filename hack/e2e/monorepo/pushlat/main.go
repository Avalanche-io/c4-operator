// Command pushlat measures push-to-view latency: the time from `git push`
// to the pushed commit's view being stored and resolvable, for small
// source commits and for commits that add a c4git asset.
//
// One clone of the synthetic monorepo pushes every commit to three bare
// remotes:
//
//	plain     no hook: what the push alone costs
//	hook      a post-receive hook runs `c4-operator import-git
//	          --post-receive` in the remote (a self-hosted remote)
//	webhook   a post-receive hook POSTs the push to `c4-operator import-git
//	          --listen`, which fetches into its mirror and imports (the
//	          shape of a git host webhook, without the host's delivery delay)
//
// git waits for post-receive, so a push returns once its import is done.
// After each push the harness checks that the commit's mapping line exists,
// that its root record and changeset are in the store, and resolves the
// changeset with view.FromID.
//
//	go run ./hack/e2e/monorepo/pushlat -repo /tmp/c4-monorepo-e2e/repo -out /tmp/c4-pushlat
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

type rig struct {
	out, bin, c4git, c4store string
	env                      []string
	rng                      *rand.Rand
	sources                  []string
	services                 []string
}

type remote struct {
	name    string
	dir     string // bare repository pushed to
	mapping string // "" for plain
	st      *storeurl.Sharded
}

type sample struct {
	push, stored, resolve, importMs float64
}

func main() {
	repo := flag.String("repo", "", "synthetic monorepo (from hack/e2e/monorepo), with its .c4/store")
	out := flag.String("out", "", "work directory to create")
	bin := flag.String("bin", "bin", "directory holding c4-operator and c4git")
	small := flag.Int("small", 30, "small source commits")
	assets := flag.Int("assets", 10, "commits that add a c4git asset")
	assetMB := flag.Int("asset-mb", 3, "asset size, MB")
	flag.Parse()
	if *repo == "" || *out == "" {
		log.Fatal("--repo and --out are required")
	}
	if _, err := os.Stat(*out); err == nil {
		log.Fatalf("%s exists", *out)
	}
	absBin, _ := filepath.Abs(*bin)
	absRepo, _ := filepath.Abs(*repo)
	absOut, _ := filepath.Abs(*out)
	r := &rig{
		out: absOut, bin: filepath.Join(absBin, "c4-operator"), c4git: filepath.Join(absBin, "c4git"),
		// Developers and the importer share one c4git store, as they must
		// for the importer to find pushed assets.
		c4store: filepath.Join(absRepo, ".c4", "store"),
		rng:     rand.New(rand.NewPCG(7, 7)),
	}
	r.env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "C4_STORE="+r.c4store,
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	must(os.MkdirAll(r.out, 0o755))

	plain := &remote{name: "plain", dir: r.path("plain.git")}
	hook := &remote{name: "hook", dir: r.path("hook.git"), mapping: r.path("map-hook.jsonl")}
	web := &remote{name: "webhook", dir: r.path("webhook.git"), mapping: r.path("map-webhook.jsonl")}
	for _, rm := range []*remote{plain, hook, web} {
		r.git("", "clone", "-q", "--bare", absRepo, rm.dir)
	}
	mirror := r.path("mirror.git")
	r.git("", "clone", "-q", "--mirror", web.dir, mirror)

	// Each importer starts from a full import of the history, untimed.
	hook.st = r.shards("shards-hook")
	web.st = r.shards("shards-webhook")
	r.run(r.bin, "import-git", "-repo", hook.dir, "-ref", "refs/heads/main", "-c4git-store", r.c4store, "-out", hook.mapping, "-quiet", r.storeFlags("shards-hook"))
	r.run(r.bin, "import-git", "-repo", mirror, "-ref", "refs/heads/main", "-c4git-store", r.c4store, "-out", web.mapping, "-quiet", r.storeFlags("shards-webhook"))

	r.hookScript(hook.dir, fmt.Sprintf("exec %q import-git -post-receive -c4git-store %q -out %q %s\n",
		r.bin, r.c4store, hook.mapping, r.storeFlags("shards-hook")))
	port := freePort()
	r.hookScript(web.dir, fmt.Sprintf(`while read old new ref; do
	curl -fsS -X POST -H 'Content-Type: application/json' -d "{\"ref\":\"$ref\",\"after\":\"$new\"}" http://127.0.0.1:%d/import >/dev/null
done
`, port))
	listener := exec.Command(r.bin, append([]string{"import-git", "-listen", fmt.Sprintf("127.0.0.1:%d", port), "-repo", mirror, "-fetch",
		"-c4git-store", r.c4store, "-out", web.mapping}, strings.Fields(r.storeFlags("shards-webhook"))...)...)
	listener.Env = r.env
	logf, err := os.Create(r.path("listener.log"))
	must(err)
	listener.Stdout, listener.Stderr = logf, logf
	must(listener.Start())
	defer func() {
		listener.Process.Signal(syscall.SIGINT)
		listener.Wait()
	}()
	waitHTTP(port)

	work := r.path("work")
	r.git("", "clone", "-q", "-c", "filter.c4.clean="+r.c4git+" clean %f", "-c", "filter.c4.smudge="+r.c4git+" smudge %f",
		"-c", "filter.c4.required=true", plain.dir, work)
	for _, rm := range []*remote{plain, hook, web} {
		r.git(work, "remote", "add", rm.name, rm.dir)
	}
	r.scan(work)

	results := map[string]map[string][]sample{"small": {}, "asset": {}}
	kinds := make([]string, 0, *small+*assets)
	for range *small {
		kinds = append(kinds, "small")
	}
	for range *assets {
		kinds = append(kinds, "asset")
	}
	r.rng.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	for i, kind := range kinds {
		sha := r.change(work, kind, i, *assetMB)
		for _, rm := range []*remote{plain, hook, web} {
			s := r.pushAndCheck(work, rm, sha)
			results[kind][rm.name] = append(results[kind][rm.name], s)
		}
	}

	fmt.Printf("push-to-view latency, %d small commits and %d asset commits (%d MB each)\n", *small, *assets, *assetMB)
	for _, kind := range []string{"small", "asset"} {
		for _, name := range []string{"plain", "hook", "webhook"} {
			ss := results[kind][name]
			if len(ss) == 0 {
				continue
			}
			pick := func(f func(sample) float64) (p50, p90, max float64) {
				xs := make([]float64, len(ss))
				for i, s := range ss {
					xs[i] = f(s)
				}
				sort.Float64s(xs)
				return xs[len(xs)/2], xs[int(float64(len(xs)-1)*0.9)], xs[len(xs)-1]
			}
			p50, p90, mx := pick(func(s sample) float64 { return s.push })
			line := fmt.Sprintf("%-6s %-8s push returns p50 %6.1fms p90 %6.1fms max %6.1fms", kind, name, p50, p90, mx)
			if name != "plain" {
				s50, s90, smax := pick(func(s sample) float64 { return s.stored })
				i50, _, _ := pick(func(s sample) float64 { return s.importMs })
				v50, _, _ := pick(func(s sample) float64 { return s.resolve })
				line += fmt.Sprintf(" | view stored p50 %6.1fms p90 %6.1fms max %6.1fms | import p50 %5.1fms | full resolve p50 %5.1fms", s50, s90, smax, i50, v50)
			}
			fmt.Println(line)
		}
	}
}

func (r *rig) path(p string) string { return filepath.Join(r.out, p) }

func (r *rig) shards(name string) *storeurl.Sharded {
	var specs []storeurl.Spec
	for _, s := range []string{"a", "b", "c"} {
		d := r.path(filepath.Join(name, s))
		must(os.MkdirAll(d, 0o755))
		specs = append(specs, storeurl.ParseSpec(d))
	}
	st, err := storeurl.OpenSharded(specs, storeurl.Credentials{})
	must(err)
	return st
}

func (r *rig) storeFlags(name string) string {
	var f []string
	for _, s := range []string{"a", "b", "c"} {
		f = append(f, "-store", r.path(filepath.Join(name, s)))
	}
	return strings.Join(f, " ")
}

func (r *rig) hookScript(bare, body string) {
	p := filepath.Join(bare, "hooks", "post-receive")
	must(os.WriteFile(p, []byte("#!/bin/sh\nset -e\n"+body), 0o755))
}

func (r *rig) run(name string, args ...string) {
	var flat []string
	for _, a := range args {
		if strings.HasPrefix(a, "-store ") {
			flat = append(flat, strings.Fields(a)...)
			continue
		}
		flat = append(flat, a)
	}
	cmd := exec.Command(name, flat...)
	cmd.Env = r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("%s %s: %v: %s", name, strings.Join(flat, " "), err, out)
	}
}

func (r *rig) git(dir string, args ...string) string {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// scan lists the clone's Go sources and service directories.
func (r *rig) scan(work string) {
	for _, p := range strings.Split(r.git(work, "ls-files", "services"), "\n") {
		if strings.HasSuffix(p, ".go") {
			r.sources = append(r.sources, p)
		}
		parts := strings.SplitN(p, "/", 3)
		if len(parts) == 3 && (len(r.services) == 0 || r.services[len(r.services)-1] != parts[1]) {
			r.services = append(r.services, parts[1])
		}
	}
}

// change makes and commits one change: 1 to 3 Go files of one service, or
// a new asset in a service's testdata.
func (r *rig) change(work, kind string, i, assetMB int) string {
	if kind == "asset" {
		svc := r.services[r.rng.IntN(len(r.services))]
		p := filepath.Join(work, "services", svc, "testdata", fmt.Sprintf("push_%03d.exr", i))
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		var key [32]byte
		for k := range key {
			key[k] = byte(r.rng.Uint32())
		}
		buf := make([]byte, assetMB<<20)
		rand.NewChaCha8(key).Read(buf)
		must(os.WriteFile(p, buf, 0o644))
	} else {
		base := r.sources[r.rng.IntN(len(r.sources))]
		dir := filepath.Dir(base)
		var sib []string
		for _, s := range r.sources {
			if filepath.Dir(s) == dir {
				sib = append(sib, s)
			}
		}
		for k := range 1 + r.rng.IntN(3) {
			p := filepath.Join(work, sib[(k+i)%len(sib)])
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
			must(err)
			fmt.Fprintf(f, "\n// push %d change %d\n", i, k)
			f.Close()
		}
	}
	r.git(work, "add", "-A")
	r.git(work, "commit", "-q", "-m", fmt.Sprintf("push %d (%s)", i, kind))
	return r.git(work, "rev-parse", "HEAD")
}

// pushAndCheck pushes to rm and, for an importing remote, waits until the
// commit's view is stored and resolves.
func (r *rig) pushAndCheck(work string, rm *remote, sha string) sample {
	t0 := time.Now()
	r.git(work, "push", "-q", rm.name, "HEAD:refs/heads/main")
	s := sample{push: ms(t0)}
	if rm.mapping == "" {
		return s
	}
	var line gitimport.Commit
	for {
		if l, ok := findLine(rm.mapping, sha); ok && present(rm.st, l.View) && present(rm.st, l.Changeset) {
			line = l
			break
		}
		if time.Since(t0) > time.Minute {
			log.Fatalf("%s: view of %s not stored after a minute", rm.name, sha)
		}
		time.Sleep(time.Millisecond)
	}
	s.stored = ms(t0)
	s.importMs = line.Millis
	t1 := time.Now()
	res, err := view.FromID(line.Changeset, rm.st, view.Filter{})
	if err != nil || res.View().ID != line.View || res.Chain[0].ID != line.ParentView {
		log.Fatalf("%s: changeset of %s does not resolve to its view: %v", rm.name, sha, err)
	}
	s.resolve = ms(t1)
	return s
}

func present(st *storeurl.Sharded, id c4.ID) bool {
	_, err := st.Head(context.Background(), id)
	return err == nil
}

// findLine returns the mapping line of a commit.
func findLine(p, sha string) (gitimport.Commit, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return gitimport.Commit{}, false
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<16), 64<<20)
	var found gitimport.Commit
	ok := false
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(sha)) {
			continue
		}
		var c gitimport.Commit
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.Commit == sha && !c.RefOnly {
			found, ok = c, true
		}
	}
	return found, ok
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(port int) {
	for range 200 {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/refs", port))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	log.Fatal("listener did not start")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
