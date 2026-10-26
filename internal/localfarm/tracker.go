package localfarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
	"github.com/Avalanche-io/c4-operator/internal/sandbox"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

// The trackers `c4 farm up --tracker` adds: a stand-in GitHub and a
// stand-in Flow Production Tracking on the farm (internal/sandbox, run from
// c4-sync's image), c4-sync, and two Trackers in Saltmarsh: issues labeled
// c4 become jobs for the person they are assigned to, and lighting Tasks
// set ready become renders whose output is published back as a Version
// for review. The pages are at the HUD's /sandbox/ and /sandbox/flow/;
// nothing leaves this machine.
const (
	SandboxTracker = "sandbox"
	SandboxRepo    = "petrel-finch/saltmarsh"
	sandboxName    = "c4-sandbox-github"
	sandboxURL     = "http://" + sandboxName + ".c4-system.svc:8080"
	// The sandbox's token and webhook secret: fixed, as the store's are,
	// for a stand-in that only c4-sync on this farm reaches.
	sandboxToken  = "c4-sandbox-local-token"
	sandboxSecret = "c4-sandbox-local-hook"
	// sandboxFlowKey is the stand-in Flow's API script key, as fixed.
	sandboxFlowKey = "c4-sandbox-local-flow-key"
	// SandboxFlowPlate is the materialization a stand-in Flow job renders
	// into: the shot's plate, which c4 farm samples load flow uploads.
	SandboxFlowPlate = "sandbox-flow-plate"
	// sandboxReviewer is who a sandbox job's review waits for on a farm
	// with no owner; on a farm with one, the owner reviews.
	sandboxReviewer = "supervisor"
)

// sandboxPeople are the sandbox's logins: the farm's owner first, whom
// the page acts as unless told otherwise, then each of the local farm's
// roles, each by the same name.
func sandboxPeople(owner string) (flag string, people obj) {
	vs := Viewers
	if owner != "" {
		vs = append([]Viewer{OwnerViewer(owner)}, Viewers...)
	}
	var parts []string
	people = obj{}
	for _, v := range vs {
		if _, ok := people[v.Name]; ok {
			continue
		}
		parts = append(parts, v.Name+"="+v.Name)
		people[v.Name] = v.Name
	}
	return strings.Join(parts, ","), people
}

// sandboxReviewerOf is who reviews a sandbox job: the farm's one person
// where it has an owner, else the supervisor.
func sandboxReviewerOf(o Options) string {
	if o.Owner != "" {
		return o.Owner
	}
	return sandboxReviewer
}

// SandboxJob is the job an issue becomes: two stand-in units, then the
// job's own review, by reviewer, which the pull request's review serves.
func SandboxJob(reviewer string) (c4v1.WorkJobSpec, error) {
	pod, err := jobspec.Pod(jobspec.Container{Image: farmcli.StandInImage, Command: []string{"sh", "-euc"},
		Args: []string{`echo "frames $C4_FRAMES: a stand-in for the issue's work"; sleep 20`}})
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	return c4v1.WorkJobSpec{Class: "render", Pools: []string{"cpu"}, Template: pod,
		Params: map[string]string{"frames": "1001-1004", "chunk": "2", "estimateSeconds": "20"},
		Review: &c4v1.WorkReviewSpec{Server: "person:" + reviewer, Choices: []string{"approve", "rework"},
			Ask: "Is the work for this issue ready?"}}, nil
}

// sandboxFlowRender is a stand-in Flow job's unit: it renders its frames
// into the shot's plate, a placeholder EXR and a preview PNG each (the
// HUD's preview/<frame>.png, which c4-sync uploads as the Version's
// still). A version made from the supervisor's note (mounted at
// /c4/notes/note.md) renders cooler and keeps the note, so it is other
// content with its own C4 ID.
const sandboxFlowRender = `look=warm png=$S_WARM
if [ -f /c4/notes/note.md ]; then look=cool png=$S_COOL; cp /c4/notes/note.md /show/render-note.md; fi
mkdir -p /show/render /show/preview
f=$C4_FRAME_START
while [ "$f" -le "$C4_FRAME_END" ]; do
  sleep 6
  echo "stand-in frame $f of sh040 lighting ($look)" >"/show/render/sh040_lgt.$f.exr"
  echo "$png" | base64 -d >"/show/preview/$f.png"
  f=$((f + 1))
done
echo "frames $C4_FRAMES rendered ($look)"`

// SandboxFlowJob is the job a ready lighting Task becomes: eight frames in
// four units, rendered into the shot's plate, then the dailies review by
// reviewer, which the Version's status in Flow serves.
func SandboxFlowJob(reviewer string) (c4v1.WorkJobSpec, error) {
	pod, err := jobspec.Pod(jobspec.Container{Image: farmcli.StandInImage, Command: []string{"sh", "-euc"},
		Args: []string{sandboxFlowRender}, Env: map[string]string{"S_WARM": previewWarm, "S_COOL": previewCool}})
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	return c4v1.WorkJobSpec{Class: "render", Pools: []string{"cpu"}, Template: pod,
		Params: map[string]string{"frames": "1001-1008", "chunk": "2", "estimateSeconds": "12"},
		Inputs: []c4v1.WorkInput{{Materialization: SandboxFlowPlate, MountPath: "/show"}},
		Review: &c4v1.WorkReviewSpec{Server: "person:" + reviewer, Choices: []string{"approve", "rework"},
			Ask: "Dailies: approve sh040 lighting?"}}, nil
}

// The stand-in render's stills, 64x36 PNGs: a lit sphere, warm, and cooler
// after the note.
const (
	previewWarm = "iVBORw0KGgoAAAANSUhEUgAAAEAAAAAkCAIAAAC2bqvFAAAC8ElEQVR42u2W2WsTYRTF71/igkVi7J60jTVd06ZpY0ybpo1dNAld0qYlLa3VNNjGhZa2SCW4YAsuVAWXgogbgijom4i++CLim+CbL/4B+sNAHnVm4AsOBA6Xyx2Y79w7555vZIfFbWrITqvH1JDdxV5TQ/aU+kwNKSrvNDVkb2XA1BCLvcfUkH3VIVND9jv6TQ0pqR00NaTUGTY1pLw+mje8zQy+yQy8Xu97sRZ6vBR8kO7cmj+0MeO5EHcZfqdUNg3nBx82oh83o8R3V8J08vL8EXq4t+i/ccJ7ecq9Fmsy9lqxuUZV4/3VyKfrQ5+3Rr/cihHJqdAD3+Hh2cDtlI+PsB53nYvWJfscel8uVa3jqgHjr3fGvt2f+L49SSSnwndAS0+Wg3cX/NfmOjKTLcvDDamBA3pfLjVtE0qBZpg6vH88mv75dIZIToU6+/BspZdNQEUXE60rI42nBmunu+263i+O9oQ6oBN0j3KYPex/vUoSyalQ5+nz1d7t0103T3ov5RoIVo14y7QfIbXeaXVgxga+wHBHmfYjxOmbVQdUjtZ17UAiYB9qL9V+hNT559QBn0EnulxostMW9ZRoP0IaupLqgNPj93Bl3v+8B9LHDh7vrR4/XHHUXaz9CGnqTqkD+oYlk0YtKP4vN/HSUD36meq2s8H9Lqv2I6S5Z0Ed4MeM0QlaZ1/xHCI5Feo83Zz1oH7Wd5Hxh2ri/spIW0lfs1X7EdISSqsD00UhqJxNZd44JpGcCnWewn51tPFMxIn6EwEb/sP4aUD7EWIp2qUOaAN9QxSfgTF+TySnQp2nzB728/0O3DPmK0f9sAfaj1DbAGA7cRgmjdNzWxHJqVBH9yiH2cN+zFcR/iMeXezz0QDAH/F4hp0FORU8h61F9yiH2Rtjn6cGmDFcuWWzIKeCY+I5bC26zynnP20gC3TCfw6Ru5bbCr/HMfGc7NYaoJ7vBgCMmTd/Cty1TD1H3TD7fDegAoUGCg0UGjB5A78BxTjd7N5iumMAAAAASUVORK5CYII="
	previewCool = "iVBORw0KGgoAAAANSUhEUgAAAEAAAAAkCAIAAAC2bqvFAAAC0UlEQVR42u2W+0uacRTGz1+yC4vhbhHtFtE22sU077dMzUuZlmlqqV1c5kLKitzIxYQagwYb7Mf9h/uYbD9ur8JX9oLwcDicF97vc573Oef7yjWDUdeQ63dNuobcvG/RNeTWsE3XkKERp64ht0fduoYYHvl0DbnzxK9ryL2xoK4hD8bndA0ZnojqGjLyfL5veFdrVGsnlb3jt7v1zXKtUNrN5rdT6UI8udrzO2V0crE/ODj6WD8+I9bqp3SyUz2kh/ViJZPbTKbWYvGV3l4rD18lVWP/sHnUaJ18OG+cXhDJqdAD36G0tZdbL6cyxXhiNRxNzgSi3b5cHr9JqQaM3zc/Nz9dnrW+Esmp8B3wEh9hrVhJZzcSS7nI/PJscL7bl8vTqbRS4BlUh3fr4vv5lx9EcirUmYftnX0moe2i5Xx0IRUILbi9oa7eL2PmrDrgE3yPc9Ae9pfffhLJqVDnablyUNioXo1BPnbVgMcXstq92o+QcUteHdC4hy9gtXu0HyETtoI64HK83tUMuDxBi82j/Qh55iipA3sGn3S1hZzu2WmrS/sR8sK1pQ4IzL6HK3r/8x4Ihhe9/rDdOWOadmg/QiY9ZXXA37BEadyC4/9yE0diS/jH7Q0ywUaTTfsR8tJXUQf4oTE+gS7zys4hklOh3mafKeJ+9k9wLu7zhx0uv9niMk7ZtB8hr/1VdUBdHILLmVT0ZmMSyalQ52mbfXwlFEngfpcnwP5BfhrQfoQYhm6oA97A38jMnoFxB+RUqPMU7WHvD8TYnjaHD/fDHmg/Qm0DAIHZMCjNpue2IpJToY7vcQ7aw57ZNVuc3bLvRwOA/ciO555CbyI5FXYOU4vvcQ7a98a+Tw2gMVy5ZTsgp8LGZOcwtfj+j3P+0wY6wCf85xC5a7mt8Awbs71zTD1S73cDAMbozZ8Cd21b9d/Ue2bf7wZUYNDAoIFBAzpv4Bde6M18ECLfvgAAAABJRU5ErkJggg=="
)

// TrackerObjects are what --tracker adds after c4-sync is installed.
func TrackerObjects(o Options) ([]obj, error) {
	job, err := SandboxJob(sandboxReviewerOf(o))
	if err != nil {
		return nil, err
	}
	flowJob, err := SandboxFlowJob(sandboxReviewerOf(o))
	if err != nil {
		return nil, err
	}
	logins, people := sandboxPeople(o.Owner)
	labels := obj{"app.kubernetes.io/name": sandboxName}
	ctr := obj{"name": "sandbox", "image": "c4-sync", "command": []string{"/c4-sandbox-github"},
		"args": []string{"--repo=" + SandboxRepo, "--people=" + logins, "--url=" + sandboxURL,
			"--hook=http://c4-sync.c4-system.svc/webhooks/" + Project + "/" + SandboxTracker,
			"--project=" + Project, "--tracker=" + SandboxTracker, "--hud=http://c4-hud.c4-system.svc",
			// it reads the HUD as itself, never as a person
			"--hud-token-file=/var/run/c4-sandbox/token"},
		"env": []obj{
			{"name": "C4_SANDBOX_TOKEN", "valueFrom": obj{"secretKeyRef": obj{"name": sandboxName, "key": "token"}}},
			{"name": "C4_SANDBOX_HOOK_SECRET", "valueFrom": obj{"secretKeyRef": obj{"name": sandboxName, "key": "hook"}}},
			{"name": "C4_SANDBOX_FLOW_KEY", "valueFrom": obj{"secretKeyRef": obj{"name": sandboxName, "key": "flow"}}},
		},
		"ports":           []obj{{"containerPort": 8080}},
		"volumeMounts":    []obj{{"name": "hud-token", "mountPath": "/var/run/c4-sandbox", "readOnly": true}},
		"readinessProbe":  obj{"httpGet": obj{"path": "/healthz", "port": 8080}},
		"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": obj{"drop": []string{"ALL"}}},
	}
	secret := obj{"token": sandboxToken, "hook": sandboxSecret}
	return []obj{
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta(sandboxName, "namespace", "c4-system"),
			"stringData": obj{"token": sandboxToken, "hook": sandboxSecret, "flow": sandboxFlowKey}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(sandboxName, "namespace", "c4-system"),
			"spec": obj{"replicas": 1, "selector": obj{"matchLabels": labels},
				"template": obj{"metadata": obj{"labels": labels}, "spec": obj{
					"serviceAccountName": sandboxName,
					"securityContext":    obj{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": obj{"type": "RuntimeDefault"}},
					"containers":         []obj{ctr},
					// a token for the HUD alone, short-lived and rotated by the
					// kubelet, as c4-sync's is
					"volumes": []obj{{"name": "hud-token", "projected": obj{"sources": []obj{{"serviceAccountToken": obj{
						"path": "token", "audience": c4v1.HUDAPIAudience, "expirationSeconds": 3600}}}}}}}}}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta(sandboxName, "namespace", "c4-system")},
		// What the sandbox reads, as itself: the project's jobs, which is
		// also what makes the HUD show it the project (and its Tracker's
		// grants). Nothing it could change.
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": meta(sandboxName, "namespace", Project),
			"rules": []obj{{"apiGroups": []string{"c4.avalanche.io"}, "resources": []string{"workjobs"}, "verbs": []string{"get", "list"}}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": meta(sandboxName, "namespace", Project),
			"roleRef":  obj{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": sandboxName},
			"subjects": []obj{{"kind": "ServiceAccount", "name": sandboxName, "namespace": "c4-system"}}},
		{"apiVersion": "v1", "kind": "Service", "metadata": meta(sandboxName, "namespace", "c4-system"),
			"spec": obj{"selector": labels, "ports": []obj{{"port": 8080, "targetPort": 8080}}}},
		// The Tracker's credentials, in the project, readable by c4-sync
		// by name only.
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("sandbox-github", "namespace", Project), "stringData": secret},
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("sandbox-flow", "namespace", Project), "stringData": obj{"key": sandboxFlowKey}},
		// and the project's jobs, which the HUD lets c4-sync submit and
		// change as itself (docs/c4-sync.md), as a project on any farm does
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": meta("c4-sync-sandbox", "namespace", Project),
			"rules": []obj{{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{"sandbox-github", "sandbox-flow"}, "verbs": []string{"get"}},
				{"apiGroups": []string{"c4.avalanche.io"}, "resources": []string{"workjobs"}, "verbs": []string{"get", "list", "watch", "create", "patch"}},
				// a decision's evidence (the note a rework mounts) goes to
				// the project's store, which the HUD lets in as it lets in a tree
				{"apiGroups": []string{"c4.avalanche.io"}, "resources": []string{"materializations"}, "verbs": []string{"create"}}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": meta("c4-sync-sandbox", "namespace", Project),
			"roleRef":  obj{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "c4-sync-sandbox"},
			"subjects": []obj{{"kind": "ServiceAccount", "name": "c4-sync", "namespace": "c4-system"}}},
		{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Tracker",
			"metadata": obj{"name": SandboxTracker, "namespace": Project, "annotations": obj{syncer.AnnotationSelf: "c4-sync[bot]"}},
			"spec": obj{"kind": "github", "endpoint": sandboxURL, "scope": obj{"repo": SandboxRepo},
				"credential":    obj{"secret": obj{"name": "sandbox-github", "key": "token"}},
				"webhookSecret": obj{"name": "sandbox-github", "key": "hook"},
				"events":        obj{"mode": "webhook", "pollSeconds": 10, "resyncSeconds": 300},
				// a stand-in on the farm has no rate limit; a pass is a second, not a minute
				"rate":   obj{"perSecond": "50", "burst": 100},
				"people": people,
				"intake": []obj{{"match": obj{"type": "Issue", "labels": []string{sandbox.Label}}, "as": "job",
					"for": obj{"assignee": true}, "rework": "nextVersion", "job": job}},
				"statusOut": obj{"commitStatus": true, "fieldSeconds": 30}}},
		// The stand-in Flow: its API on the sandbox, links to its page as
		// this machine reaches it through the HUD, statuses as a default
		// site has them, read from its event log.
		{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Tracker",
			"metadata": obj{"name": sandbox.FlowTracker, "namespace": Project},
			"spec": obj{"kind": "flow", "endpoint": sandboxURL + sandbox.FlowAPI,
				"scope": obj{"project": strconv.Itoa(sandbox.FlowProject), "script": "c4-sync", "review_status": "rev",
					"id_field": "sg_c4_id", "eta_field": "sg_c4_eta", "link_field": "sg_c4_link", "spend_field": "sg_c4_spend",
					"site_url": o.HUD() + strings.TrimSuffix(sandbox.FlowPrefix, "/")},
				"credential": obj{"secret": obj{"name": "sandbox-flow", "key": "key"}},
				"events":     obj{"mode": "eventlog", "pollSeconds": 5, "resyncSeconds": 300},
				"rate":       obj{"perSecond": "50", "burst": 100},
				"people":     people,
				"intake": []obj{{"match": obj{"type": "Task", "fields": obj{"sg_status_list": "rdy", "step.Step.short_name": "LGT"}},
					"as": "job", "for": obj{"assignee": true}, "rework": "nextVersion", "job": flowJob}},
				"statusOut":  obj{"states": obj{"running": "ip", "rework": "ip", "in-review": "rev", "needs-person": "hld"}, "fieldSeconds": 15},
				"verdictsIn": obj{"status:apr": "approve", "status:fix": "rework"}}},
	}, nil
}

// syncArgs are c4-sync's arguments on the local farm: its own projected
// token for the HUD, as on any farm (a pod never holds the installer's),
// links to the HUD as this machine reaches it, a short pass, and the
// farm's zone for the times it writes.
func syncArgs(o Options) []string {
	args := []string{"--hud=http://c4-hud.c4-system.svc", "--hud-token-file=/var/run/c4-sync/token",
		"--hud-public-url=" + o.HUD(), "--interval=10s"}
	if o.TZ != "" {
		args = append(args, "--tz="+o.TZ)
	}
	return args
}

var syncImage = regexp.MustCompile(`(image:\s*"?)([^\s"]*c4-sync(?::[^\s"]+)?)("?)`)

// syncManifest is c4-sync's install manifest (its two CRDs and
// config/sync): from a file, else the release's.
func (o UpOptions) syncManifest(ctx context.Context) ([]byte, error) {
	var data []byte
	if o.SyncInstall != "" {
		b, err := os.ReadFile(o.SyncInstall)
		if err != nil {
			return nil, err
		}
		data = b
	} else {
		version, _, err := farmcli.ReleaseFor(ctx, nil, o.Version)
		if errors.Is(err, farmcli.ErrNoRelease) {
			return nil, errors.New("this is a development build and no release is published yet: pass --sync-install bin/c4-sync.yaml and --sync-image c4-sync:dev (make sync-yaml image-sync)")
		}
		if err != nil {
			return nil, err
		}
		u := "https://github.com/Avalanche-io/c4-operator/releases/download/" + version + "/c4-sync.yaml"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("downloading %s: %w", u, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("downloading %s: %s", u, resp.Status)
		}
		if data, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20)); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// upTracker installs c4-sync and the sandbox, and waits for both.
func upTracker(ctx context.Context, c client.Client, o UpOptions) error {
	m, err := o.syncManifest(ctx)
	if err != nil {
		return err
	}
	img := o.SyncImage
	if img != "" && !strings.Contains(img, "/") {
		// a development build on this machine: into the farm's registry
		got, err := LoadImages(ctx, o.RegistryHostPort, []string{img})
		if err != nil {
			return fmt.Errorf("loading %s into the farm's registry: %w", img, err)
		}
		img = got[0]
	}
	if img != "" {
		m = syncImage.ReplaceAll(m, []byte("${1}"+img+"${3}"))
	} else if s := syncImage.FindSubmatch(m); s != nil {
		img = string(s[2])
	}
	o.logf("tracker   c4-sync, a stand-in GitHub (%s) and a stand-in Flow: nothing leaves this machine\n", SandboxRepo)
	if err := applyAll(ctx, c, m); err != nil {
		return fmt.Errorf("installing c4-sync: %w", err)
	}
	objs, err := TrackerObjects(o.Options)
	if err != nil {
		return err
	}
	for _, x := range objs {
		if x["kind"] == "Deployment" {
			spec := x["spec"].(obj)["template"].(obj)["spec"].(obj)
			spec["containers"].([]obj)[0]["image"] = img
		}
	}
	if err := applyAll(ctx, c, YAML(objs)); err != nil {
		return fmt.Errorf("the sandbox tracker: %w", err)
	}
	if err := patchSync(ctx, c, o.Options); err != nil {
		return err
	}
	for _, d := range []string{"c4-sync", sandboxName} {
		if err := waitDeployment(ctx, c, "c4-system", d, 5*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// patchSync gives c4-sync the local farm's arguments.
func patchSync(ctx context.Context, c client.Client, o Options) error {
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: "c4-sync"}, &dep); err != nil {
		return err
	}
	p, _ := json.Marshal(obj{"spec": obj{"template": obj{"spec": obj{"containers": []obj{{"name": "sync", "args": syncArgs(o)}}}}}})
	if err := c.Patch(ctx, &dep, client.RawPatch(types.StrategicMergePatchType, p)); err != nil {
		return fmt.Errorf("configuring c4-sync: %w", err)
	}
	return nil
}

// HasTracker says whether the local farm has the sandbox tracker.
func HasTracker(ctx context.Context) bool {
	c, err := kubeClient(Name)
	if err != nil {
		return false
	}
	var d appsv1.Deployment
	return c.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: sandboxName}, &d) == nil
}
