package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
)

// submissionFile says `submit`'s one argument is a submission (a YAML or
// JSON file saying c4: submit/1), not a prebuilt type's asset.
func submissionFile(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".yaml", ".yml", ".json":
	default:
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

type connectFunc func() (*farmcli.Client, farmcli.Farm, farmcli.Out, string, error)

// toolCommand is `c4 farm tool new|test|publish|show`.
func toolCommand(ctx context.Context, fs *flag.FlagSet, args []string, stdout io.Writer, connect connectFunc) error {
	const use = "usage: c4 farm tool new KIND DIR | test DIR | publish DIR | show NAME"
	if len(args) == 0 {
		return errors.New(use)
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "new":
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return errors.New("usage: c4 farm tool new KIND DIR (KIND: frames or once)")
		}
		return farmcli.NewTool(farmcli.Out{W: stdout}, pos[1], pos[0])

	case "test":
		var a farmcli.TestArgs
		var with multi
		fs.StringVar(&a.Frames, "frames", "", "the frames to run, instead of the test's")
		fs.Var(&with, "with", "a parameter K=V, over the test's (repeatable)")
		fs.StringVar(&a.Image, "image", "", "run this image instead of the tool's (a local build)")
		fs.BoolVar(&a.Keep, "keep", false, "keep the scratch tree to look at")
		fs.StringVar(&a.Log, "log", "", "replay a captured log through the tool's patterns instead of running it")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: c4 farm tool test DIR [--frames F] [--with K=V] [--image IMAGE] [--keep] [--log FILE]")
		}
		a.Dir, a.With = pos[0], map[string]string{}
		for _, s := range with {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				return fmt.Errorf("--with %q: want K=V", s)
			}
			a.With[k] = v
		}
		return farmcli.TestTool(ctx, farmcli.Out{W: stdout}, a)

	case "publish":
		var a farmcli.PublishArgs
		fs.BoolVar(&a.Farm, "farm-wide", false, "publish to every project (the farm admin role)")
		fs.BoolVar(&a.DryRun, "dry-run", false, "check the tool on the farm and change nothing")
		fs.DurationVar(&a.Wait, "wait", 15*time.Minute, "how long to wait for the farm's test run")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: c4 farm tool publish DIR [--farm-wide] [--dry-run]")
		}
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		if a.Project, err = farmcli.ResolveProject(ctx, c, project, f.Project); err != nil {
			return err
		}
		a.Dir = pos[0]
		if f.Local {
			// publishing takes the TD role (farm admin for every
			// project); on the local farm you hold every role
			role, as := "TD", "td"
			if a.Farm {
				role, as = "farm admin", "admin"
			}
			if c.As != as && c.As != "admin" {
				c.As = as
				fmt.Fprintf(stdout, "as        %s: publishing takes the %s role, and the local farm signs you in as it\n", as, role)
			}
		}
		return farmcli.PublishTool(ctx, c, o, a)

	case "show":
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: c4 farm tool show NAME")
		}
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		if project, err = farmcli.ResolveProject(ctx, c, project, f.Project); err != nil {
			return err
		}
		return farmcli.ShowTool(ctx, c, o, project, pos[0])
	}
	return errors.New(use)
}
