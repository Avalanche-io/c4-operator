// Command c4-sandbox-github is the local farm's stand-in GitHub (c4 farm
// up --tracker; internal/sandbox): the GitHub REST API c4-sync speaks, from
// GitHub's published shapes, and a page at /sandbox/ where a newcomer acts
// as the farm's people. It keeps everything in memory and reaches nothing
// outside the farm.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/sandbox"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "c4-sandbox-github:", err)
		os.Exit(2)
	}
}

func parse(args []string, stderr io.Writer) (sandbox.Config, string, error) {
	fs := flag.NewFlagSet("c4-sandbox-github", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c sandbox.Config
	listen := fs.String("listen", ":8080", "address to serve on")
	repo := fs.String("repo", "petrel-finch/saltmarsh", "the repository, owner/name")
	people := fs.String("people", "", "who a person may act as: login=person,... (the local farm's people)")
	fs.StringVar(&c.Token, "token", os.Getenv("C4_SANDBOX_TOKEN"), "the only token the API takes, c4-sync's (C4_SANDBOX_TOKEN)")
	fs.StringVar(&c.URL, "url", "http://c4-sandbox-github.c4-system.svc:8080", "where c4-sync reaches the API")
	fs.StringVar(&c.Hook, "hook", "", "where deliveries go: c4-sync's /webhooks/{namespace}/{tracker} (empty: none; c4-sync polls)")
	fs.StringVar(&c.HookSecret, "hook-secret", os.Getenv("C4_SANDBOX_HOOK_SECRET"), "the secret deliveries are signed with (C4_SANDBOX_HOOK_SECRET)")
	fs.StringVar(&c.HUD, "hud", "http://c4-hud.c4-system.svc", "the HUD, as the sandbox reaches it")
	fs.StringVar(&c.HUDAs, "hud-as", "wrangler", "the HUD viewer the sandbox reads jobs as (a development farm's ?as=)")
	fs.StringVar(&c.Project, "project", "saltmarsh", "the project the Tracker is in")
	fs.StringVar(&c.Namespace, "namespace", "", "the Tracker's namespace (default the project)")
	fs.StringVar(&c.Tracker, "tracker", "sandbox", "the Tracker's name")
	fs.StringVar(&c.Agent, "agent", "c4-agent", "the login the agent's part is played as")
	if err := fs.Parse(args); err != nil {
		return c, "", err
	}
	var ok bool
	if c.Owner, c.Repo, ok = strings.Cut(*repo, "/"); !ok || c.Owner == "" || c.Repo == "" {
		return c, "", fmt.Errorf("--repo %q: want owner/name", *repo)
	}
	if c.Namespace == "" {
		c.Namespace = c.Project
	}
	for _, p := range strings.Split(*people, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		login, person, ok := strings.Cut(p, "=")
		if !ok || login == "" || person == "" {
			return c, "", fmt.Errorf("--people %q: want login=person,...", p)
		}
		c.People = append(c.People, sandbox.Person{Login: login, Person: person, Name: person})
	}
	if len(c.People) == 0 {
		return c, "", errors.New("--people is required: who a person may act as")
	}
	if c.Token == "" {
		return c, "", errors.New("a token is required (--token or C4_SANDBOX_TOKEN)")
	}
	return c, *listen, nil
}

func run(args []string, stderr io.Writer) error {
	c, listen, err := parse(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: sandbox.New(c).Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("c4-sandbox-github on %s: %s/%s for tracker %s/%s", listen, c.Owner, c.Repo, c.Namespace, c.Tracker)
	return srv.ListenAndServe()
}
