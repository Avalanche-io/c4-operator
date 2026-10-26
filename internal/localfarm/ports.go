package localfarm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// A local farm's HUD and store are at host ports kind fixes when it makes
// the cluster: a farm made with --port 30180 keeps 30180 whatever a later
// up is told. So up reads an existing farm's ports from its node rather
// than assuming them, and a new farm takes the next free port when the
// default is taken (by another farm on this machine), saying so.

// MappedPorts are the host ports farm name's control plane maps the HUD's
// and the store's NodePorts to; 0 for one it does not map.
func MappedPorts(ctx context.Context, name string) (hud, store int, err error) {
	out, err := docker(ctx, "inspect", "-f", "{{json .NetworkSettings.Ports}}", name+"-control-plane")
	if err != nil {
		return 0, 0, err
	}
	hud, store, err = parsePorts(out)
	if err != nil {
		return 0, 0, fmt.Errorf("the ports of %s-control-plane: %w", name, err)
	}
	return hud, store, nil
}

// parsePorts reads the HUD's and the store's host ports from a node's
// .NetworkSettings.Ports, as docker inspect gives them.
func parsePorts(out string) (hud, store int, err error) {
	var ports map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(out), &ports); err != nil {
		return 0, 0, err
	}
	host := func(p int) int {
		for _, b := range ports[strconv.Itoa(p)+"/tcp"] {
			if n, err := strconv.Atoi(b.HostPort); err == nil {
				return n
			}
		}
		return 0
	}
	return host(HUDPort), host(StorePort), nil
}

// PortOwner is the local farm (kind cluster) whose node publishes host
// port, else the container that does; "" when none does.
func PortOwner(ctx context.Context, port int) string {
	names, _ := docker(ctx, "ps", "--filter", fmt.Sprintf("publish=%d", port), "--format", "{{.Names}}")
	f := strings.Fields(names)
	if len(f) == 0 {
		return ""
	}
	if c, ok := strings.CutSuffix(f[0], "-control-plane"); ok {
		return c
	}
	return "the container " + f[0]
}

// free says nothing on this machine listens at 127.0.0.1:port (a
// variable, for tests).
var free = func(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// nextFree is the first free port of from, from+100, from+200, ..., the
// way a second farm's ports are written (30180 beside 30080); 0 when ten
// tries find none.
func nextFree(from int, free func(int) bool) int {
	for p := from; p < from+1000; p += 100 {
		if free(p) {
			return p
		}
	}
	return 0
}

// ResolvePorts settles o's HUD and store ports before up, and says what it
// chose. An existing farm keeps the ports it was made with, read from its
// node, and a --port that asks for another is said to be ignored. A new
// farm takes a port given (up refuses it when taken) or, with none given,
// the default, else the next free one.
func ResolvePorts(ctx context.Context, o *Options, exists bool, say func(format string, a ...any)) error {
	name := o.name()
	if exists {
		hud, store, err := MappedPorts(ctx, name)
		if err != nil {
			return err
		}
		for _, p := range []struct {
			what       string
			asked, has int
			set        *int
		}{{"HUD", o.HUDHostPort, hud, &o.HUDHostPort}, {"store", o.StoreHostPort, store, &o.StoreHostPort}} {
			if p.has == 0 {
				continue
			}
			if p.asked != 0 && p.asked != p.has {
				say("ports     %s's %s is at 127.0.0.1:%d, fixed when it was made; the port asked for, %d, is not used\n", name, p.what, p.has, p.asked)
			}
			*p.set = p.has
		}
		return nil
	}
	for _, p := range []struct {
		what string
		def  int
		set  *int
	}{{"HUD", HUDPort, &o.HUDHostPort}, {"store", StorePort, &o.StoreHostPort}} {
		if *p.set != 0 || free(p.def) {
			continue
		}
		n := nextFree(p.def+100, free)
		if n == 0 {
			return fmt.Errorf("port %d, for the %s, is in use by %s, and so are the next ones: give the farm a port with --%s", p.def, p.what, ownerOr(ctx, p.def), map[string]string{"HUD": "port", "store": "store-port"}[p.what])
		}
		say("ports     the %s at 127.0.0.1:%d, since %d is in use by %s\n", p.what, n, p.def, ownerOr(ctx, p.def))
		*p.set = n
	}
	return nil
}

func ownerOr(ctx context.Context, port int) string {
	if o := PortOwner(ctx, port); o != "" {
		return o
	}
	return "something else on this machine"
}
