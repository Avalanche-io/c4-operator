package localfarm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A node's ports are read as docker inspect gives them: the HUD's and the
// store's NodePorts mapped to the host ports the farm was made with.
func TestParsePorts(t *testing.T) {
	out := `{"30080/tcp":[{"HostIp":"127.0.0.1","HostPort":"30180"}],"30900/tcp":[{"HostIp":"127.0.0.1","HostPort":"30910"}],"6443/tcp":[{"HostIp":"127.0.0.1","HostPort":"52198"}]}`
	hud, store, err := parsePorts(out)
	if err != nil || hud != 30180 || store != 30910 {
		t.Errorf("%d %d %v", hud, store, err)
	}
	if hud, _, _ := parsePorts(`{}`); hud != 0 {
		t.Errorf("no mapping: %d", hud)
	}
}

// A new farm whose default port another farm holds takes the next free
// one and says so; a port given is kept as given (up refuses it if taken).
func TestResolvePortsForANewFarm(t *testing.T) {
	defer func(f func(int) bool) { free = f }(free)
	taken := map[int]bool{30080: true, 30180: true, 30900: true}
	free = func(p int) bool { return !taken[p] }
	var said []string
	say := func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }

	o := Options{Name: "c4-walk"}
	if err := ResolvePorts(context.Background(), &o, false, say); err != nil {
		t.Fatal(err)
	}
	if o.HUDHostPort != 30280 || o.StoreHostPort != 31000 {
		t.Errorf("ports %d %d", o.HUDHostPort, o.StoreHostPort)
	}
	if len(said) != 2 || !strings.HasPrefix(said[0], "ports     the HUD at 127.0.0.1:30280, since 30080 is in use by ") {
		t.Errorf("said %q", said)
	}

	said = nil
	o = Options{Name: "c4-walk", HUDHostPort: 30080, StoreHostPort: 30950}
	if err := ResolvePorts(context.Background(), &o, false, say); err != nil || o.HUDHostPort != 30080 || len(said) != 0 {
		t.Errorf("given ports: %d %v %q", o.HUDHostPort, err, said)
	}
	free = func(int) bool { return true }
	o = Options{Name: "c4-walk"}
	if ResolvePorts(context.Background(), &o, false, say); o.HUDHostPort != 0 || o.HUD() != "http://127.0.0.1:30080" {
		t.Errorf("defaults free: %d %s", o.HUDHostPort, o.HUD())
	}
}
