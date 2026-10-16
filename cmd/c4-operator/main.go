// Command c4-operator is every component of the operator in one binary.
//
//	c4-operator tree-id DIR     view ID of a directory
//
// The manager, node daemon, regional tier and tools are added as they
// land on branch v2.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

const usage = `usage: c4-operator <command> [flags]

commands:
  tree-id   print the view ID of a directory
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "tree-id":
		err = runTreeID(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "c4-operator "+cmd+": "+err.Error())
		os.Exit(1)
	}
}

func runTreeID(args []string) error {
	fs := flag.NewFlagSet("tree-id", flag.ExitOnError)
	expect := fs.String("expect", "", "fail unless the tree has this ID")
	hold := fs.Duration("hold", 0, "keep running this long after printing (for demos)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: c4-operator tree-id [--expect ID] [--hold DURATION] DIR")
	}
	id, err := view.ScanID(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Println(id)
	if *expect != "" && id.String() != *expect {
		return fmt.Errorf("tree is %s, expected %s", id, *expect)
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	return nil
}
