package farmcli

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DiagnoseArgs are `c4 farm diagnose`'s.
type DiagnoseArgs struct {
	// Window keeps the snapshots within it of the latest (a Go duration);
	// empty is everything the farm's ring keeps.
	Window string
	// Out is where the bundle is written (default its own file name, in
	// this folder).
	Out string
	// Show prints the bundle's README.txt and every entry's fields, in
	// plain words, and writes nothing.
	Show bool
}

// bundleName is a bundle's file name as the farm gives it: no farm name,
// no date, no folder.
var bundleName = regexp.MustCompile(`^c4-diagnose-[0-9A-Za-z]{1,64}\.tgz$`)

// Diagnose is `c4 farm diagnose`: the farm's diagnostic bundle, a redacted
// file of its scheduling decisions for a bug report, holding no names,
// paths or contents. It is the farm admin's (or a single-person farm's
// owner's) to make. --show says what it holds before anything is written;
// otherwise it is saved whole, under the name the farm gives it.
func Diagnose(ctx context.Context, c *Client, o Out, a DiagnoseArgs) error {
	path := "/diagnose"
	if a.Window != "" {
		if _, err := time.ParseDuration(a.Window); err != nil {
			return fmt.Errorf("--window %q is not a duration (30m, 2h)", a.Window)
		}
		path += "?window=" + url.QueryEscape(a.Window)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := c.request(cctx, http.MethodGet, path, nil, -1, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// the farm's own words; a failure carries no bundle bytes
		return apiError(resp)
	}
	name := "c4-diagnose.tgz"
	if _, p, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && bundleName.MatchString(filepath.Base(p["filename"])) {
		name = filepath.Base(p["filename"])
	}
	id, tier := resp.Header.Get("X-C4-Bundle"), resp.Header.Get("X-C4-Bundle-Tier")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		return fmt.Errorf("reading the bundle: %w", err)
	}
	if a.Show {
		return showBundle(o, data, id, tier)
	}
	out := a.Out
	if out == "" {
		out = name
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s is there already; give --out FILE, or move it", out)
	}
	if err := writeAtomic(out, data); err != nil {
		return err
	}
	o.printf("bundle    %s · %s · %s\n", out, tierWords(tier), id)
	o.printf("          it holds no names, paths or contents; c4 farm diagnose --show prints what it does hold, before anything is written\n")
	return nil
}

// tierWords is what a bundle's tier means.
func tierWords(tier string) string {
	switch tier {
	case "ring":
		return "the farm's recent decisions"
	case "present":
		return "the present moment only (the farm keeps no ring of past decisions)"
	}
	return "tier " + cmp.Or(tier, "unknown")
}

// showBundle prints a bundle's README.txt whole, then each entry: its
// size and, for JSON, the fields it holds.
func showBundle(o Out, data []byte, id, tier string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return errors.New("the farm's bundle is not a gzipped tar")
	}
	tr := tar.NewReader(zr)
	o.printf("bundle    %s · %s (nothing written)\n\n", cmp.Or(id, "no ID given"), tierWords(tier))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the bundle: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		if err != nil {
			return err
		}
		if h.Name == "README.txt" {
			o.printf("%s\n", strings.TrimRight(string(body), "\n"))
			o.printf("\n")
			continue
		}
		o.printf("%-22s %s", h.Name, byteSize(int64(len(body))))
		if fields := jsonFields(h.Name, body); len(fields) > 0 {
			o.printf("  fields: %s", strings.Join(fields, ", "))
		}
		o.printf("\n")
	}
}

// jsonFields are the keys a JSON entry holds (a JSONL entry's first line).
func jsonFields(name string, body []byte) []string {
	if strings.HasSuffix(name, ".jsonl") {
		body, _, _ = bytes.Cut(body, []byte("\n"))
	} else if !strings.HasSuffix(name, ".json") {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
