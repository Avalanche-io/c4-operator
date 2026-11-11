package farmcli

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// sealedOwnerIssuer is newBookedOwnerIssuer with what a farm with a farm
// book has besides: the HUD's page of the book (GET /api/v1/book/farm, the
// entries sealed and those pending the seal) and the queue's sealer, which
// the test runs instead of a clock.
func sealedOwnerIssuer(t *testing.T) (*ownerIssuer, func()) {
	t.Helper()
	o := newBookedOwnerIssuer(t)
	book := o.index.Book
	queue, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := o.scratch.CA.Issue(pub, provenance.ComponentIdentity("queue"), 48*time.Hour, time.Now())
		return []*x509.Certificate{c, o.scratch.CA.Cert}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	sealer := &farmbook.Sealer{Farm: book.Farm, Roots: book.Roots, Store: book.Store, Pointer: book.Pointer, Own: book.Own, Signer: queue, Log: logr.Discard(),
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{people.NewBookChecker(book.Farm, book.Roots, book.Store),
				&farmbook.ControlChecker{Farm: book.Farm, Roots: book.Roots, Store: book.Store}}
		}}
	o.mux.HandleFunc("GET /api/v1/book/farm", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		v, err := book.View(r.Context(), time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(farmbook.NewPage(v, since, 100))
	})
	seal := func() {
		t.Helper()
		if _, err := sealer.Seal(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return o, seal
}

// up checks the owner it has just enrolled against the farm's own record
// of it, and on a farm with a farm book that record is sealed seconds
// after the enrolment: up waits for the seal, while entries wait for it,
// and then passes. Before, it read the book at once, found no owner
// sealed, and failed with "its index books none" on every fresh farm (2
// of 2 on a farm made that day, 3 of 3 on a scratch farm), passing only
// when run again.
func TestUpWaitsForTheBookToSealItsOwner(t *testing.T) {
	o, seal := sealedOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	type result struct {
		out *people.OwnerCertified
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := UpKeys(ctx, o.client, st, "noor", "", false)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("up finished before the book sealed its owner: %v", r.err)
	case <-time.After(time.Second):
	}
	seal()
	select {
	case r := <-done:
		if r.err != nil || r.out == nil || r.out.Owner == nil {
			t.Fatalf("up, once the book sealed its owner: %v %+v", r.err, r.out)
		}
		// up again, pinned, on the sealed book: at once
		start := time.Now()
		if _, err := UpKeys(ctx, o.client, st, "noor", r.out.Owner.Device, false); err != nil || time.Since(start) > time.Second {
			t.Fatalf("up again: %v, after %s", err, time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("up still waits after the book sealed its owner")
	}
}

// The wait is only for entries waiting for the seal. A farm whose book
// seals another owner is refused at once, and one that seals nothing in
// time is refused saying so, not as a farm with no owner.
func TestUpRefusesWhatTheBookSealsAndWhatItNeverSeals(t *testing.T) {
	ctx := context.Background()

	o, seal := sealedOwnerIssuer(t)
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	out, err := EnrolOwner(ctx, o.client, st, "noor")
	if err != nil {
		t.Fatal(err)
	}
	seal()
	last := "x"
	if strings.HasSuffix(out.Owner.Device, "x") {
		last = "y"
	}
	other := out.Owner.Device[:len(out.Owner.Device)-1] + last
	start := time.Now()
	if _, err := UpKeys(ctx, o.client, st, "noor", other, false); !errors.Is(err, ErrOwnerKey) || time.Since(start) > time.Second {
		t.Errorf("another owner key than the pinned one, sealed: %v, after %s", err, time.Since(start))
	}

	old := ownerSealWait
	t.Cleanup(func() { ownerSealWait = old })
	ownerSealWait = 600 * time.Millisecond
	o2, _ := sealedOwnerIssuer(t)
	st2 := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	start = time.Now()
	_, err = UpKeys(ctx, o2.client, st2, "noor", "", false)
	if !errors.Is(err, ErrOwnerKey) || !strings.Contains(err.Error(), "sealed no owner within") || time.Since(start) < ownerSealWait {
		t.Errorf("a book that never seals: %v, after %s", err, time.Since(start))
	}
}

// A device record that does not load is an error, never a farm that books
// no owner: "its index books none" once named a read that failed.
func TestTheOwnerCheckReportsARecordItCannotRead(t *testing.T) {
	ctx := context.Background()
	o, seal := sealedOwnerIssuer(t)
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	out, err := EnrolOwner(ctx, o.client, st, "noor")
	if err != nil {
		t.Fatal(err)
	}
	seal()
	o.mux.HandleFunc("/issuer/v1/people/records/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the store is not answering", http.StatusBadGateway)
	})
	err = UpKeysCheck(ctx, o, st, out.Owner.Device)
	if err == nil || strings.Contains(err.Error(), "books none") || !strings.Contains(err.Error(), "not answering") {
		t.Errorf("a record that does not load: %v", err)
	}
}

// UpKeysCheck is up's owner check alone, on o's farm.
func UpKeysCheck(ctx context.Context, o *ownerIssuer, st keystore.Store, pinned string) error {
	cfg, err := PeopleConfig(ctx, o.client)
	if err != nil {
		return err
	}
	return CheckOwner(ctx, o.client, st, cfg, pinned)
}

// With nothing waiting for the seal, there is nothing to wait for: a farm
// whose book holds no owner and no pending entry is refused at once, not
// after ownerSealWait. Here the machine has no certificate of its own, so
// it reads no owner from a book that holds nothing.
func TestTheOwnerCheckWaitsOnlyForWhatIsPending(t *testing.T) {
	ctx := context.Background()
	o, _ := sealedOwnerIssuer(t)
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	start := time.Now()
	err := UpKeysCheck(ctx, o, st, "c4notthisfarmsowner")
	if !errors.Is(err, ErrOwnerKey) || !strings.Contains(err.Error(), "books none") || time.Since(start) > time.Second {
		t.Errorf("no owner and nothing pending: %v, after %s", err, time.Since(start))
	}
}
