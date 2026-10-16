package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// listFlag is a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

const importGitUsage = `usage: c4-operator import-git [flags]

Imports the first-parent history of a revision. Each commit becomes a view
and a changeset over its parent's view; file objects, records and
changesets go to the project store. c4git pointer files become their
content's IDs, and their objects are copied from the c4git store without
rehashing. With --out, one JSON line per commit is appended, and a later
run continues after the newest commit the file lists.

Each ref's chain (its commits' changesets as one c4m chain, DESIGN.md 2.5)
is kept in OUT.chains/ and indexed in the mapping; --export-chain REF
writes it with its base in full, which c4 log and c4 split read.

Push-driven modes (these need --out, which holds the last commit per ref):

  --since-last --ref R   import only R's commits after the last one imported
  --post-receive         run as a git post-receive hook: read "<old> <new>
                         <ref>" lines and import each ref's new commits
  --listen ADDR          serve push webhooks (POST /import with ref and
                         after), GET /refs and GET /commits/SHA

S3 shards take credentials from AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY.

flags:
`

func runImportGit(args []string) error {
	fs := flag.NewFlagSet("import-git", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), importGitUsage)
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "git repository (default: $GIT_DIR in a hook, else .)")
	rev := fs.String("rev", "", "revision whose first-parent history is imported (default: --ref, else HEAD)")
	ref := fs.String("ref", "", "ref the commits are recorded under in the mapping, e.g. refs/heads/main")
	maxCommits := fs.Int("max-commits", 0, "import only the newest N commits (0: all)")
	since := fs.String("since", "", "import only commits after this one")
	sinceLast := fs.Bool("since-last", false, "import only commits after the last one the mapping records for --ref")
	postReceive := fs.Bool("post-receive", false, "run as a git post-receive hook")
	listen := fs.String("listen", "", "serve push webhooks on this address, e.g. :8790")
	secretEnv := fs.String("webhook-secret-env", "C4_WEBHOOK_SECRET", "environment variable holding the webhook secret (unset: no check)")
	fetch := fs.Bool("fetch", false, "git fetch origin before each import (the repository is a mirror of the git host)")
	name := fs.String("name", "", "repository name a webhook may give in \"repo\" (default: the directory name)")
	var shards listFlag
	fs.Var(&shards, "store", "project store shard as NAME=URL or a bare URL (s3://, c4d://, http(s)://, /path); names decide placement and must match the Project; repeatable or comma-separated; default $C4_STORE")
	fs.Var(&shards, "shard", "the same as -store (the flag ingest takes)")
	c4gitStore := fs.String("c4git-store", "", "c4git store directory (default REPO/.c4/store when it exists)")
	out := fs.String("out", "", "mapping file to append one JSON line per commit to")
	quiet := fs.Bool("quiet", false, "print only the summary")
	jobs := fs.Int("jobs", 8, "concurrent store writes")
	chainMax := fs.Int("chain-max", 1024, "re-base a branch's chain after this many sections (0: never)")
	exportChain := fs.String("export-chain", "", "write this ref's chain to standard output with its base view in full (for c4 log and c4 split), and exit")
	signerURL := fs.String("signer", "", "the farm's certificate issuer (for example http://c4-signer.c4-system.svc): sign an import record per commit as this importer's workload identity")
	signToken := fs.String("sign-token-file", "", "service account token (audience c4-signer) that proves the importer's identity to the issuer")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("unexpected arguments")
	}
	if *repo == "" {
		*repo = "."
		if d := os.Getenv("GIT_DIR"); *postReceive && d != "" {
			*repo = d
		}
	}
	// A hook's environment points git at the hook's repository; the
	// importer names its repository explicitly instead.
	os.Unsetenv("GIT_DIR")
	if len(shards) == 0 {
		shards.Set(os.Getenv("C4_STORE"))
	}
	if len(shards) == 0 {
		return errors.New("no project store: pass --store or set C4_STORE")
	}
	if (*postReceive || *listen != "" || *sinceLast || *exportChain != "") && *out == "" {
		return errors.New("--post-receive, --listen, --since-last and --export-chain need --out, the mapping that records each ref")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	creds := storeurl.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
	var specs []storeurl.Spec
	for _, sh := range shards {
		specs = append(specs, storeurl.ParseSpec(sh))
	}
	st, err := storeurl.OpenSharded(specs, creds)
	if err != nil {
		return err
	}
	svc := &importService{
		base: gitimport.Options{
			Repo: *repo, MaxCommits: *maxCommits, Since: *since,
			Sink:    &gitimport.ShardedSink{S: st},
			Jobs:    *jobs,
			Records: view.StoreRecords(st),
			Warn:    func(s string) { fmt.Fprintln(os.Stderr, "warning: "+s) },
		},
		out: *out, fetch: *fetch, quiet: *quiet, log: os.Stdout, name: *name,
		chainMax: *chainMax,
	}
	if svc.name == "" {
		abs, _ := filepath.Abs(*repo)
		svc.name = strings.TrimSuffix(filepath.Base(abs), ".git")
	}
	if svc.signer, err = remoteSigner(ctx, *signerURL, *signToken); err != nil {
		return fmt.Errorf("signer: %w", err)
	}
	src, err := openC4GitStore(*repo, *c4gitStore)
	if err != nil {
		return err
	}
	if src != nil {
		svc.base.C4Git = src
	}

	switch {
	case *exportChain != "":
		data, err := os.ReadFile(svc.chainPath(*exportChain))
		if err != nil {
			return err
		}
		full, err := view.ExportChain(ctx, data, svc.base.Records)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(full)
		return err
	case *listen != "":
		svc.log = os.Stderr
		return svc.listen(ctx, *listen, os.Getenv(*secretEnv))
	case *postReceive:
		svc.quiet = true
		return svc.postReceive(ctx, os.Stdin, os.Stdout)
	}
	// A one-shot import records its chain under the revision's ref
	// (HEAD on a branch is that branch) and stores the whole chain.
	if *ref == "" {
		*ref = gitimport.SymbolicRef(ctx, *repo, *rev)
	}
	svc.storeChain = true
	res, err := svc.importRef(ctx, *ref, *rev, *sinceLast)
	if err != nil {
		return err
	}
	if res.ChainObject != "" && !*quiet {
		fmt.Printf("chain %s for %s: %d sections from %s, head %s\n", res.ChainObject, *ref, res.ChainSections, shortID(mustParse(res.ChainBase)), shortID(mustParse(res.ChainHead)))
	}
	if *sinceLast {
		fmt.Println(res.line())
		return nil
	}
	return printSummary(ctx, res.summary, st)
}

// openC4GitStore opens the c4git store: the flag's directory, or the
// repository's .c4/store when it exists.
func openC4GitStore(repo, dir string) (gitimport.Source, error) {
	if dir != "" {
		return gitimport.OpenTreeSource(dir)
	}
	def := filepath.Join(repo, ".c4", "store")
	if _, err := os.Stat(def); err != nil {
		return nil, nil
	}
	return gitimport.OpenTreeSource(def)
}

func printCommitTo(w io.Writer, c gitimport.Commit) {
	kind := ""
	if c.Base {
		kind = " base"
	}
	fmt.Fprintf(w, "%s%s view %s +%d ~%d -%d files %d up %d/%s present %d/%s c4git %d records r%d w%d %.1fms (store %.1fms)\n",
		short(c.Commit), kind, shortID(c.View), c.Added, c.Modified, c.Deleted, c.Files,
		c.ObjectsUploaded, human(c.BytesUploaded), c.ObjectsPresent, human(c.BytesPresent),
		c.C4GitObjects, c.RecordsRead, c.RecordsWritten, c.Millis, c.SinkMillis)
}

func printSummary(ctx context.Context, sum *gitimport.Summary, st view.Store) error {
	var (
		up, present     int
		upB, presentB   int64
		c4git, rr, rw   int
		ms              []float64
		largest         gitimport.Commit
		blobsRead, ptrs int
		c4gitB          int64
		incrementalMs   float64
		sinkMs          []float64
	)
	for i, c := range sum.Commits {
		up += c.ObjectsUploaded
		upB += c.BytesUploaded
		present += c.ObjectsPresent
		presentB += c.BytesPresent
		c4git += c.C4GitObjects
		rr += c.RecordsRead
		rw += c.RecordsWritten
		blobsRead += c.BlobsRead
		ptrs += c.Pointers
		c4gitB += c.C4GitBytes
		if !c.Base {
			ms = append(ms, c.Millis)
			sinkMs = append(sinkMs, c.SinkMillis)
			incrementalMs += c.Millis
		}
		if i == 0 || c.Files > largest.Files {
			largest = c
		}
	}
	fmt.Printf("commits %d (skipped %d already imported) in %s\n", len(sum.Commits), sum.Skipped, sum.Elapsed.Round(time.Millisecond))
	if len(sum.Commits) == 0 {
		return nil
	}
	sort.Float64s(ms)
	sort.Float64s(sinkMs)
	if len(ms) > 0 {
		fmt.Printf("per commit (excluding base): p50 %.1fms p90 %.1fms p99 %.1fms max %.1fms mean %.1fms\n",
			pct(ms, 50), pct(ms, 90), pct(ms, 99), ms[len(ms)-1], incrementalMs/float64(len(ms)))
		fmt.Printf("store write time per commit, summed over concurrent writes: p50 %.1fms p90 %.1fms max %.1fms\n",
			pct(sinkMs, 50), pct(sinkMs, 90), sinkMs[len(sinkMs)-1])
	}
	fmt.Printf("objects uploaded %d (%s), already present %d (%s)\n", up, human(upB), present, human(presentB))
	fmt.Printf("git blobs read %d: %d hashed as content, %d c4git pointers\n", blobsRead, blobsRead-ptrs, ptrs)
	fmt.Printf("c4git objects used by ID without rehashing %d (%s)\n", c4git, human(c4gitB))
	fmt.Printf("records: parent records read %d, new records %d\n", rr, rw)
	if sum.Submodules > 0 {
		fmt.Printf("submodule entries skipped %d\n", sum.Submodules)
	}
	if sum.Missing > 0 {
		fmt.Printf("c4git objects missing %d (see warnings)\n", sum.Missing)
	}
	v, err := view.Load(ctx, largest.View, view.StoreRecords(st), 16)
	if err != nil {
		return fmt.Errorf("load largest view: %w", err)
	}
	t := v.Totals()
	fmt.Printf("largest view %s at %s: %d files, %d symlinks, %d dirs, %s, %d unique objects (%s)\n",
		shortID(v.ID), short(largest.Commit), t.Files, len(v.Symlinks), t.Dirs, human(t.Bytes), t.UniqueObjects, human(t.UniqueBytes))
	fmt.Printf("head %s view %s\n", sum.Commits[len(sum.Commits)-1].Commit, sum.Commits[len(sum.Commits)-1].View)
	return nil
}

func pct(sorted []float64, p float64) float64 {
	i := int(float64(len(sorted)-1) * p / 100)
	return sorted[i]
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func mustParse(s string) c4.ID {
	id, _ := c4.Parse(s)
	return id
}

func shortID(id c4.ID) string {
	s := id.String()
	if len(s) > 14 {
		return s[:14]
	}
	return s
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
