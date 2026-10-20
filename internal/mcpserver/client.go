// Package mcpserver is the farm's MCP server (DESIGN.md section 13): every
// HUD capability as a tool an agent can call. It is a client of the HUD
// API, so an agent sees and changes exactly what its user could in the
// HUD: the same viewer filtering, the same authorization, the same audit
// Events.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls the HUD API as one identity.
type Client struct {
	// Base is the HUD's address, for example http://c4-hud.c4-system.
	Base string
	// Token is the caller's bearer token; empty sends none.
	Token string
	// As selects a development viewer (?as=) on HUDs started with
	// --hud-dev-viewers. Never used in production.
	As string
	// Agent names the agent using this client; the HUD books its
	// controls as "agent/NAME for USER" (X-C4-Agent).
	Agent string
	HTTP  *http.Client
}

// APIError is a HUD error, phrased for an agent to act on.
type APIError struct {
	Status int
	Msg    string
	// Body is the answer as sent, for answers that carry more than an
	// error (a refused submission's errors).
	Body []byte
}

func (e *APIError) Error() string { return e.Msg }

func (c *Client) url(path string) string {
	u := strings.TrimSuffix(c.Base, "/") + "/api/v1" + path
	if c.As != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "as=" + url.QueryEscape(c.As)
	}
	return u
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	// The HUD notes the client in its signed control records.
	req.Header.Set("X-C4-Client", "mcp")
	if c.Agent != "" {
		req.Header.Set("X-C4-Agent", c.Agent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("the HUD at %s is unreachable: %v", c.Base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Msg: advise(resp.StatusCode, path, e.Error), Body: data}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// advise turns an HTTP failure into a next step.
func advise(status int, path, msg string) string {
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusNotFound:
		if strings.HasPrefix(path, "/jobs/") {
			return msg + ". Job names are namespace/name; call c4_list_jobs to see the jobs you can see."
		}
		return msg + "."
	case http.StatusUnauthorized:
		return msg + ". The MCP server needs a bearer token for the HUD (--token-file), or must be reached through the SSO proxy."
	case http.StatusForbidden:
		return msg + ". Your Kubernetes permissions do not allow this change; ask someone who may patch it."
	case http.StatusConflict:
		return msg + ". The object changed while you were editing it; read it again and retry."
	case http.StatusBadRequest:
		return msg + "."
	}
	return fmt.Sprintf("the HUD answered %d: %s", status, msg)
}

func remarshal(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asMaps(v any) []map[string]any {
	out := []map[string]any{}
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}
