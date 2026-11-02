package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/hud/server"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// The people's issuer (PEOPLE-KEYS-2 3, internal/provenance/people), beside
// the workload issuer on the same listener. It runs only where the
// install names its store (--people-store), so no existing farm changes.
// Its records and index entries go to that object store by C4 ID; the
// index's head is a Secret in the signing namespace, which only the
// manager reads, written by resourceVersion so replicas append one at a
// time. The people's sign-ins are the farm's Parties' (partySignIns), and
// a Secret there on a farm with no Parties yet.
const (
	peopleIndexSecret = "c4-people-index"
	peopleStoreSecret = "c4-people-store"
	signInsSecret     = "c4-sign-ins"
)

// peopleOptions are the install's settings for the people's issuer.
type peopleOptions struct {
	// store is the people's store, shards as name=url, comma-separated.
	store string
	// provider and clientID are the install's sign-in provider, booked as
	// the setting in force; until K9 it changes only by redeploying.
	provider, clientID string
	// stepUpACR are acr values that mean multi-factor at the provider.
	stepUpACR []string
	// singlePerson is --single-person-farm, the owner before a binding.
	singlePerson string
	// installer is a one-person farm's installer token.
	installer []byte
	// hudOrigin is the HUD's origin, where a browser's passkey touch key
	// is made and signs (--hud-origin); "" takes no passkeys.
	hudOrigin string
	// softwareTouchKeys lets a touch key state a software protection
	// (--software-touch-keys): a test or file-key farm.
	softwareTouchKeys bool
}

// newPeopleIssuer makes the people's issuer, or nil when the install
// names no people's store.
//
// With a farm book, the index is kept in the book: its records go to the
// system store, and the people's store holds only the index kept before
// the book, which the book's first seal adopts (PEOPLE-KEYS-2 10.4 item
// 11).
func newPeopleIssuer(ctx context.Context, s *signing, kube kubernetes.Interface, reader client.Reader, signingNS string, o peopleOptions, book *farmBook) (*people.Issuer, error) {
	if o.store == "" || s.ca == nil {
		return nil, nil
	}
	if (o.provider == "") != (o.clientID == "") {
		return nil, errors.New("--sign-in-provider and --sign-in-client-id go together")
	}
	creds, err := peopleCredentials(ctx, kube, signingNS)
	if err != nil {
		return nil, err
	}
	var specs []storeurl.Spec
	for _, sh := range splitList(o.store) {
		specs = append(specs, storeurl.ParseSpec(sh))
	}
	st, err := storeurl.OpenSharded(specs, creds)
	if err != nil {
		return nil, fmt.Errorf("--people-store: %w", err)
	}
	rp, err := passkeyRP(o.hudOrigin)
	if err != nil {
		return nil, err
	}
	owner := func(ctx context.Context) string {
		b := farmBinding(ctx, reader)
		if b == nil {
			return o.singlePerson
		}
		return b.Spec.SingleOwner()
	}
	index := &people.Index{Farm: farmOf(s), Roots: s.pool, Store: shardStore{st}, Head: &secretHead{kube: kube, ns: signingNS}}
	if book != nil {
		book.sealer.Adopt = index.Adoption
		index = &people.Index{Farm: farmOf(s), Roots: s.pool, Store: book.store, Head: index.Head, Book: book.reader}
	}
	iss := &people.Issuer{CA: s.ca, Bind: s.bind, Stale: s.newStale(),
		Index:    index,
		Provider: people.Provider{Issuer: o.provider, ClientID: o.clientID}, StepUp: people.StepUp{ACR: o.stepUpACR}, WebAuthn: rp,
		SignIns: &partySignIns{reader: reader, secret: &secretSignIns{kube: kube, ns: signingNS}}, Owner: owner, InstallerToken: o.installer, SoftwareTouchKeys: o.softwareTouchKeys,
		// The farm owner's admins: the farm binding's owner before
		// ORGANIZATION's Parties, then the farm owner's Party's admins.
		Admin: farmAdmins(reader, book)}
	// The index holds a booked entry to the issuer's own rules, an admin's
	// revoke included: a Party admin's revoke is sealed by the same rule.
	index.Admin = iss.Admin
	return iss, nil
}

// passkeyRP is the relying party passkeys are made for: the HUD's origin,
// and its host as the relying party's ID. An origin is https, or
// http://localhost, the only plain-HTTP origin a browser makes passkeys at.
func passkeyRP(origin string) (*people.WebAuthnRP, error) {
	if origin == "" {
		return nil, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil ||
		!(u.Scheme == "https" || u.Scheme == "http" && u.Hostname() == "localhost") {
		return nil, fmt.Errorf("--hud-origin %q is not an origin: write https://<the HUD's host>, or http://localhost:<port>", origin)
	}
	return &people.WebAuthnRP{ID: strings.ToLower(u.Hostname()), Origin: u.Scheme + "://" + strings.ToLower(u.Host)}, nil
}

// farmAdmin reports whether a person is one of the farm owner's admins:
// before ORGANIZATION's Parties and their purposes, the farm binding's
// owner.
func farmAdmin(reader client.Reader) func(ctx context.Context, person string) bool {
	return func(ctx context.Context, person string) bool {
		b := farmBinding(ctx, reader)
		return b != nil && b.Spec.Owner == person
	}
}

func farmOf(s *signing) string { return provenance.FarmID(s.ca.Cert) }

// farmBinding is the farm binding, or nil.
func farmBinding(ctx context.Context, reader client.Reader) *c4v1.DeploymentBinding {
	var b c4v1.DeploymentBinding
	if err := reader.Get(ctx, types.NamespacedName{Name: c4v1.FarmBindingName}, &b); err != nil {
		return nil
	}
	return &b
}

// peopleCredentials reads the people's store's credentials, the Secret
// c4-people-store in the signing namespace, when there is one.
func peopleCredentials(ctx context.Context, kube kubernetes.Interface, ns string) (storeurl.Credentials, error) {
	c, err := secretCredentials(ctx, kube, ns, peopleStoreSecret)
	if err != nil {
		return storeurl.Credentials{}, fmt.Errorf("the people's store credentials: %w", err)
	}
	return c, nil
}

// secretCredentials reads an object store's credentials from the Secret
// name in ns, or none when there is no such Secret.
func secretCredentials(ctx context.Context, kube kubernetes.Interface, ns, name string) (storeurl.Credentials, error) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return storeurl.Credentials{}, nil
	}
	if err != nil {
		return storeurl.Credentials{}, err
	}
	return storeurl.Credentials{AccessKeyID: string(sec.Data["AWS_ACCESS_KEY_ID"]), SecretAccessKey: string(sec.Data["AWS_SECRET_ACCESS_KEY"])}, nil
}

// shardStore keeps the people's records in an object store by C4 ID.
type shardStore struct{ s *storeurl.Sharded }

func (s shardStore) Put(ctx context.Context, id string, data []byte) error {
	got, err := s.s.PutBytes(ctx, data)
	if err == nil && got.String() != id {
		return fmt.Errorf("record %.16s hashes to %.16s", id, got)
	}
	return err
}

func (s shardStore) Get(ctx context.Context, id string) ([]byte, error) {
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a C4 ID", people.ErrNotFound, id)
	}
	body, _, _, err := s.s.For(cid).Get(ctx, cid, 0)
	if errors.Is(err, storeurl.ErrNotFound) {
		return nil, fmt.Errorf("%w: %.16s", people.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 1<<20))
}

// secretHead is the index's head: the Secret c4-people-index, its last
// entry's C4 ID and position and the issuer's signature over them,
// written by resourceVersion.
type secretHead struct {
	kube kubernetes.Interface
	ns   string
}

func (h *secretHead) Read(ctx context.Context) (people.HeadState, error) {
	sec, err := h.kube.CoreV1().Secrets(h.ns).Get(ctx, peopleIndexSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return people.HeadState{}, nil
	}
	if err != nil {
		return people.HeadState{}, err
	}
	pos, err := strconv.ParseInt(string(sec.Data["position"]), 10, 64)
	if err != nil {
		return people.HeadState{}, fmt.Errorf("%s: position: %w", peopleIndexSecret, err)
	}
	return people.HeadState{ID: string(sec.Data["head"]), Position: pos, Signed: sec.Data["signed"], Version: sec.ResourceVersion}, nil
}

func (h *secretHead) Write(ctx context.Context, from, to people.HeadState) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: peopleIndexSecret, Namespace: h.ns, ResourceVersion: from.Version},
		Data: map[string][]byte{"head": []byte(to.ID), "position": []byte(strconv.FormatInt(to.Position, 10)), "signed": to.Signed}}
	var err error
	if from.Version == "" {
		_, err = h.kube.CoreV1().Secrets(h.ns).Create(ctx, sec, metav1.CreateOptions{})
	} else {
		_, err = h.kube.CoreV1().Secrets(h.ns).Update(ctx, sec, metav1.UpdateOptions{})
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return people.ErrMoved
	}
	return err
}

// farmParties lists the farm's Parties (ORGANIZATION 1): none on a farm
// without the kind installed.
func farmParties(ctx context.Context, reader client.Reader) ([]c4v1.Party, error) {
	var l c4v1.PartyList
	if err := reader.List(ctx, &l); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, nil
		}
		return nil, err
	}
	return l.Items, nil
}

// partySignIns are the people's sign-ins (PEOPLE-KEYS-2 3.1), person by
// person: a person who has a person Party signs in by that Party's
// signIns that name a provider, and nothing else; a person who has none
// yet keeps K2's stand-in, their lines in the c4-sign-ins Secret, which so
// drops out as people get Parties (O2 makes them at up). A subject both
// sources give, to two people, is held by both, so it signs in neither
// (one subject, one person: holder, 409). Parties that can't be read
// sign no one in.
type partySignIns struct {
	reader client.Reader
	secret people.SignIns
}

func (s *partySignIns) Load(ctx context.Context) (people.StaticSignIns, error) {
	ps, err := farmParties(ctx, s.reader)
	if err != nil {
		return nil, fmt.Errorf("the farm's parties: %w", err)
	}
	stand, err := s.secret.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := people.StaticSignIns{}
	hasParty := map[string]bool{}
	for _, p := range ps {
		if p.Spec.Kind != c4v1.PartyPerson {
			continue
		}
		hasParty[p.Name] = true
		for _, x := range p.Spec.SignIns {
			if x.Issuer != "" {
				out[p.Name] = append(out[p.Name], people.SignIn{Issuer: x.Issuer, Subject: x.Subject})
			}
		}
	}
	for person, ins := range stand {
		if !hasParty[person] {
			out[person] = append(out[person], ins...)
		}
	}
	return out, nil
}

// farmAdmins decides who the farm owner's admins are, who may remove
// anyone's devices and read others' ID tokens and addresses (Issuer.Sees,
// the farm book's private parts):
//   - once the farm owner's Party (the binding owner's name) exists and its
//     make-party is in the farm book, a person holding admin by name in it,
//     in force in the book now; for a person's Party, that person;
//   - until then, whatever other Parties exist, the farm binding's owner,
//     as before Parties.
//
// Parties or a book that can't be read make no one an admin.
func farmAdmins(reader client.Reader, book *farmBook) func(ctx context.Context, person string) bool {
	return func(ctx context.Context, person string) bool {
		ps, err := farmParties(ctx, reader)
		b := farmBinding(ctx, reader)
		if err != nil || b == nil || person == "" {
			return false
		}
		var owner *c4v1.Party
		for i := range ps {
			if ps[i].Name == b.Spec.Owner {
				owner = &ps[i]
			}
		}
		if owner == nil || book == nil || book.reader == nil {
			return b.Spec.Owner == person
		}
		view, err := book.reader.View(ctx, time.Now())
		if err != nil {
			return false
		}
		if on, err := view.InForceAt(farmbook.ForceParty, farmbook.PartyKey(owner.Name), view.Position(), view.Now); err != nil {
			return false
		} else if on == nil {
			// made, its record not booked yet: the binding's owner still
			return b.Spec.Owner == person
		}
		if owner.Spec.Kind == c4v1.PartyPerson {
			return owner.Name == person
		}
		ok, _ := farmbook.HoldsByName(ctx, view, book.store, book.roots, owner.Name, person, server.PurposeAdmin)
		return ok
	}
}

// secretSignIns reads the people's sign-ins from the Secret c4-sign-ins:
// each key a person, each line of its value "<issuer URL> <subject ID>".
// It stands in for ORGANIZATION's Party signIns on a farm with no Parties
// yet (partySignIns); a cluster administrator writes it, as they install
// the farm.
type secretSignIns struct {
	kube kubernetes.Interface
	ns   string
}

func (s *secretSignIns) Load(ctx context.Context) (people.StaticSignIns, error) {
	sec, err := s.kube.CoreV1().Secrets(s.ns).Get(ctx, signInsSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return people.StaticSignIns{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSignIns(sec.Data), nil
}

// parseSignIns reads c4-sign-ins' data.
func parseSignIns(data map[string][]byte) people.StaticSignIns {
	out := people.StaticSignIns{}
	for person, v := range data {
		sc := bufio.NewScanner(bytes.NewReader(v))
		for sc.Scan() {
			iss, sub, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
			if ok && strings.TrimSpace(sub) != "" {
				out[person] = append(out[person], people.SignIn{Issuer: iss, Subject: strings.TrimSpace(sub)})
			}
		}
	}
	return out
}
