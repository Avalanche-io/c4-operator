// Package agentenv holds the agent environments the release ships: the
// clean config directory trees a member or block runs in, in place of a
// person's own ~/.claude (internal/agent, env.go). base is the farm's
// default.
package agentenv

import (
	"embed"
	"io/fs"
)

//go:embed all:base
var trees embed.FS

// Base is the farm's default agent environment.
func Base() fs.FS {
	sub, err := fs.Sub(trees, "base")
	if err != nil {
		panic(err) // embedded above
	}
	return sub
}
