// Command c4-operator is every component of the operator in one binary.
//
//	c4-operator manager    controllers and the pod webhook (Deployment)
//	c4-operator node       CSI node plugin, node cache, peer tier (DaemonSet)
//	c4-operator regional   regional cache tier (Deployment)
//	c4-operator ingest     hash a directory into a project's store
//	c4-operator tree-id    view ID of a directory
//	c4-operator mcp        the farm's MCP server, over stdio or Streamable HTTP
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

const usage = `usage: c4-operator <command> [flags]

commands:
  manager    run the controllers and the pod webhook
  node       run the node daemon (CSI node plugin, cache, peer tier)
  regional   run a regional cache replica
  ingest     hash a directory into a project's store and print its view ID
  tree-id    print the view ID of a directory
  mcp        serve the farm's tools to agents (MCP, stdio or HTTP)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	commands := map[string]func([]string) error{
		"manager":  runManager,
		"node":     runNode,
		"regional": runRegional,
		"ingest":   runIngest,
		"tree-id":  runTreeID,
		"mcp":      runMCP,
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "c4-operator "+cmd+": "+err.Error())
		os.Exit(1)
	}
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := c4v1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
