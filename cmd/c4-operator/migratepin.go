package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/Avalanche-io/c4"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/pinmove"
	"github.com/Avalanche-io/c4-operator/internal/project"
)

// runMigratePin migrates one project's stored views to the c4 library this
// binary is built with (internal/pinmove). It is run once per project with
// the operator stopped, before the new operator starts: dry first (the
// default, which writes nothing and reports what a run would do), then
// with --write.
func runMigratePin(args []string) error {
	fs := flag.NewFlagSet("migrate-pin", flag.ContinueOnError)
	name := fs.String("project", "", "the project to migrate (required)")
	namespace := fs.String("namespace", "c4-system", "the operator's namespace: the projects' store credentials, and the farm's Tools")
	write := fs.Bool("write", false, "migrate; without it nothing is written, and the report says what a run would do")
	var mappings, named multiFlag
	fs.Var(&mappings, "mapping", "a git-import mapping file of the project's store, on this machine (repeatable)")
	fs.Var(&named, "view", "a view's C4 ID named outside the cluster, a tracker's link say (repeatable)")
	viewsFile := fs.String("views", "", "a file of view C4 IDs named outside the cluster, one per line")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: c4-operator migrate-pin --project NAME [--mapping FILE]... [--view ID]... [--views FILE] [--write]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *name == "" {
		return errors.New("--project is required")
	}
	ids, err := namedViews(named, *viewsFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: newScheme()})
	if err != nil {
		return err
	}
	reg := &project.Registry{Cache: c, Secrets: c, Namespace: *namespace}
	return pinmove.Migrate(ctx, pinmove.Options{
		Client: c, Project: *name, FarmNamespace: *namespace, Mappings: mappings, Named: ids, Write: *write,
		Store: func(ctx context.Context, p *c4v1.Project) (pinmove.Store, error) { return reg.Store(ctx, p) },
		Out:   os.Stdout, Log: os.Stderr,
	})
}

// namedViews are the IDs given by --view and in the --views file.
func namedViews(flags []string, file string) ([]c4.ID, error) {
	texts := append([]string(nil), flags...)
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				texts = append(texts, line)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	var ids []c4.ID
	for _, s := range texts {
		id, err := c4.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("view %q: %w", s, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
