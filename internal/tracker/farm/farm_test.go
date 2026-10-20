package farm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
)

type hud struct {
	*httptest.Server
	names   map[string]bool
	objects map[string][]byte
	decided []map[string]any
	nudges  []map[string]string
	agent   []string
	submits []api.SubmitRequest
}

// newHUD answers the HUD API routes c4-sync calls (internal/hud/API.md):
// 201 then 409 for a taken name, 404 for an unknown job, 409 from decide
// when nothing open waits.
func newHUD(t *testing.T) *hud {
	h := &hud{names: map[string]bool{}, objects: map[string][]byte{}}
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sync-token" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h.agent = append(h.agent, r.Header.Get("X-C4-Agent"))
			next(w, r)
		}
	}
	mux.HandleFunc("POST /api/v1/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		var in api.SubmitRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		var spec c4v1.WorkJobSpec
		if err := json.Unmarshal(in.Spec, &spec); err != nil || spec.DeadlineMode != "Tracking" {
			http.Error(w, `{"error":"bad spec"}`, http.StatusBadRequest)
			return
		}
		if h.names[in.Name] {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"name taken"}`))
			return
		}
		h.names[in.Name] = true
		h.submits = append(h.submits, in)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.SubmitResponse{OK: true, Namespace: in.Namespace, Name: in.Name})
	}))
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		if !h.names[r.PathValue("name")] {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"namespace": r.PathValue("ns"), "name": r.PathValue("name"),
			"phase": "Running", "project": "proj", "unitList": []any{}})
	}))
	mux.HandleFunc("PUT /api/v1/objects/{project}/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if c4.Identify(bytes.NewReader(b)).String() != r.PathValue("id") {
			http.Error(w, `{"error":"id mismatch"}`, http.StatusBadRequest)
			return
		}
		h.objects[r.PathValue("id")] = b
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/decide", auth(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["unit"] != "review" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"nothing open waits for a decision as unit"}`))
			return
		}
		h.decided = append(h.decided, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/nudge", auth(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		h.nudges = append(h.nudges, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)
	return h
}

func client(h *hud) *farm.HUD {
	return &farm.HUD{Base: h.URL, Agent: "c4-sync", Token: func(context.Context) (string, error) { return "sync-token", nil }}
}

func TestSubmitIsIdempotent(t *testing.T) {
	h := newHUD(t)
	f := client(h)
	due := metav1.NewTime(time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC))
	req := farm.JobRequest{Namespace: "work", Name: "gh-7-v002", Spec: c4v1.WorkJobSpec{Class: "agent", Deadline: &due, DeadlineMode: "Tracking"},
		TrackerRef: "gh/Issue/7", Follows: "gh-7"}
	for i := 0; i < 2; i++ {
		ref, err := f.Submit(context.Background(), req)
		if err != nil || ref != (farm.JobRef{Namespace: "work", Name: "gh-7-v002"}) {
			t.Fatalf("submit %d: %v %v", i, ref, err)
		}
	}
	if h.agent[0] != "c4-sync" {
		t.Fatalf("X-C4-Agent: %q", h.agent[0])
	}
	// The job carries the entity it was made for and the version it follows.
	if s := h.submits[0]; s.TrackerRef != "gh/Issue/7" || s.Follows != "gh-7" || s.Type != "tracker" {
		t.Fatalf("submitted: %+v", s)
	}
}

func TestJobNotFound(t *testing.T) {
	f := client(newHUD(t))
	if _, err := f.Job(context.Background(), farm.JobRef{Namespace: "work", Name: "nope"}); !errors.Is(err, farm.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDecideStoresEvidenceAndRelays(t *testing.T) {
	h := newHUD(t)
	f := client(h)
	ev := []byte(`{"review":{"id":5,"state":"CHANGES_REQUESTED"}}`)
	err := f.Decide(context.Background(), farm.JobRef{Namespace: "work", Name: "gh-7"}, farm.Relay{
		Project: "proj", Unit: "review", Do: "rework", Note: "assert the order",
		Tracker: "github", Source: "Review/12#5", By: "@dana", Person: "dana", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	id := c4.Identify(bytes.NewReader(ev)).String()
	if !bytes.Equal(h.objects[id], ev) {
		t.Fatal("evidence not stored under its C4 ID")
	}
	note, _ := h.decided[0]["note"].(string)
	if h.decided[0]["do"] != "rework" || !strings.HasPrefix(note, "assert the order") || !strings.Contains(note, "Relayed from github Review/12#5 by @dana (dana); evidence "+id) {
		t.Fatalf("decide body: %v", h.decided[0])
	}
	err = f.Decide(context.Background(), farm.JobRef{Namespace: "work", Name: "gh-7"}, farm.Relay{Unit: "other", Do: "approve"})
	if !errors.Is(err, farm.ErrNotOpen) {
		t.Fatalf("want ErrNotOpen, got %v", err)
	}
}

func TestSetDeadlineSendsTheDeadlineAlone(t *testing.T) {
	h := newHUD(t)
	at := time.Date(2026, 11, 3, 1, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	if err := client(h).SetDeadline(context.Background(), farm.JobRef{Namespace: "work", Name: "gh-7"}, at); err != nil {
		t.Fatal(err)
	}
	if len(h.nudges[0]) != 1 || h.nudges[0]["deadline"] != "2026-11-03T08:00:00Z" {
		t.Fatalf("nudge: %v", h.nudges[0])
	}
}

func TestDevViewerAndPublicURL(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"namespace":"work","name":"j","phase":"Running"}`))
	}))
	defer srv.Close()
	h := &farm.HUD{Base: srv.URL, As: "c4-sync", Public: "https://c4.example.com/"}
	if _, err := h.Job(context.Background(), farm.JobRef{Namespace: "work", Name: "j"}); err != nil || got != "as=c4-sync" {
		t.Fatalf("query %q, %v", got, err)
	}
	if u := h.JobURL(farm.JobRef{Namespace: "work", Name: "j"}); u != "https://c4.example.com/#/job/work/j" {
		t.Fatalf("job URL %q", u)
	}
}

// A wait that asks for a reply gets the relayed note as its content
// (API.md, decide: reply {text}); one that does not, gets none.
func TestDecideSendsTheNoteAsAReply(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	h := &farm.HUD{Base: srv.URL}
	j := farm.JobRef{Namespace: "work", Name: "j"}
	if err := h.Decide(context.Background(), j, farm.Relay{Unit: "review", Do: "rework", Note: "assert the order", Reply: true, Tracker: "github"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(context.Background(), j, farm.Relay{Unit: "review", Do: "approve", Note: "ok", Tracker: "github"}); err != nil {
		t.Fatal(err)
	}
	reply, _ := bodies[0]["reply"].(map[string]any)
	if text, _ := reply["text"].(string); !strings.HasPrefix(text, "assert the order") {
		t.Fatalf("reply: %v", bodies[0])
	}
	if _, ok := bodies[1]["reply"]; ok {
		t.Fatalf("a wait that asks for no reply got one: %v", bodies[1])
	}
}
