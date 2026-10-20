package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// docs/agents/tools.md is what c4-operator mcp --tools-md prints, but for
// the padding of its tables (commits align markdown tables): the
// reference lists every tool the server has.
func TestToolsDocIsCurrent(t *testing.T) {
	var b bytes.Buffer
	if err := toolsMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/agents/tools.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unpadTables(doc), unpadTables(b.Bytes())) {
		t.Fatal("docs/agents/tools.md is stale: run c4-operator mcp --tools-md > docs/agents/tools.md")
	}
}

var (
	cellPad = regexp.MustCompile(` *\| *`)
	rule    = regexp.MustCompile(`-{3,}`)
)

// unpadTables drops the padding around table cells and the length of
// separator rules.
func unpadTables(md []byte) []byte {
	lines := bytes.Split(md, []byte("\n"))
	for i, l := range lines {
		if bytes.HasPrefix(l, []byte("|")) {
			lines[i] = rule.ReplaceAll(cellPad.ReplaceAll(bytes.TrimRight(l, " "), []byte("|")), []byte("---"))
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// Over HTTP every request acts as its own caller: one with no bearer token
// is refused, never served under a token the server was started with.
func TestMCPOverHTTPNeedsABearer(t *testing.T) {
	var seen []string
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	defer hud.Close()
	srv := httptest.NewServer(mcpHTTPHandler(hud.URL, "", "an-agent"))
	defer srv.Close()
	call := func(auth string) int {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"c4_farm_status","arguments":{}}}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := call(""); code != http.StatusUnauthorized {
		t.Errorf("a request with no bearer got %d, want 401 (the HUD saw %q)", code, seen)
	}
	for _, a := range seen {
		if strings.Contains(a, "the-servers-own-token") {
			t.Errorf("a request reached the HUD under the server's own token")
		}
	}
	if code := call("Basic eDp5"); code != http.StatusUnauthorized {
		t.Errorf("a request with a non-bearer Authorization got %d, want 401", code)
	}
	if code := call("Bearer callers-token"); code == http.StatusUnauthorized {
		t.Errorf("a request with its own bearer was refused")
	}
}
