package localfarm

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// The local farm's image registry: a tool's tool.lock pins images by
// repository digest, which an image loaded straight onto kind's nodes does
// not have, so the farm runs a registry beside its nodes. This machine
// pushes to it at 127.0.0.1:<RegistryPort>; the nodes and the HUD reach it
// by name, at RegistryInFarm, over plain HTTP.
const (
	RegistryName   = "c4-local-registry"
	RegistryPort   = 5001
	RegistryInFarm = RegistryName + ":5000"
	RegistryImage  = "registry:3"
)

// registryPatch points every node's containerd at per-registry hosts
// files, so the nodes pull from the local registry by name.
const registryPatch = `[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"`

// docker runs the docker command and returns its trimmed output.
func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ensureRegistry starts the registry if it is not running, and joins it
// to kind's network so the nodes resolve it by name.
func ensureRegistry(ctx context.Context, port int) error {
	state, err := docker(ctx, "inspect", "-f", "{{.State.Running}}", RegistryName)
	switch {
	case err != nil:
		if err := portsFree(ctx, []farmPort{{"the farm's registry", port, "--registry-port"}}); err != nil {
			return err
		}
		if _, err := docker(ctx, "run", "-d", "--restart=always", "-p", fmt.Sprintf("127.0.0.1:%d:5000", port),
			"--name", RegistryName, RegistryImage); err != nil {
			return fmt.Errorf("starting the farm's registry: %w", err)
		}
	case state != "true":
		if _, err := docker(ctx, "start", RegistryName); err != nil {
			return fmt.Errorf("starting the farm's registry: %w", err)
		}
	}
	if _, err := docker(ctx, "network", "connect", "kind", RegistryName); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("joining the farm's registry to kind's network: %w", err)
	}
	return nil
}

// registryHosts tells a node's containerd to pull the registry's images
// over plain HTTP.
func registryHosts(ctx context.Context, node string) error {
	dir := "/etc/containerd/certs.d/" + RegistryInFarm
	script := fmt.Sprintf("mkdir -p '%s' && printf '[host.\"http://%s\"]\\n' >'%s/hosts.toml'", dir, RegistryInFarm, dir)
	if _, err := docker(ctx, "exec", node, "sh", "-c", script); err != nil {
		return fmt.Errorf("pointing %s at the farm's registry: %w", node, err)
	}
	return nil
}

// LoadImages pushes images from this machine's Docker into the local
// farm's registry, as <name>:<tag> without their registry, and returns how
// the farm names each (c4-local-registry:5000/<name>:<tag>). A tool names
// one of the farm's own images as c4/<name>; a development farm finds
// c4-<name>:dev here.
func LoadImages(ctx context.Context, port int, images []string) ([]string, error) {
	if port == 0 {
		port = RegistryPort
	}
	var out []string
	for _, img := range images {
		local := localName(img)
		push := fmt.Sprintf("127.0.0.1:%d/%s", port, local)
		if _, err := docker(ctx, "tag", img, push); err != nil {
			return out, err
		}
		if _, err := docker(ctx, "push", "-q", push); err != nil {
			return out, err
		}
		docker(ctx, "rmi", push) // the extra tag only; the image stays
		out = append(out, RegistryInFarm+"/"+local)
	}
	return out, nil
}

// localName is an image's name in the local registry: its repository and
// tag without the registry it came from (ghcr.io/avalanche-io/c4-job-hydra:v1
// is c4-job-hydra:v1), with latest when it names no tag.
func localName(img string) string {
	name := img
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	if !strings.Contains(name, ":") {
		name += ":latest"
	}
	return name
}

// farmPort is one of the farm's ports on this machine, and the flag that
// moves it.
type farmPort struct {
	what string
	port int
	flag string
}

// portsFree refuses, before kind is asked, a port something else on this
// machine holds, naming the container that publishes it when there is one
// (another kind cluster, often) and the flag that moves the farm's.
func portsFree(ctx context.Context, ports []farmPort) error {
	for _, p := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.port))
		if err == nil {
			l.Close()
			continue
		}
		by := "something else on this machine"
		if names, _ := docker(ctx, "ps", "--filter", fmt.Sprintf("publish=%d", p.port), "--format", "{{.Names}}"); names != "" {
			by = "the container " + strings.Fields(names)[0]
		}
		return fmt.Errorf("port %d, for %s, is in use by %s; stop it, or give the farm another port with %s", p.port, p.what, by, p.flag)
	}
	return nil
}
