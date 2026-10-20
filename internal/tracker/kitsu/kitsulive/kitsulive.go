// Package kitsulive drives a real Kitsu site as its people do, for the
// adapter's live check (kitsu_live_test.go) and the live demo: it seeds a
// show with an admin's sign-in (a production, a sequence, a shot and its
// lighting task, a supervisor, an artist, and c4-sync's bot), and posts
// comments as the supervisor. Use it only on a throwaway local server
// (CGWire's cgwire/cgwire image): everything it makes is real.
package kitsulive

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

// Session is one person's sign-in.
type Session struct {
	API   string // the site's /api
	Token string
	HC    *http.Client
}

// Login signs a person in with their email and password.
func Login(ctx context.Context, site, email, password string) (*Session, error) {
	s := &Session{API: strings.TrimSuffix(site, "/") + "/api", HC: &http.Client{Timeout: 30 * time.Second}}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.Do(ctx, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password}, &out); err != nil {
		return nil, err
	}
	s.Token = out.AccessToken
	return s, nil
}

// Do calls the API with JSON.
func (s *Session) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kitsu %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Person is someone the seed made.
type Person struct {
	ID, Name, Email, Password string
}

// Show is what Seed made.
type Show struct {
	Project, ProjectName, Sequence, Shot, Task string
	Supervisor, Artist                         Person
	// Bot is c4-sync's: a manager in the production's team, with its API
	// token, shown once.
	Bot      Person
	BotToken string
}

type idOnly struct {
	ID string `json:"id"`
}

// Seed makes a new production named name (a short: sequences and shots)
// with sequence sq010, shot sh040 and its lighting task, ready, due on a
// date (YYYY-MM-DD) and bid at minutes; a supervisor, Dana Reyes; the
// artist assigned to it, Mo Okafor; and c4-sync's bot. Names and emails
// carry tag, so seeds do not collide on one server.
func Seed(ctx context.Context, admin *Session, name, tag, due string, minutes float64) (Show, error) {
	sh := Show{ProjectName: name}
	var open []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := admin.Do(ctx, http.MethodGet, "/data/project-status", nil, &open); err != nil {
		return sh, err
	}
	statusID := ""
	for _, s := range open {
		if s.Name == "Open" {
			statusID = s.ID
		}
	}
	var p idOnly
	if err := admin.Do(ctx, http.MethodPost, "/data/projects", map[string]any{"name": name, "production_type": "short",
		"project_status_id": statusID}, &p); err != nil {
		return sh, fmt.Errorf("making the production: %w", err)
	}
	sh.Project = p.ID
	pp := "/data/projects/" + p.ID
	var types []idOnly
	if err := admin.Do(ctx, http.MethodGet, "/data/task-types?name=Lighting", nil, &types); err != nil || len(types) == 0 {
		return sh, fmt.Errorf("the Lighting task type: %v (%d)", err, len(types))
	}
	if err := admin.Do(ctx, http.MethodPost, pp+"/settings/task-types", map[string]string{"task_type_id": types[0].ID}, nil); err != nil {
		return sh, err
	}
	var statuses []struct {
		ID        string `json:"id"`
		ShortName string `json:"short_name"`
	}
	if err := admin.Do(ctx, http.MethodGet, "/data/task-status", nil, &statuses); err != nil {
		return sh, err
	}
	ready := ""
	for _, s := range statuses {
		switch s.ShortName {
		case "todo", "wip", "wfa", "retake", "done", "ready":
			if err := admin.Do(ctx, http.MethodPost, pp+"/settings/task-status", map[string]string{"task_status_id": s.ID}, nil); err != nil {
				return sh, err
			}
		}
		if s.ShortName == "ready" {
			ready = s.ID
		}
	}
	var seq, shot, task idOnly
	if err := admin.Do(ctx, http.MethodPost, pp+"/sequences", map[string]string{"name": "sq010"}, &seq); err != nil {
		return sh, err
	}
	if err := admin.Do(ctx, http.MethodPost, pp+"/shots", map[string]any{"name": "sh040", "sequence_id": seq.ID, "nb_frames": 240}, &shot); err != nil {
		return sh, err
	}
	sh.Sequence, sh.Shot = seq.ID, shot.ID
	person := func(first, last, role string, bot bool) (Person, string, error) {
		email := fmt.Sprintf("%s+%s@example.com", strings.ToLower(first), tag)
		in := map[string]any{"first_name": first, "last_name": last, "email": email, "role": role}
		pw := ""
		if bot {
			in["is_bot"] = true
		} else {
			pw = "pw-" + tag + "-" + strings.ToLower(first)
			in["password"] = pw
		}
		var out struct {
			ID          string `json:"id"`
			AccessToken string `json:"access_token"`
		}
		if err := admin.Do(ctx, http.MethodPost, "/data/persons", in, &out); err != nil {
			return Person{}, "", err
		}
		if err := admin.Do(ctx, http.MethodPost, pp+"/team", map[string]string{"person_id": out.ID}, nil); err != nil {
			return Person{}, "", err
		}
		return Person{ID: out.ID, Name: first + " " + last, Email: email, Password: pw}, out.AccessToken, nil
	}
	var err error
	if sh.Supervisor, _, err = person("Dana", "Reyes", "supervisor", false); err != nil {
		return sh, err
	}
	if sh.Artist, _, err = person("Mo", "Okafor", "user", false); err != nil {
		return sh, err
	}
	if sh.Bot, sh.BotToken, err = person("C4", "Sync", "manager", true); err != nil {
		return sh, err
	}
	if err := admin.Do(ctx, http.MethodPost, "/data/tasks", map[string]any{"name": "main", "project_id": p.ID, "entity_id": shot.ID,
		"task_type_id": types[0].ID, "task_status_id": ready, "due_date": due, "estimation": minutes}, &task); err != nil {
		return sh, err
	}
	sh.Task = task.ID
	if err := admin.Do(ctx, http.MethodPut, "/actions/tasks/"+task.ID+"/assign", map[string]string{"person_id": sh.Artist.ID}, nil); err != nil {
		return sh, err
	}
	return sh, nil
}

// Comment posts a comment as the session's person, moving the task to a
// status by short name ("" keeps it).
func Comment(ctx context.Context, s *Session, task, status, text string) (string, error) {
	var t struct {
		TaskStatusID string `json:"task_status_id"`
	}
	if err := s.Do(ctx, http.MethodGet, "/data/tasks/"+task, nil, &t); err != nil {
		return "", err
	}
	st := t.TaskStatusID
	if status != "" {
		var ss []idOnly
		if err := s.Do(ctx, http.MethodGet, "/data/task-status?short_name="+url.QueryEscape(status), nil, &ss); err != nil || len(ss) == 0 {
			return "", fmt.Errorf("status %s: %v", status, err)
		}
		st = ss[0].ID
	}
	var c idOnly
	err := s.Do(ctx, http.MethodPost, "/actions/tasks/"+task+"/comment", map[string]string{"task_status_id": st, "comment": text}, &c)
	return c.ID, err
}
