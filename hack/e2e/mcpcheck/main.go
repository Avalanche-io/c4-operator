// Command mcpcheck drives the farm's MCP server the way an agent would: it
// starts `c4-operator mcp` over stdio, lists the tools, and makes the calls
// named on its command line, printing each result as one JSON line. The
// P6 e2e uses it against the live HUD.
//
//	mcpcheck --server "bin/c4-operator mcp --hud http://127.0.0.1:31080 --as wrangler" \
//	    'c4_farm_status {}' 'c4_list_jobs {"at_risk_only": true}'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := flag.String("server", "", "the MCP server command line")
	flag.Parse()
	if *server == "" {
		fmt.Fprintln(os.Stderr, "usage: mcpcheck --server 'cmd args' 'tool {json}'...")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	argv := strings.Fields(*server)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpcheck", Version: "v1"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list tools:", err)
		os.Exit(1)
	}
	var names []string
	for _, t := range tools.Tools {
		names = append(names, t.Name)
	}
	line(map[string]any{"tools": names, "server": cs.InitializeResult().ServerInfo.Name, "protocol": cs.InitializeResult().ProtocolVersion})
	failed := false
	for _, c := range flag.Args() {
		name, args, _ := strings.Cut(c, " ")
		var in map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &in); err != nil {
			fmt.Fprintf(os.Stderr, "%s: arguments: %v\n", name, err)
			os.Exit(2)
		}
		start := time.Now()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			os.Exit(1)
		}
		out := map[string]any{"tool": name, "isError": res.IsError, "ms": time.Since(start).Milliseconds()}
		if res.StructuredContent != nil {
			out["result"] = res.StructuredContent
		} else {
			var text []string
			for _, ct := range res.Content {
				if tc, ok := ct.(*mcp.TextContent); ok {
					text = append(text, tc.Text)
				}
			}
			out["text"] = strings.Join(text, "\n")
		}
		failed = failed || res.IsError
		line(out)
	}
	if failed {
		os.Exit(3)
	}
}

func line(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
