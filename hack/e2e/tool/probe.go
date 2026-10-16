package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// probe makes one request to a cache tier the way an attacker inside a
// pod would, and prints the status and the first line of the body. It
// always exits 0 when it got an answer: the e2e decides what the answer
// means.
//
//	c4e2e probe --url http://10.0.0.5:7444/c4... --project iso-a [--token /var/run/tier/token]
func probe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	url := fs.String("url", "", "tier URL, ending in the object ID")
	project := fs.String("project", "", "X-C4-Project header (empty: none)")
	tokenFile := fs.String("token", "", "file holding a bearer token (empty: none)")
	method := fs.String("method", http.MethodGet, "GET or HEAD")
	fs.Parse(args)
	req, err := http.NewRequest(*method, *url, nil)
	if err != nil {
		return err
	}
	if *project != "" {
		req.Header.Set("X-C4-Project", *project)
	}
	if *tokenFile != "" {
		t, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(t)))
	}
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	line, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	fmt.Printf("%d %s\n", resp.StatusCode, line)
	return nil
}
