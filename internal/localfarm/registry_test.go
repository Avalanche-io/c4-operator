package localfarm

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

// A port something else holds is refused before kind is asked, with the
// flag that moves the farm's.
func TestPortsFreeNamesTheFlag(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	err = portsFree(context.Background(), []farmPort{{"the HUD", port, "--port"}})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("port %d, for the HUD, is in use", port)) || !strings.HasSuffix(err.Error(), "with --port") {
		t.Errorf("err %v", err)
	}
	l.Close()
	if err := portsFree(context.Background(), []farmPort{{"the HUD", port, "--port"}}); err != nil {
		t.Errorf("a free port: %v", err)
	}
}
