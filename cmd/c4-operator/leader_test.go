package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// What only one manager may run (the queue says so; a runnable that says
// nothing is the same) never runs in two managers at once, with the
// options the manager is built with: not while both start, not while the
// leader runs, and not across a clean handover, which takes the next
// manager seconds, not the lease's 15 s. The HUD's listener is served by
// both, leader or not, since a manager is in the Service before it leads.
func TestTwoManagersNeverRunTheQueueAtOnce(t *testing.T) {
	if !(&queue.Queue{}).NeedLeaderElection() {
		t.Fatal("the queue no longer asks for leader election")
	}
	cfg := startEnvtest(t)
	const ns = "c4-system"
	kube := kubernetes.NewForConfigOrDie(cfg)
	if _, err := kube.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	running, most := 0, 0
	type span struct{ start, end time.Time }
	spans := map[string]*span{}
	hud := map[string]string{}
	start := func(name string) (cancel func(), done <-chan struct{}) {
		mgr, err := ctrl.NewManager(cfg, electLeader(ctrl.Options{
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		}, ns))
		if err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		hud[name] = l.Addr().String()
		l.Close()
		if err := mgr.Add(hudListener(hud[name], http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))); err != nil {
			t.Fatal(err)
		}
		// A stand-in for the queue: like every runnable that does not say
		// otherwise, it needs the lease.
		err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			mu.Lock()
			running++
			most = max(most, running)
			spans[name] = &span{start: time.Now()}
			mu.Unlock()
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond) // a queue finishing its tick
			mu.Lock()
			running--
			spans[name].end = time.Now()
			mu.Unlock()
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			if err := mgr.Start(ctx); err != nil {
				t.Errorf("manager %s: %v", name, err)
			}
		}()
		return cancel, ch
	}
	cancelA, doneA := start("a")
	cancelB, doneB := start("b")
	t.Cleanup(func() { cancelA(); cancelB(); <-doneA; <-doneB })

	leader := func(d time.Duration) string {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			mu.Lock()
			for name, s := range spans {
				if s.end.IsZero() {
					mu.Unlock()
					return name
				}
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
		return ""
	}
	first := leader(20 * time.Second)
	if first == "" {
		t.Fatal("neither manager took the lease")
	}
	time.Sleep(3 * time.Second) // both running, one leading
	for name, addr := range hud {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("manager %s (leading: %v) does not serve the HUD: %v", name, name == first, err)
		}
		resp.Body.Close()
	}
	stopped := time.Now()
	if first == "a" {
		cancelA()
		<-doneA
	} else {
		cancelB()
		<-doneB
	}
	second := leader(10 * time.Second)
	took := time.Since(stopped)
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Fatalf("the queue ran in %d managers at once", most)
	}
	if second == "" || second == first {
		t.Fatalf("after %s stopped cleanly, no other manager led within 10 s (%v)", first, second)
	}
	if s1, s2 := spans[first], spans[second]; !s2.start.After(s1.end) {
		t.Fatalf("%s started its queue at %s, before %s's ended at %s", second, s2.start.Format(time.StampMilli), first, s1.end.Format(time.StampMilli))
	}
	t.Logf("%s led; it stopped and %s led %s later", first, second, took.Round(100*time.Millisecond))
	if took > 8*time.Second {
		t.Errorf("a clean handover took %s: is the lease still released on cancel?", took)
	}
}

func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			t.Skip("envtest binaries not found; run `make envtest`")
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		t.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	return cfg
}

// Every manager this binary builds takes the lease: each ctrl.NewManager
// call is given its options through electLeader.
func TestEveryManagerIsElected(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewManager" {
				return true
			}
			calls++
			if len(call.Args) == 2 {
				if inner, ok := call.Args[1].(*ast.CallExpr); ok {
					if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "electLeader" {
						return true
					}
				}
			}
			t.Errorf("%s: a manager built without electLeader", fset.Position(call.Pos()))
			return true
		})
	}
	if calls == 0 {
		t.Fatal("found no ctrl.NewManager call: the test no longer sees the manager")
	}
}
