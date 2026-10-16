package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runIngest hashes a directory (the verify-once point), uploads every
// object the project's store lacks to its shard, publishes the view's
// records, and prints the view ID. Credentials come from the environment
// (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY), as with the c4 CLI.
func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	var shards multiFlag
	fs.Var(&shards, "shard", "store shard as name=URL (repeat for each shard; names decide placement and must match the Project)")
	workers := fs.Int("workers", 16, "parallel uploads")
	out := fs.String("c4m", "", "also write the view as a nested c4m file here")
	quiet := fs.Bool("q", false, "print only the view ID")
	signerURL := fs.String("signer", "", "the farm's certificate issuer: sign an ingest record of the view as this workload's identity")
	signToken := fs.String("sign-token-file", "", "service account token (audience c4-signer) that proves the ingesting workload's identity to the issuer")
	recordOut := fs.String("record-out", "", "write the ingest record's ID to this file")
	fs.Parse(args)
	if fs.NArg() != 1 || len(shards) == 0 {
		return errors.New("usage: c4-operator ingest --shard NAME=URL [--shard NAME=URL...] [--c4m FILE] DIR")
	}
	dir := fs.Arg(0)
	creds := storeurl.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
	var specs []storeurl.Spec
	for _, sh := range shards {
		specs = append(specs, storeurl.ParseSpec(sh))
	}
	st, err := storeurl.OpenSharded(specs, creds)
	if err != nil {
		return err
	}
	ctx := context.Background()
	signer, err := remoteSigner(ctx, *signerURL, *signToken)
	if err != nil {
		return fmt.Errorf("signer: %w", err)
	}
	start := time.Now()
	v, err := view.ScanDir(dir)
	if err != nil {
		return err
	}
	scanned := time.Since(start)

	// One path per unique object.
	paths := map[c4.ID]string{}
	for _, f := range v.Files {
		if _, ok := paths[f.ID]; !ok {
			paths[f.ID] = filepath.Join(dir, filepath.FromSlash(f.Path))
		}
	}
	objs := v.Objects()
	var (
		uploaded, present atomic.Int64
		bytesUp           atomic.Int64
		next              atomic.Int64
		wg                sync.WaitGroup
		mu                sync.Mutex
		first             error
	)
	for range min(*workers, max(len(objs), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(objs) {
					return
				}
				o := objs[i]
				if _, err := st.Head(ctx, o.ID); err == nil {
					present.Add(1)
					continue
				} else if !errors.Is(err, storeurl.ErrNotFound) {
					mu.Lock()
					first = errors.Join(first, err)
					mu.Unlock()
					return
				}
				f, err := os.Open(paths[o.ID])
				if err != nil {
					mu.Lock()
					first = errors.Join(first, err)
					mu.Unlock()
					return
				}
				err = st.Put(ctx, o.ID, f, o.Size)
				f.Close()
				if err != nil {
					mu.Lock()
					first = errors.Join(first, fmt.Errorf("%s: %w", o.ID, err))
					mu.Unlock()
					return
				}
				uploaded.Add(1)
				bytesUp.Add(o.Size)
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	for _, r := range v.Records() {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			return fmt.Errorf("record %s: %w", r.ID, err)
		}
	}
	record := ""
	if signer != nil {
		// The ingest record vouches for the view as hashed here, the point
		// where content is verified once (DESIGN.md section 1).
		t := v.Totals()
		pred := map[string]any{"source": filepath.Base(filepath.Clean(dir)), "files": t.Files, "bytes": t.Bytes,
			"uniqueObjects": t.UniqueObjects, "uploaded": uploaded.Load(), "ingester": "c4-operator ingest " + version}
		rec, err := provenance.SignStatement(ctx, signer, provenance.PredicateIngest, []provenance.Subject{provenance.SubjectFor("view", v.ID)}, pred, time.Now())
		if err != nil {
			return fmt.Errorf("ingest record: %w", err)
		}
		id, err := st.PutBytes(ctx, rec)
		if err != nil {
			return fmt.Errorf("ingest record: %w", err)
		}
		record = id.String()
		if *recordOut != "" {
			if err := os.WriteFile(*recordOut, []byte(record+"\n"), 0o644); err != nil {
				return err
			}
		}
	}
	if *out != "" {
		data, err := v.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
	}
	if *quiet {
		fmt.Println(v.ID)
		return nil
	}
	t := v.Totals()
	fmt.Printf("view %s\n", v.ID)
	if record != "" {
		fmt.Printf("record %s signed by %s\n", record, signer.Identity())
	}
	fmt.Printf("files=%d dirs=%d bytes=%d unique=%d uniqueBytes=%d uploaded=%d present=%d bytesUploaded=%d scan=%s total=%s\n",
		t.Files, t.Dirs, t.Bytes, t.UniqueObjects, t.UniqueBytes, uploaded.Load(), present.Load(), bytesUp.Load(),
		scanned.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	return nil
}
