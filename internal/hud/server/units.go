package server

// Units as viewers (DESIGN 10.6, tools). A unit whose tool
// grants c4 (read, job or project) holds a token for its own pod with the
// HUD's audience, which the Kubernetes API does not accept. The HUD checks
// it with a TokenReview for that audience, finds the pod it is bound to,
// and lets the unit do what its tool grants: read sees its project;
// job may also act on its own job; project may also submit jobs in its
// project. Every control it makes is recorded under its name.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// forHUD reports whether a token says it is a unit's, for the HUD (its
// aud claim): a hint for which review to ask; the review decides.
func forHUD(tok string) bool { return claimsAudience(tok, c4v1.HUDAudience) }

// claimsAudience reports whether a token's aud claim names aud.
func claimsAudience(tok, aud string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(b, &claims) != nil {
		return false
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil {
		return one == aud
	}
	var many []string
	return json.Unmarshal(claims.Aud, &many) == nil && slices.Contains(many, aud)
}

// unitViewer is the unit whose pod token tok is, with its tool's grant.
func (s *Server) unitViewer(ctx context.Context, tok string) (*api.Viewer, error) {
	tr, err := s.Kube.AuthenticationV1().TokenReviews().Create(ctx,
		&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: tok, Audiences: []string{c4v1.HUDAudience}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if !tr.Status.Authenticated || !slices.Contains(tr.Status.Audiences, c4v1.HUDAudience) {
		return nil, errUnauthenticated
	}
	extra := tr.Status.User.Extra
	podName, podUID := first(extra["authentication.kubernetes.io/pod-name"]), first(extra["authentication.kubernetes.io/pod-uid"])
	ns := ""
	if rest, ok := strings.CutPrefix(tr.Status.User.Username, "system:serviceaccount:"); ok {
		ns, _, _ = strings.Cut(rest, ":")
	}
	if podName == "" || ns == "" {
		return nil, errors.New("the token is not bound to a pod")
	}
	var pod corev1.Pod
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, &pod); err != nil || string(pod.UID) != podUID {
		return nil, errors.New("the token's pod is not a running unit")
	}
	job, unit, tool := pod.Labels[c4v1.LabelJob], pod.Labels[c4v1.LabelUnit], pod.Labels[c4v1.LabelTool]
	if job == "" || tool == "" {
		return nil, errors.New("the token's pod is not a unit with a tool")
	}
	var p c4v1.Tool
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: tool}, &p)
	if apierrors.IsNotFound(err) && s.Namespace != "" {
		err = s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: tool}, &p)
	}
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", tool, err)
	}
	if p.Spec.C4 == "" {
		return nil, fmt.Errorf("tool %s grants no c4", tool)
	}
	v := &api.Viewer{User: fmt.Sprintf("unit:%s/%s/%s", ns, job, unit), Projects: []string{},
		Unit: &api.UnitViewer{Namespace: ns, Job: job, Unit: unit, Grant: p.Spec.C4}}
	if m := s.current(); m != nil {
		for _, pr := range m.projects {
			if slices.Contains(pr.Spec.Namespaces, ns) {
				v.Projects = append(v.Projects, pr.Name)
			}
		}
	}
	return v, nil
}

func first(xs authnv1.ExtraValue) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

// unitMay says whether a unit viewer may make a write: read none, job its
// own job's, project any in its project's namespaces.
func (s *Server) unitMay(v *api.Viewer, ns, resource, name string) error {
	u := v.Unit
	switch u.Grant {
	case "job":
		if ns == u.Namespace && resource == "workjobs" && name == u.Job {
			return nil
		}
		return fmt.Errorf("%s may act only on its own job, %s/%s", v.User, u.Namespace, u.Job)
	case "project":
		if m := s.current(); m != nil {
			for _, pr := range m.projects {
				if slices.Contains(pr.Spec.Namespaces, ns) && v.Sees(pr.Name) {
					return nil
				}
			}
		}
		return fmt.Errorf("%s may act only in its own project", v.User)
	}
	return fmt.Errorf("%s may read, not change, what it sees", v.User)
}
