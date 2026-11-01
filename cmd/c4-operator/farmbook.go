package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// The farm book (internal/farmbook, DESIGN.md section 9.2), in the
// installation's system store (--system-store). It runs only where the
// install names that store, so no existing farm changes. Its entries,
// segments and the records they cite go to the store by C4 ID; its State
// is the ConfigMap c4-farm-book in the operator's namespace; the store's
// credentials are the Secret c4-system-store in the signing namespace,
// which only the manager reads, as the people's store's are. The queue
// seals it; the HUD and the people's issuer write it.
const systemStoreSecret = "c4-system-store"

// farmBook is the farm book's parts in the manager.
type farmBook struct {
	store  farmbook.Store
	ptr    farmbook.Pointer
	reader *farmbook.Reader
	sealer *farmbook.Sealer
}

// checkFarmBookFlags checks the farm book's settings: a seal interval
// within its bound, and a signing CA, since every segment is signed.
func checkFarmBookFlags(store string, every time.Duration, signing bool) error {
	if store == "" {
		return nil
	}
	if !signing {
		return errors.New("--system-store: the farm book's segments are signed, and --signing-namespace is empty")
	}
	if err := farmbook.CheckEvery(every); err != nil {
		return fmt.Errorf("--farm-book-seal: %w", err)
	}
	return nil
}

// newFarmBook opens the system store and makes the book's reader and
// sealer, or nil when the install names no system store. The sealer
// checks the HUD's control entries and the people's issuer's entries;
// legacy, when set, is a people's index kept before the book, which the
// first seal adopts.
func newFarmBook(ctx context.Context, s *signing, kube kubernetes.Interface, mgr ctrl.Manager, signingNS, namespace, store string, every time.Duration) (*farmBook, error) {
	if store == "" || s.ca == nil {
		return nil, nil
	}
	creds, err := storeCredentials(ctx, kube, signingNS, systemStoreSecret)
	if err != nil {
		return nil, err
	}
	var specs []storeurl.Spec
	for _, sh := range splitList(store) {
		specs = append(specs, storeurl.ParseSpec(sh))
	}
	st, err := storeurl.OpenSharded(specs, creds)
	if err != nil {
		return nil, fmt.Errorf("--system-store: %w", err)
	}
	farm := farmOf(s)
	b := &farmBook{store: shardStore{st}}
	b.ptr = &farmbook.ConfigMapPointer{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: namespace}
	b.reader = &farmbook.Reader{Farm: farm, Roots: s.pool, Store: b.store, Pointer: b.ptr}
	b.sealer = &farmbook.Sealer{Farm: farm, Roots: s.pool, Store: b.store, Pointer: b.ptr, Signer: s.queue, Every: every,
		Log: ctrl.Log.WithName("farm-book"),
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{&farmbook.ControlChecker{Farm: farm, Roots: s.pool, Store: b.store},
				people.NewBookChecker(farm, s.pool, b.store)}
		}}
	return b, nil
}

// writer is the HUD's way into the book, or nil.
func (b *farmBook) writer() *farmbook.Writer {
	if b == nil {
		return nil
	}
	return &farmbook.Writer{Store: b.store, Pointer: b.ptr}
}

// storeCredentials reads an object store's credentials, the Secret name in
// the signing namespace, when there is one.
func storeCredentials(ctx context.Context, kube kubernetes.Interface, ns, name string) (storeurl.Credentials, error) {
	c, err := secretCredentials(ctx, kube, ns, name)
	if err != nil {
		return storeurl.Credentials{}, fmt.Errorf("the %s credentials: %w", name, err)
	}
	return c, nil
}
