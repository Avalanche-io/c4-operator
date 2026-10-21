package main

import (
	"bytes"
	"strings"
	"testing"
)

// Every command in the usage parses its flags: a flag a command defines
// twice (its own and one for every command) panics, and only running the
// command shows it.
func TestEveryCommandParsesItsFlags(t *testing.T) {
	for _, line := range strings.Split(usage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		var words []string
		for _, w := range strings.Fields(line) {
			if w == strings.ToLower(w) && !strings.ContainsAny(w, "[]|-.") {
				words = append(words, w)
				continue
			}
			break
		}
		if len(words) == 0 || len(words) > 2 {
			continue
		}
		args := append(words, "-h")
		t.Run(strings.Join(words, " "), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("c4 farm %s: %v", strings.Join(args, " "), r)
				}
			}()
			var out bytes.Buffer
			run(args, &out, strings.NewReader(""))
		})
	}
}
