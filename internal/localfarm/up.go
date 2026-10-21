package localfarm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/config/samples/quickstart"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/schema"
	"github.com/Avalanche-io/c4-operator/tools"
)

// UpOptions are `c4 farm up`'s choices.
type UpOptions struct {
	Options
	// Install is the operator's install manifest: a path, or empty for
	// the release of Version.
	Install string
	Version string
	// ImageArchive is an OCI or docker archive of the operator image to
	// load into the cluster's nodes (a development build).
	ImageArchive string
	Log          io.Writer
}

// Requirements of Docker for the local farm.
const (
	MinMemory = 6 << 30
	MinCPUs   = 4
)

// CheckDocker makes sure Docker runs with enough memory and CPUs.
func CheckDocker(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.MemTotal}} {{.NCPU}}").Output()
	if err != nil {
		return errors.New("Docker is not running: start Docker Desktop (or another Docker engine) and try again")
	}
	var mem int64
	var cpus int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &mem, &cpus); err != nil {
		return fmt.Errorf("docker info: %q", out)
	}
	if mem < MinMemory || cpus < MinCPUs {
		return fmt.Errorf("Docker has %.1f GB of memory and %d CPUs; the local farm needs %d GB and %d (Docker Desktop: Settings, Resources)",
			float64(mem)/(1<<30), cpus, MinMemory>>30, MinCPUs)
	}
	return nil
}

func (o UpOptions) logf(format string, a ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format, a...)
	}
}

// Exists reports whether the local farm's cluster exists.
func Exists(name string) (bool, error) {
	names, err := cluster.NewProvider().List()
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

// Up creates the local farm, or finishes one a previous run left half
// made: every step is idempotent.
func Up(ctx context.Context, o UpOptions) error {
	name := o.name()
	p := cluster.NewProvider()
	have, err := Exists(name)
	if err != nil {
		return err
	}
	if !have {
		if err := portsFree(ctx, []farmPort{
			{"the HUD", or(o.HUDHostPort, HUDPort), "--port"},
			{"the store", or(o.StoreHostPort, StorePort), "--store-port"},
		}); err != nil {
			return err
		}
		o.logf("cluster   creating %s: a control plane and %d workers\n", name, len(nodes))
		if err := p.Create(name, cluster.CreateWithRawConfig(KindConfig(o.Options)), cluster.CreateWithWaitForReady(5*time.Minute),
			cluster.CreateWithDisplayUsage(false), cluster.CreateWithDisplaySalutation(false)); err != nil {
			return fmt.Errorf("creating the cluster: %w", err)
		}
	}
	// the farm's registry, which every node pulls from by name
	if err := ensureRegistry(ctx, or(o.RegistryHostPort, RegistryPort)); err != nil {
		return err
	}
	all, err := p.ListInternalNodes(name)
	if err != nil {
		return err
	}
	for _, n := range all {
		if err := registryHosts(ctx, n.String()); err != nil {
			return err
		}
	}
	o.logf("registry  %s, at 127.0.0.1:%d (c4 farm image load pushes to it)\n", RegistryInFarm, or(o.RegistryHostPort, RegistryPort))
	if o.ImageArchive != "" {
		o.logf("image     loading %s\n", filepath.Base(o.ImageArchive))
		ns, err := p.ListInternalNodes(name)
		if err != nil {
			return err
		}
		for _, n := range ns {
			f, err := os.Open(o.ImageArchive)
			if err != nil {
				return err
			}
			err = nodeutils.LoadImageArchive(n, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("loading the image into %s: %w", n.String(), err)
			}
		}
	}
	c, err := kubeClient(name)
	if err != nil {
		return err
	}

	install, err := o.installManifest(ctx)
	if err != nil {
		return err
	}
	o.logf("operator  installing\n")
	if err := applyAll(ctx, c, install); err != nil {
		return fmt.Errorf("installing the operator: %w", err)
	}
	o.logf("store     Garage, bucket quickstart\n")
	if err := applyAll(ctx, c, quickstart.Store); err != nil {
		return fmt.Errorf("the store: %w", err)
	}
	if err := waitJob(ctx, c, "c4-store", "bucket", 5*time.Minute); err != nil {
		return fmt.Errorf("the store's bucket: %w", err)
	}
	if err := waitDeployment(ctx, c, "c4-system", "c4-manager", 5*time.Minute); err != nil {
		return err
	}
	o.logf("project   %s, pools cpu, gpu and cloud-gpu (burst)\n", Project)
	if err := applyAll(ctx, c, YAML(Farm(o.Options))); err != nil {
		return err
	}
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: "c4-manager"}, &dep); err != nil {
		return err
	}
	var args []string
	for _, ct := range dep.Spec.Template.Spec.Containers {
		if ct.Name == "manager" {
			args = ct.Args
		}
	}
	if err := c.Patch(ctx, &dep, client.RawPatch(types.StrategicMergePatchType, ManagerPatch(o.Options, args))); err != nil {
		return fmt.Errorf("configuring the manager: %w", err)
	}
	for _, d := range []string{"c4-manager", "c4-regional"} {
		if err := waitDeployment(ctx, c, "c4-system", d, 5*time.Minute); err != nil {
			return err
		}
	}
	if err := waitHUD(ctx, o.HUD(), 5*time.Minute); err != nil {
		return err
	}
	return publishTools(ctx, o)
}

// publishTools publishes the release's own tools (the folders c4-farm
// embeds) to every project, as tested in the release they shipped in, so
// the farm runs no test for them. A tool whose image the farm cannot pin
// (a development build whose job images are not loaded) is left out and
// named, not fatal: the farm is still a farm.
func publishTools(ctx context.Context, o UpOptions) error {
	c, err := farmcli.NewClient(farmcli.Farm{HUD: o.HUD(), As: "admin", Local: true})
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "c4-tools-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var done, left, unloaded, images []string
	for _, name := range tools.Names {
		f, err := tools.Folder(name)
		if err != nil {
			return err
		}
		dir := filepath.Join(tmp, name)
		if err := os.CopyFS(dir, f); err != nil {
			return err
		}
		var out strings.Builder
		err = farmcli.PublishTool(ctx, c, farmcli.Out{W: &out}, farmcli.PublishArgs{Dir: dir, Project: Project, Farm: true, TestedIn: o.Version})
		if err != nil {
			why := strings.TrimSpace(out.String())
			if why == "" {
				why = err.Error()
			}
			if strings.Contains(why, "cannot be pinned") {
				// its image is not in the farm's registry yet
				unloaded = append(unloaded, name)
				if t, err := farmcli.ReadTool(dir); err == nil {
					img := localName(schema.ImageRef(t.Runs.Image, "dev"))
					if !slices.Contains(images, img) {
						images = append(images, img)
					}
				}
				continue
			}
			left = append(left, name+" ("+strings.Join(strings.Fields(why), " ")+")")
			continue
		}
		done = append(done, name)
	}
	if len(done) > 0 {
		o.logf("tools     %s, for every project\n", strings.Join(done, ", "))
	}
	if len(unloaded) > 0 {
		o.logf("tools     %s wait for their images: c4 farm image load %s, then c4 farm up\n", strings.Join(unloaded, ", "), strings.Join(images, " "))
	}
	for _, l := range left {
		o.logf("tools     not published: %s\n", l)
	}
	return nil
}

// kubeClient is a client for the local farm's cluster.
func kubeClient(name string) (client.Client, error) {
	kc, err := cluster.NewProvider().KubeConfig(name, false)
	if err != nil {
		return nil, err
	}
	rc, err := clientcmd.RESTConfigFromKubeConfig([]byte(kc))
	if err != nil {
		return nil, err
	}
	// The client logs nothing a user should see; without a logger it
	// prints a warning and a stack trace.
	ctrllog.SetLogger(logr.Discard())
	return client.New(rc, client.Options{})
}

// Down deletes the local farm.
func Down(name string) error {
	if name == "" {
		name = Name
	}
	p := cluster.NewProvider()
	if err := p.Delete(name, ""); err != nil {
		return err
	}
	// and its registry, with the images pushed to it, unless another local
	// farm (one whose nodes pull from it) still uses it
	ctx := context.Background()
	others, _ := p.List()
	for _, other := range others {
		nodes, err := p.ListInternalNodes(other)
		if err != nil || len(nodes) == 0 {
			continue
		}
		if _, err := docker(ctx, "exec", nodes[0].String(), "test", "-f", "/etc/containerd/certs.d/"+RegistryInFarm+"/hosts.toml"); err == nil {
			return nil
		}
	}
	if out, err := docker(ctx, "rm", "-f", RegistryName); err != nil && !strings.Contains(out, "No such container") {
		return err
	}
	return nil
}

// installManifest is the operator's install.yaml: a file, or the release's.
func (o UpOptions) installManifest(ctx context.Context) ([]byte, error) {
	var data []byte
	if o.Install != "" {
		b, err := os.ReadFile(o.Install)
		if err != nil {
			return nil, err
		}
		data = b
	} else {
		version, dev, err := farmcli.ReleaseFor(ctx, nil, o.Version)
		if errors.Is(err, farmcli.ErrNoRelease) {
			return nil, errors.New("this is a development build and no release is published yet: pass --install bin/install.yaml and --image-archive (make install-yaml, make image)")
		}
		if err != nil {
			return nil, err
		}
		if dev {
			o.logf("operator  a development build: installing the latest release, %s (--install for your own build)\n", version)
		}
		u := "https://github.com/Avalanche-io/c4-operator/releases/download/" + version + "/install.yaml"
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
		if data, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20)); err != nil {
			return nil, err
		}
	}
	return OverrideImage(data, o.OperatorImage), nil
}

var operatorImage = regexp.MustCompile(`(image:\s*)("?)[^\s"]*c4-operator(:[^\s"]+)?("?)`)

// OverrideImage points every operator container in a manifest at image
// (with imagePullPolicy left as the manifest has it); "" changes nothing.
func OverrideImage(data []byte, image string) []byte {
	if image == "" {
		return data
	}
	return operatorImage.ReplaceAll(data, []byte("${1}${2}"+image+"${4}"))
}

// Docs splits a multi-document YAML file into objects.
func Docs(data []byte) ([]*unstructured.Unstructured, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []*unstructured.Unstructured
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
}

// applyAll server-side applies every object, CRDs and namespaces first,
// retrying while freshly made CRDs and webhooks come up.
func applyAll(ctx context.Context, c client.Client, data []byte) error {
	objs, err := Docs(data)
	if err != nil {
		return err
	}
	first := func(u *unstructured.Unstructured) int {
		switch u.GetKind() {
		case "CustomResourceDefinition", "Namespace":
			return 0
		}
		return 1
	}
	slices.SortStableFunc(objs, func(a, b *unstructured.Unstructured) int { return first(a) - first(b) })
	for _, u := range objs {
		var err error
		for try := 0; try < 30; try++ {
			if err = c.Patch(ctx, u, client.Apply, client.FieldOwner("c4-farm"), client.ForceOwnership); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

func waitDeployment(ctx context.Context, c client.Client, ns, name string, limit time.Duration) error {
	return poll(ctx, limit, "deployment "+name, func() (bool, error) {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			return false, nil
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		return d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want, nil
	})
}

func waitJob(ctx context.Context, c client.Client, ns, name string, limit time.Duration) error {
	return poll(ctx, limit, "job "+name, func() (bool, error) {
		var j batchv1.Job
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j); err != nil {
			return false, nil
		}
		return j.Status.Succeeded > 0, nil
	})
}

func waitHUD(ctx context.Context, hud string, limit time.Duration) error {
	u := hud + "/api/v1/snapshot?as=wrangler"
	return poll(ctx, limit, "the HUD", func() (bool, error) {
		resp, err := http.Get(u)
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	})
}

func poll(ctx context.Context, limit time.Duration, what string, ok func() (bool, error)) error {
	end := time.Now().Add(limit)
	for {
		done, err := ok()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("%s was not ready after %s", what, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// LocalTZ is this machine's IANA time zone, or "".
func LocalTZ() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	link, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if _, after, ok := strings.Cut(link, "zoneinfo/"); ok {
		return after
	}
	return ""
}

// Place puts "This machine" on the map near its time zone's city.
func Place(tz string) (float64, float64) {
	if p, ok := places[tz]; ok {
		return p[0], p[1]
	}
	return 0, 0
}

var places = map[string][2]float64{
	"America/Los_Angeles": {34.05, -118.24}, "America/Vancouver": {49.28, -123.12}, "America/Denver": {39.74, -104.99},
	"America/Chicago": {41.88, -87.63}, "America/New_York": {40.71, -74.01}, "America/Toronto": {43.65, -79.38},
	"America/Sao_Paulo": {-23.55, -46.63}, "Europe/London": {51.51, -0.13}, "Europe/Paris": {48.86, 2.35},
	"Europe/Berlin": {52.52, 13.40}, "Asia/Kolkata": {19.08, 72.88}, "Asia/Singapore": {1.35, 103.82},
	"Asia/Tokyo": {35.68, 139.69}, "Asia/Seoul": {37.57, 126.98}, "Australia/Sydney": {-33.87, 151.21},
	"Pacific/Auckland": {-36.85, 174.76},
}
