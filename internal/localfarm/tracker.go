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

// The tracker `c4 farm up --tracker` adds: a stand-in GitHub on the farm
// (internal/sandbox, run from c4-sync's image), c4-sync, and a Tracker in
// Saltmarsh whose issues labeled c4 become jobs for the person they are
// assigned to. The page is at the HUD's /sandbox/; nothing leaves this
// machine.
const (
	SandboxTracker = "sandbox"
	SandboxRepo    = "petrel-finch/saltmarsh"
	sandboxName    = "c4-sandbox-github"
	sandboxURL     = "http://" + sandboxName + ".c4-system.svc:8080"
	// The sandbox's token and webhook secret: fixed, as the store's are,
	// for a stand-in that only c4-sync on this farm reaches.
	sandboxToken  = "c4-sandbox-local-token"
	sandboxSecret = "c4-sandbox-local-hook"
	// syncViewer is c4-sync's sign-in on a development farm's HUD.
	syncViewer = c4v1.SyncDevViewer + "@wrangler=" + Project
	// sandboxReviewer is who a sandbox job's review waits for.
	sandboxReviewer = "supervisor"
)

// SandboxPage is the sandbox's page on the HUD's port, as person.
func SandboxPage(hud, person string) string {
	return strings.TrimSuffix(hud, "/") + sandbox.Prefix + "?as=" + person
}

// sandboxPeople are the sandbox's logins: each of the local farm's
// people, by the same name.
func sandboxPeople() (flag string, people obj) {
	var parts []string
	people = obj{}
	for _, v := range Viewers {
		parts = append(parts, v.Name+"="+v.Name)
		people[v.Name] = v.Name
	}
	return strings.Join(parts, ","), people
}

// SandboxJob is the job an issue becomes: two stand-in units, then the
// job's own review, by the supervisor, which the pull request's review
// serves.
func SandboxJob() (c4v1.WorkJobSpec, error) {
	pod, err := jobspec.Pod(jobspec.Container{Image: farmcli.StandInImage, Command: []string{"sh", "-euc"},
		Args: []string{`echo "frames $C4_FRAMES: a stand-in for the issue's work"; sleep 20`}})
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	return c4v1.WorkJobSpec{Class: "render", Pools: []string{"cpu"}, Template: pod,
		Params: map[string]string{"frames": "1001-1004", "chunk": "2", "estimateSeconds": "20"},
		Review: &c4v1.WorkReviewSpec{Server: "person:" + sandboxReviewer, Choices: []string{"approve", "rework"},
			Ask: "Is the work for this issue ready?"}}, nil
}

// TrackerObjects are what --tracker adds after c4-sync is installed.
func TrackerObjects(o Options) ([]obj, error) {
	job, err := SandboxJob()
	if err != nil {
		return nil, err
	}
	logins, people := sandboxPeople()
	labels := obj{"app.kubernetes.io/name": sandboxName}
	ctr := obj{"name": "sandbox", "image": "c4-sync", "command": []string{"/c4-sandbox-github"},
		"args": []string{"--repo=" + SandboxRepo, "--people=" + logins, "--url=" + sandboxURL,
			"--hook=http://c4-sync.c4-system.svc/webhooks/" + Project + "/" + SandboxTracker,
			"--project=" + Project, "--tracker=" + SandboxTracker, "--hud=http://c4-hud.c4-system.svc"},
		"env": []obj{
			{"name": "C4_SANDBOX_TOKEN", "valueFrom": obj{"secretKeyRef": obj{"name": sandboxName, "key": "token"}}},
			{"name": "C4_SANDBOX_HOOK_SECRET", "valueFrom": obj{"secretKeyRef": obj{"name": sandboxName, "key": "hook"}}},
		},
		"ports":           []obj{{"containerPort": 8080}},
		"readinessProbe":  obj{"httpGet": obj{"path": "/healthz", "port": 8080}},
		"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": obj{"drop": []string{"ALL"}}},
	}
	secret := obj{"token": sandboxToken, "hook": sandboxSecret}
	return []obj{
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta(sandboxName, "namespace", "c4-system"), "stringData": secret},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(sandboxName, "namespace", "c4-system"),
			"spec": obj{"replicas": 1, "selector": obj{"matchLabels": labels},
				"template": obj{"metadata": obj{"labels": labels}, "spec": obj{
					"securityContext": obj{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": obj{"type": "RuntimeDefault"}},
					"containers":      []obj{ctr}}}}},
		{"apiVersion": "v1", "kind": "Service", "metadata": meta(sandboxName, "namespace", "c4-system"),
			"spec": obj{"selector": labels, "ports": []obj{{"port": 8080, "targetPort": 8080}}}},
		// The Tracker's credentials, in the project, readable by c4-sync
		// by name only.
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("sandbox-github", "namespace", Project), "stringData": secret},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": meta("c4-sync-sandbox", "namespace", Project),
			"rules": []obj{{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{"sandbox-github"}, "verbs": []string{"get"}}}},
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
	}, nil
}

// syncArgs are c4-sync's arguments on the local farm: the HUD's
// development sign-in, links to the HUD as this machine reaches it, and a
// short pass, and the farm's zone for the times it writes.
func syncArgs(o Options) []string {
	args := []string{"--hud=http://c4-hud.c4-system.svc", "--hud-dev-viewer=" + c4v1.SyncDevViewer,
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
	o.logf("tracker   c4-sync and a stand-in GitHub (%s): nothing leaves this machine\n", SandboxRepo)
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
