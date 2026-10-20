// Command kitsuctl does, on a real Kitsu, what the Kitsu demo's people do
// (hack/demo/kitsu.sh), through internal/tracker/kitsu/kitsulive. Use it
// only on a throwaway local server.
//
//	kitsuctl seed    --site URL --admin EMAIL:PASSWORD --due YYYY-MM-DD
//	kitsuctl comment --site URL --as EMAIL:PASSWORD --task ID [--status SHORT] --text TEXT
//	kitsuctl state   --site URL --admin EMAIL:PASSWORD --task ID
//
// seed prints the show it made as JSON; state prints the task's status,
// its comments oldest first, and its previews.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu/kitsulive"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kitsuctl:", err)
		os.Exit(1)
	}
}

func login(ctx context.Context, site, cred string) (*kitsulive.Session, error) {
	email, pw, ok := strings.Cut(cred, ":")
	if !ok {
		return nil, errors.New("credentials are EMAIL:PASSWORD")
	}
	return kitsulive.Login(ctx, site, email, pw)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("seed, comment or state")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	site := fs.String("site", "http://127.0.0.1:8091", "the Kitsu site")
	admin := fs.String("admin", "", "the admin's EMAIL:PASSWORD")
	as := fs.String("as", "", "the commenter's EMAIL:PASSWORD")
	due := fs.String("due", "", "the task's due date")
	task := fs.String("task", "", "the task's id")
	status := fs.String("status", "", "the status's short name")
	text := fs.String("text", "", "the comment")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out := json.NewEncoder(os.Stdout)
	switch args[0] {
	case "seed":
		s, err := login(ctx, *site, *admin)
		if err != nil {
			return err
		}
		tag := fmt.Sprint(time.Now().Unix())
		sh, err := kitsulive.Seed(ctx, s, "C4 demo "+tag, tag, *due, 960)
		if err != nil {
			return err
		}
		return out.Encode(sh)
	case "comment":
		s, err := login(ctx, *site, *as)
		if err != nil {
			return err
		}
		id, err := kitsulive.Comment(ctx, s, *task, *status, *text)
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"comment": id})
	case "state":
		s, err := login(ctx, *site, *admin)
		if err != nil {
			return err
		}
		return state(ctx, s, *task, out)
	}
	return fmt.Errorf("no command %q", args[0])
}

func state(ctx context.Context, s *kitsulive.Session, task string, out *json.Encoder) error {
	var t struct {
		TaskStatusID string         `json:"task_status_id"`
		Data         map[string]any `json:"data"`
	}
	if err := s.Do(ctx, "GET", "/data/tasks/"+task, nil, &t); err != nil {
		return err
	}
	var raw []map[string]any
	if err := s.Do(ctx, "GET", "/data/task-status", nil, &raw); err != nil {
		return err
	}
	short := map[string]string{}
	for _, r := range raw {
		short[fmt.Sprint(r["id"])] = fmt.Sprint(r["short_name"])
	}
	var cs []struct {
		Text         string `json:"text"`
		CreatedAt    string `json:"created_at"`
		TaskStatusID string `json:"task_status_id"`
		Person       struct {
			FullName string `json:"full_name"`
		} `json:"person"`
		Previews []struct {
			Revision int    `json:"revision"`
			Status   string `json:"status"`
		} `json:"previews"`
		Mentions []string `json:"mentions"`
	}
	if err := s.Do(ctx, "GET", "/data/tasks/"+task+"/comments", nil, &cs); err != nil {
		return err
	}
	// Zou sends them newest first: oldest first, ties kept in order.
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].CreatedAt < cs[j].CreatedAt })
	type comment struct {
		By, Status, Text, At string
		Previews             []string
		Mentions             int
	}
	res := struct {
		Status   string
		Data     map[string]any
		Comments []comment
	}{Status: short[t.TaskStatusID], Data: t.Data}
	for _, c := range cs {
		cm := comment{By: c.Person.FullName, Status: short[c.TaskStatusID], Text: c.Text, At: c.CreatedAt, Mentions: len(c.Mentions)}
		for _, p := range c.Previews {
			cm.Previews = append(cm.Previews, fmt.Sprintf("r%d:%s", p.Revision, p.Status))
		}
		res.Comments = append(res.Comments, cm)
	}
	return out.Encode(res)
}
