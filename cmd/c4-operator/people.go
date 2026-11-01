package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
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
// time; the people's sign-ins are a Secret there too until ORGANIZATION's
// Parties carry them.
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
	// the setting in force until an admin's signed act sets one.
	provider, clientID string
	// stepUpACR are acr values that mean multi-factor at the provider.
	stepUpACR []string
	// singlePerson is --single-person-farm, the owner before a binding.
	singlePerson string
	// installer is a one-person farm's installer token.
	installer []byte
}

// newPeopleIssuer makes the people's issuer, or nil when the install
// names no people's store.
func newPeopleIssuer(ctx context.Context, s *signing, kube kubernetes.Interface, reader client.Reader, signingNS string, o peopleOptions) (*people.Issuer, error) {
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
	owner := func(ctx context.Context) string {
		b := farmBinding(ctx, reader)
		if b == nil {
			return o.singlePerson
		}
		return b.Spec.SingleOwner()
	}
	iss := &people.Issuer{CA: s.ca,
		Index:    &people.Index{Farm: farmOf(s), Roots: s.pool, Store: shardStore{st}, Head: &secretHead{kube: kube, ns: signingNS}},
		Provider: people.Provider{Issuer: o.provider, ClientID: o.clientID}, StepUp: people.StepUp{ACR: o.stepUpACR},
		SignIns: &secretSignIns{kube: kube, ns: signingNS}, Owner: owner, InstallerToken: o.installer,
		// The farm owner's admins: before ORGANIZATION's Parties and their
		// purposes, the farm binding's owner.
		Admin: func(ctx context.Context, person string) bool {
			b := farmBinding(ctx, reader)
			return b != nil && b.Spec.Owner == person
		}}
	return iss, nil
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
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, peopleStoreSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return storeurl.Credentials{}, nil
	}
	if err != nil {
		return storeurl.Credentials{}, fmt.Errorf("the people's store credentials: %w", err)
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
// entry's C4 ID and position, written by resourceVersion.
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
	return people.HeadState{ID: string(sec.Data["head"]), Position: pos, Version: sec.ResourceVersion}, nil
}

func (h *secretHead) Write(ctx context.Context, from, to people.HeadState) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: peopleIndexSecret, Namespace: h.ns, ResourceVersion: from.Version},
		Data: map[string][]byte{"head": []byte(to.ID), "position": []byte(strconv.FormatInt(to.Position, 10))}}
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

// secretSignIns reads the people's sign-ins from the Secret c4-sign-ins:
// each key a person, each line of its value "<issuer URL> <subject ID>".
// It stands in for ORGANIZATION's Party signIns (O1), which aren't built;
// a cluster administrator writes it, as they install the farm.
type secretSignIns struct {
	kube kubernetes.Interface
	ns   string
}

func (s *secretSignIns) Person(ctx context.Context, issuer, subject string) (string, error) {
	sec, err := s.kube.CoreV1().Secrets(s.ns).Get(ctx, signInsSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return parseSignIns(sec.Data).Person(ctx, issuer, subject)
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
