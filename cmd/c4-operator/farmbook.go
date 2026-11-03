package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// The farm book (internal/farmbook, DESIGN.md section 9.2), in the
// installation's system store (--system-store). It runs only where the
// install names that store, so no existing farm changes. Its entries,
// segments and the records they cite go to the store by C4 ID. Its State
// (c4-farm-book), the sealer's own head (c4-farm-book-sealer) and the
// store's credentials (c4-system-store) are Secrets in the signing
// namespace, which only the manager reads and writes, by name. The queue
// seals it; the HUD and the people's issuer write it.
const systemStoreSecret = "c4-system-store"

// farmBook is the farm book's parts in the manager.
type farmBook struct {
	farm   string
	roots  *x509.CertPool
	store  farmbook.Store
	ptr    farmbook.Pointer
	own    farmbook.SealerStore
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
func newFarmBook(ctx context.Context, s *signing, kube kubernetes.Interface, mgr ctrl.Manager, signingNS, store string, every time.Duration) (*farmBook, error) {
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
	b := &farmBook{store: shardStore{st}, farm: farm, roots: s.pool}
	// The manager's client reads Secrets uncached.
	b.ptr = &farmbook.SecretPointer{Client: mgr.GetClient(), Namespace: signingNS}
	b.own = &farmbook.SecretSealer{Client: mgr.GetClient(), Namespace: signingNS}
	b.reader = b.newReader()
	b.sealer = &farmbook.Sealer{Farm: farm, Roots: s.pool, Store: b.store, Pointer: b.ptr, Own: b.own, Signer: s.queue, Every: every,
		Log:      ctrl.Log.WithName("farm-book"),
		Checkers: func() []farmbook.Checker { return b.checkers(mgr.GetAPIReader()) }}
	return b, nil
}

// checkers are the seal's checkers: the control records', and the people's
// rules with the farm owner's admins as the people's issuer has them, so an
// admin's act the issuer takes, a Party admin's revoke included, the seal
// takes too.
func (b *farmBook) checkers(reader client.Reader) []farmbook.Checker {
	return []farmbook.Checker{&farmbook.ControlChecker{Farm: b.farm, Roots: b.roots, Store: b.store},
		people.NewBookChecker(b.farm, b.roots, b.store).WithAdmin(b.sealAdmin(reader))}
}

// sealAdmin is who the seal takes for the farm owner's admins: the people's
// issuer's own (farmAdmins), with Parties the farm owner's Party's admins.
func (b *farmBook) sealAdmin(reader client.Reader) func(ctx context.Context, person string) bool {
	return farmAdmins(reader, b)
}

// newReader is a reader of the book in the manager: it reads the sealed
// book to the sealer's own head, so a State rolled back never takes it
// back, across its restarts too.
func (b *farmBook) newReader() *farmbook.Reader {
	return &farmbook.Reader{Farm: b.farm, Roots: b.roots, Store: b.store, Pointer: b.ptr, Own: b.own}
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
