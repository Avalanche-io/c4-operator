package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
)

// The farm's words for what POST /jobs refuses: transient failures say
// try again; grants and their absence are refused at for or tracker,
// once, with their reason; anything else carries no operator text, and a
// dry run's estimateError neither.
func TestSubmissionWords(t *testing.T) {
	s := submissionFarm(t)
	for _, c := range []struct {
		err        error
		code       int
		path, msg  string
		fix        string
		status     int
		reason     string
		notContain string
	}{
		{apierrors.NewTooManyRequests("slow", 1), 400, "jobs.movie", "the farm could not make movie just now", "try again in a moment", 503, "", ""},
		{apierrors.NewTimeoutError("slow", 1), 400, "jobs.movie", "the farm could not make movie just now", "try again in a moment", 503, "", ""},
		{fmt.Errorf("for noor: %w", refuseGrant(http.StatusForbidden, api.RefusedNotSync, "only a tracker's sync submits work for another person")), 403,
			"for", "only a tracker's sync submits work for another person", grantFix[api.RefusedNotSync], 403, api.RefusedNotSync, ""},
		{refuseGrant(http.StatusNotImplemented, api.RefusedNotInstalled, "this farm has no trackers installed"), 501,
			"tracker", "this farm has no trackers installed, so submitting for a person is not available on this farm", grantFix[api.RefusedNotInstalled], 501, api.RefusedNotInstalled, "try again"},
		{refuseGrant(http.StatusNotImplemented, api.RefusedNotInstalled, "this farm has no submit grants installed"), 501,
			"for", "this farm has no submit grants installed, so submitting for a person is not available on this farm", grantFix[api.RefusedNotInstalled], 501, api.RefusedNotInstalled, "try again"},
		// createJob keeps the API's error (%w) whatever status it gives
		{fmt.Errorf("%w", apierrors.NewTooManyRequests("slow", 1)), 400, "jobs.movie", "the farm could not make movie just now", "try again in a moment", 503, "", ""},
		{fmt.Errorf("materialization view-1: %w", apierrors.NewInternalError(errors.New("etcd away"))), 409, "jobs.movie", "the farm could not make movie just now", "try again in a moment", 503, "", "etcd"},
		{&errGrant{http.StatusNotImplemented, "this farm has no trackers installed"}, 501,
			"tracker", "this farm has no trackers installed, so trackers are not available on this farm", "leave tracker: out on this farm", 501, "", "try again"},
		{identityError{errors.New("unit:show-a/render/3: its own job show-a/render has no owner to submit for")}, 403,
			"jobs.movie", "unit:show-a/render/3: its own job show-a/render has no owner to submit for", "a unit submits only what its tool grants, for its own job's owner", 403, "", "may not create jobs"},
		{errors.New("etcdserver: request is too large"), 400, "jobs.movie", "the farm could not make movie as lowered", "", 422, "", "etcd"},
	} {
		fe := s.farmWords("movie", "sm0140-movie-v001", "show-a", c.code, c.err)
		if fe.Path != c.path || fe.Message != c.msg || c.fix != "" && fe.Fix != c.fix || fe.code != c.status || fe.Reason != c.reason ||
			c.notContain != "" && (strings.Contains(fe.Message, c.notContain) || strings.Contains(fe.Fix, c.notContain)) {
			t.Errorf("%v: %+v (%d)", c.err, fe.FieldError, fe.code)
		}
	}
	s.DryRunAll = func(context.Context, []*c4v1.WorkJob, string, map[string]string) ([]api.SubmitEstimate, error) {
		return nil, errors.New("queue: view sm0140 has 3 units in pool cpu-show-a")
	}
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), DryRun: true})
	if code != http.StatusOK || out.Jobs[0].EstimateError != "the farm could not estimate it just now" {
		t.Fatalf("dry run: %d %+v", code, out)
	}
}

// Reuse in a job's line counts the units the estimate planned, a once
// job's too.
func TestWhatReused(t *testing.T) {
	if w := what(authoring.PlannedJob{Once: true, Units: 1}, &api.SubmitEstimate{Units: 1, Reused: 1}); w != "1 unit; 1 of 1 unit reused" {
		t.Fatalf("%q", w)
	}
}

// A job's version names it, and a name taken meanwhile is refused at its
// version, never numbered past.
func TestSubmissionVersionGiven(t *testing.T) {
	s := submissionFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	doc := strings.Replace(string(submission(t, "proxies", tree)), `"tool":"proxies"`, `"tool":"proxies","version":7`, 1)
	if code, out := submit(t, s, api.SubmissionRequest{Submission: []byte(doc)}); code != http.StatusCreated || out.Jobs[0].Name != "sm0140-proxies-v007" {
		t.Fatalf("version 7: %d %+v", code, out)
	}
	// made again: the HUD's model has not seen the first, the API has
	code, out := submit(t, s, api.SubmissionRequest{Submission: []byte(doc)})
	if code != http.StatusConflict || len(out.Errors) != 1 || out.Errors[0].Path != "jobs.proxies.version" ||
		out.Errors[0].Message != "sm0140-proxies-v007 exists; leave version out for the next one" {
		t.Fatalf("taken: %d %+v", code, out)
	}
}

// A create the API turns away for the moment (429) says try again, end to
// end, though createJob answers it 400.
func TestSubmissionTransientCreate(t *testing.T) {
	s := submissionFarm(t)
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*c4v1.WorkJob); ok {
				return apierrors.NewTooManyRequests("slow down", 1)
			}
			return c.Create(ctx, obj, opts...)
		}})
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies", tree)})
	if code != http.StatusServiceUnavailable || len(out.Errors) != 1 || !strings.HasPrefix(out.Errors[0].Fix, "try again in a moment") {
		t.Fatalf("%d %+v", code, out)
	}
}
