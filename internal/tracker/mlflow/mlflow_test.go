package mlflow_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/mlflow"
	"github.com/Avalanche-io/c4-operator/internal/tracker/mlflow/mlflowtest"
)

func server(t *testing.T) *mlflowtest.Server {
	s := mlflowtest.New("denoiser")
	t.Cleanup(s.Close)
	return s
}

func adapter(t *testing.T, s *mlflowtest.Server, pw string, secret []byte) *mlflow.Adapter {
	t.Helper()
	c := tracker.Config{Endpoint: s.URL, Scope: map[string]string{"experiment": "7", "model": "denoiser", "user": s.User},
		Token: func(context.Context) (string, error) { return pw, nil }}
	if secret != nil {
		c.WebhookSecret = func(context.Context) ([]byte, error) { return secret, nil }
	}
	a, err := mlflow.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConfigRequired(t *testing.T) {
	if _, err := mlflow.New(tracker.Config{Endpoint: "http://x", Scope: map[string]string{"experiment": "7"}}); err == nil {
		t.Fatal("model is required")
	}
	if _, err := mlflow.New(tracker.Config{Scope: map[string]string{"experiment": "7", "model": "m"}}); err == nil {
		t.Fatal("endpoint is required")
	}
}

func TestWrongPassword(t *testing.T) {
	s := server(t)
	if _, err := adapter(t, s, "wrong", nil).Get(context.Background(), tracker.Ref{Type: "Run", ID: "x"}); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401, got %v", err)
	}
}

func TestListByStatusAndTag(t *testing.T) {
	s := server(t)
	due := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	want := s.AddRun("7", "a", "SCHEDULED", map[string]string{"c4.intake": "true", "c4.deadline": due.Format(time.RFC3339)})
	s.AddRun("7", "b", "SCHEDULED", nil)
	s.AddRun("8", "c", "SCHEDULED", map[string]string{"c4.intake": "true"})
	a := adapter(t, s, s.Password, nil)
	items, _, err := a.List(context.Background(), tracker.Query{Type: "Run", Fields: map[string]string{"status": "SCHEDULED", "c4.intake": "true"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Ref.ID != want || items[0].Due == nil || !items[0].Due.At.Equal(due) || items[0].Due.DateOnly {
		t.Fatalf("items: %+v", items)
	}
	if _, _, err := a.List(context.Background(), tracker.Query{Type: "Run", Fields: map[string]string{"x": "it's"}}, ""); err == nil {
		t.Fatal("a quote in a filter value must be refused, not passed through")
	}
}

func TestPublishOnceAndDecisions(t *testing.T) {
	s := server(t)
	run := s.AddRun("7", "a", "RUNNING", nil)
	a := adapter(t, s, s.Password, nil)
	ctx := context.Background()
	out := tracker.Output{View: "c4v1", JobURL: "https://hud/#/job/w/j", Custody: "https://hud/p", Summary: "ok"}
	ref, err := a.Publish(ctx, tracker.Ref{Type: "Run", ID: run}, out, "k")
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Publish(ctx, tracker.Ref{Type: "Run", ID: run}, out, "k")
	if err != nil || again != ref || ref.ID != "denoiser/1" || len(s.Versions("denoiser")) != 1 {
		t.Fatalf("publish: %v %v %v", ref, again, err)
	}
	ds, err := a.Decisions(ctx, ref, time.Now().Add(-time.Hour))
	if err != nil || len(ds) != 0 {
		t.Fatalf("no decision yet: %+v %v", ds, err)
	}
	s.SetAlias("denoiser", "staging", "1")
	s.SetAlias("denoiser", "production", "1")
	s.SetVersionTag("denoiser", "1", "c4.review", "accept")
	ds, err = a.Decisions(ctx, ref, time.Now().Add(-time.Hour))
	if err != nil || len(ds) != 2 || ds[0].Native != "alias:production" || ds[1].Native != "tag:accept" || ds[0].By.ID != mlflow.Registry {
		t.Fatalf("decisions: %+v %v", ds, err)
	}
}

func TestReceive(t *testing.T) {
	s := server(t)
	secret := []byte("sec")
	a := adapter(t, s, s.Password, secret)
	var got tracker.Delivery
	var gerr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, gerr = a.Receive(r) }))
	defer srv.Close()
	body := mlflowtest.Payload("model_version_tag", "set", map[string]any{"name": "denoiser", "version": "3", "key": "c4.review", "value": "rework"}, time.Now())
	if _, err := mlflowtest.Deliver(srv.URL, body, secret, "d1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if gerr != nil || got.ID != "d1" || len(got.Refs) != 1 || got.Refs[0].ID != "denoiser/3" {
		t.Fatalf("delivery: %+v %v", got, gerr)
	}
	if _, err := mlflowtest.Deliver(srv.URL, body, secret, "d2", time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gerr, tracker.ErrBadSignature) {
		t.Fatalf("stale: %v", gerr)
	}
}

func TestRunWrites(t *testing.T) {
	s := server(t)
	run := s.AddRun("7", "a", "SCHEDULED", nil)
	a := adapter(t, s, s.Password, nil)
	if _, err := a.Apply(context.Background(), tracker.Write{On: tracker.Ref{Type: "Run", ID: run}, Kind: "run",
		Fields: map[string]string{"status": "FINISHED", "c4.state": "in-review", "c4.eta": ""}}); err != nil {
		t.Fatal(err)
	}
	r := s.GetRun(run)
	if r.Status != "FINISHED" || r.End == 0 || r.Tags["c4.state"] != "in-review" {
		t.Fatalf("run: %+v", r)
	}
	if _, ok := r.Tags["c4.eta"]; ok {
		t.Fatal("an empty value is not written")
	}
}
