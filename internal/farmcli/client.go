package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one farm's HUD API as the user.
type Client struct {
	Base  string
	As    string
	Token string
	// Installer is a local farm's installer token, sent as X-C4-Installer:
	// its HUD takes a development viewer (As) only with it.
	Installer string
	HTTP      *http.Client
}

// NewClient makes a client for a farm.
func NewClient(f Farm) (*Client, error) {
	tok, err := f.Token()
	if err != nil {
		return nil, err
	}
	inst, err := f.InstallerToken()
	if err != nil {
		return nil, fmt.Errorf("the local farm's installer token: %w (c4 farm up makes it again)", err)
	}
	return &Client{Base: strings.TrimSuffix(f.HUD, "/"), As: f.As, Token: tok, Installer: inst,
		HTTP: &http.Client{Timeout: 0}}, nil
}

// APIError is an error answer from the HUD.
type APIError struct {
	Status int
	Msg    string
	Body   []byte
}

func (e *APIError) Error() string { return e.Msg }

func (c *Client) url(path string) string {
	u := c.Base + "/api/v1" + path
	if c.As != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "as=" + url.QueryEscape(c.As)
	}
	return u
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, size int64, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-C4-Client", "c4-farm")
	if c.Installer != "" {
		req.Header.Set("X-C4-Installer", c.Installer)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the farm at %s is unreachable: %v", c.Base, err)
	}
	return resp, nil
}

func apiError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		msg = "not signed in: " + msg + " (run `c4 farm login`)"
	case http.StatusForbidden:
		msg = "not allowed: " + msg
	}
	return &APIError{Status: resp.StatusCode, Msg: msg, Body: data}
}

// Do sends a JSON request and decodes a JSON answer into out. Answers
// whose status is in also are decoded too, and returned with an APIError.
func (c *Client) Do(ctx context.Context, method, path string, in, out any, also ...int) error {
	var body io.Reader
	size := int64(-1)
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, size = bytes.NewReader(b), int64(len(b))
	}
	ctype := ""
	if in != nil {
		ctype = "application/json"
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := c.request(cctx, method, path, body, size, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		for _, s := range also {
			if resp.StatusCode == s && out != nil {
				data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
				json.Unmarshal(data, out)
				return &APIError{Status: s, Msg: http.StatusText(s), Body: data}
			}
		}
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Put streams size bytes to path.
func (c *Client) Put(ctx context.Context, path string, r io.Reader, size int64) error {
	resp, err := c.request(ctx, http.MethodPut, path, r, size, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// Get opens path's body.
func (c *Client) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, path, nil, -1, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp.Body, nil
}

// Web is the HUD's address for a route, as the user would open it.
func (c *Client) Web(route string, query url.Values) string {
	if query == nil {
		query = url.Values{}
	}
	if c.As != "" {
		query.Set("as", c.As)
	}
	u := c.Base + "/"
	if q := query.Encode(); q != "" {
		u += "?" + q
	}
	if route != "" {
		u += "#/" + strings.TrimPrefix(route, "#/")
	}
	return u
}
