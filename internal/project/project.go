// Package project maps namespaces to Projects and opens their stores.
//
// Projects are cluster-scoped and few, so every component watches all of
// them. Store credentials live in Secrets in the operator's namespace and
// are read on demand, only by components that need them (the manager, the
// regional tier, and node daemons for Direct projects).
package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// NamespaceIndex is the field index of Projects by namespace.
const NamespaceIndex = "spec.namespaces"

// IndexNamespaces is the indexer function for NamespaceIndex.
func IndexNamespaces(obj client.Object) []string {
	p, ok := obj.(*c4v1.Project)
	if !ok {
		return nil
	}
	return p.Spec.Namespaces
}

// ErrNoProject means a namespace belongs to no Project.
var ErrNoProject = errors.New("namespace belongs to no Project")

// Registry answers project questions from a watch cache.
type Registry struct {
	// Cache is a client.Reader backed by an informer on Projects with the
	// NamespaceIndex field index.
	Cache client.Reader
	// Secrets reads Secrets in Namespace directly (nil for components
	// that must not hold store credentials).
	Secrets   client.Reader
	Namespace string

	mu     sync.Mutex
	stores map[string]storeEntry
}

type storeEntry struct {
	key   string // project generation and secret version
	store *storeurl.Sharded
}

// ForNamespace returns the Project that lists ns.
func (r *Registry) ForNamespace(ctx context.Context, ns string) (*c4v1.Project, error) {
	var list c4v1.ProjectList
	if err := r.Cache.List(ctx, &list, client.MatchingFields{NamespaceIndex: ns}); err != nil {
		return nil, err
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrNoProject, ns)
	case 1:
		return &list.Items[0], nil
	}
	return nil, fmt.Errorf("namespace %s is listed by %d Projects; a namespace belongs to at most one", ns, len(list.Items))
}

// Get returns a Project by name.
func (r *Registry) Get(ctx context.Context, name string) (*c4v1.Project, error) {
	var p c4v1.Project
	if err := r.Cache.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// List returns every Project.
func (r *Registry) List(ctx context.Context) ([]c4v1.Project, error) {
	var list c4v1.ProjectList
	if err := r.Cache.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// VerifiesFillsFor is VerifiesFills for the project named, and yes when the
// project can't be looked up: what a tier server asks before a fill.
func (r *Registry) VerifiesFillsFor(ctx context.Context, name string) bool {
	p, err := r.Get(ctx, name)
	return err != nil || r.VerifiesFills(ctx, p)
}

// VerifiesFills reports whether fills for p hash what they fetch. They do
// unless p says not to (spec.store.verifyFill: false), and then still do
// when p shares its partition (Shared isolation) with a project that
// verifies: projects of one partition share their fills, so a project that
// verifies must not join a fill one that doesn't started. When the
// projects can't be listed, it says yes.
func (r *Registry) VerifiesFills(ctx context.Context, p *c4v1.Project) bool {
	if p.Spec.Store.VerifiesFills() {
		return true
	}
	if p.Spec.Isolation != c4v1.IsolationShared || p.Spec.SharingGroup == "" {
		return false // a partition of its own
	}
	part := p.Partition()
	all, err := r.List(ctx)
	if err != nil {
		return true
	}
	for i := range all {
		if all[i].Partition() == part && all[i].Spec.Store.VerifiesFills() {
			return true
		}
	}
	return false
}

// Store opens (or returns the cached) sharded store of p with its
// credentials.
func (r *Registry) Store(ctx context.Context, p *c4v1.Project) (*storeurl.Sharded, error) {
	creds, version, err := r.credentials(ctx, p)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%d/%s", p.Generation, version)
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.stores[p.Name]; ok && e.key == key {
		return e.store, nil
	}
	specs := make([]storeurl.Spec, 0, len(p.Spec.Store.Shards))
	for _, sh := range p.Spec.Store.Shards {
		specs = append(specs, storeurl.Spec{Name: sh.Name, URL: sh.URL})
	}
	st, err := storeurl.OpenSharded(specs, creds)
	if err != nil {
		return nil, fmt.Errorf("project %s store: %w", p.Name, err)
	}
	if r.stores == nil {
		r.stores = map[string]storeEntry{}
	}
	r.stores[p.Name] = storeEntry{key: key, store: st}
	return st, nil
}

func (r *Registry) credentials(ctx context.Context, p *c4v1.Project) (storeurl.Credentials, string, error) {
	ref := p.Spec.Store.CredentialsSecretRef
	if ref == nil {
		return storeurl.Credentials{}, "", nil
	}
	if r.Secrets == nil {
		return storeurl.Credentials{}, "", fmt.Errorf("this component holds no store credentials (project %s)", p.Name)
	}
	var sec corev1.Secret
	if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: ref.Name}, &sec); err != nil {
		return storeurl.Credentials{}, "", fmt.Errorf("project %s credentials %s/%s: %w", p.Name, r.Namespace, ref.Name, err)
	}
	return storeurl.Credentials{
		AccessKeyID:     firstKey(sec.Data, "AWS_ACCESS_KEY_ID", "access-key-id"),
		SecretAccessKey: firstKey(sec.Data, "AWS_SECRET_ACCESS_KEY", "secret-access-key"),
	}, sec.ResourceVersion, nil
}

func firstKey(data map[string][]byte, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok {
			return string(bytes.TrimSpace(v))
		}
	}
	return ""
}

// Current is the ID the view id has now in project name's store, for a
// reader given the ID from outside what it wrote (a request, a spec, a
// record's subject): an ID a c4 library change renamed resolves through
// the renames table the Project's status names (view.ResolveOld), and any
// other ID is itself. When the Project or its table cannot be read it is
// id too, and the load that follows says what it cannot find.
func Current(ctx context.Context, c client.Reader, st view.Store, name string, id c4.ID) c4.ID {
	var p c4v1.Project
	if c == nil || st == nil || c.Get(ctx, types.NamespacedName{Name: name}, &p) != nil || p.Status.Renames == "" {
		return id
	}
	table, err := c4.Parse(p.Status.Renames)
	if err != nil {
		return id
	}
	cur, err := view.ResolveOld(ctx, st, table, id)
	if err != nil {
		return id
	}
	return cur
}
