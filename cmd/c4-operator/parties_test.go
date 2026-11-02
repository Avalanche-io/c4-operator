package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

const sso = "https://sso.studio.example"

// issuerFarm is what the people's issuer reads: the farm binding, the
// farm's Parties, the c4-sign-ins Secret (counting its reads) and a farm
// book.
type issuerFarm struct {
	reader  client.Reader
	kube    *kfake.Clientset
	secrets *int
	book    *farmBook
	writer  *farmbook.Writer
	f       *provtest.Farm
}

func newIssuerFarm(t *testing.T, owner string, parties ...*c4v1.Party) *issuerFarm {
	t.Helper()
	scheme := runtime.NewScheme()
	c4v1.AddToScheme(scheme)
	objs := []client.Object{&c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: owner, To: "laptop-1/*", For: []string{"task/v1"}, By: owner}}}
	for _, p := range parties {
		objs = append(objs, p)
	}
	kube := kfake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-signing", Name: signInsSecret},
		Data: map[string][]byte{"noor": []byte(sso + " 00u-noor\n"), "lee": []byte(sso + " 00u-lee\n")}})
	n := 0
	kube.PrependReactor("get", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.GetAction).GetName() == signInsSecret {
			n++
		}
		return false, nil, nil
	})
	f := provtest.NewFarm(t, time.Now())
	store, ptr := &farmbook.MemoryStore{}, &farmbook.MemoryPointer{}
	return &issuerFarm{reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), kube: kube, secrets: &n,
		book:   &farmBook{farm: f.ID, roots: f.Roots, store: store, ptr: ptr, reader: &farmbook.Reader{Farm: f.ID, Roots: f.Roots, Store: store, Pointer: ptr, Own: &farmbook.MemorySealer{}}},
		writer: &farmbook.Writer{Store: store, Pointer: ptr}, f: f}
}

func (r *issuerFarm) signIns(t *testing.T) people.StaticSignIns {
	t.Helper()
	ins, err := (&partySignIns{reader: r.reader, secret: &secretSignIns{kube: r.kube, ns: "c4-signing"}}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ins
}

// book books a control record the HUD signs, as the HUD does: its record
// in the book's store, its entry pending the seal.
func (r *issuerFarm) bookControl(t *testing.T, action, target string, change map[string]any) {
	t.Helper()
	r.bookUnder(t, r.f.CA, action, target, change)
}

// bookUnder books a control record a HUD under ca signs.
func (r *issuerFarm) bookUnder(t *testing.T, ca *provenance.CA, action, target string, change map[string]any) {
	t.Helper()
	ctx := context.Background()
	hud, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pred := map[string]any{"actor": "ines", "action": action, "target": target, "change": change}
	rec, err := provenance.SignStatement(ctx, hud, provenance.PredicateControl, []provenance.Subject{{Name: target, Digest: map[string]string{"sha256": "00"}}}, pred, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := farmbook.ControlEntry(rec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.book.store.Put(ctx, e.Record, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.writer.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
		t.Fatal(err)
	}
}

func person(name string, ins ...c4v1.PartySignIn) *c4v1.Party {
	return &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.PartySpec{Kind: c4v1.PartyPerson, DisplayName: name, SignIns: ins,
		Made: c4v1.PartyAct{By: "ines", At: metav1.Now()}}}
}

func organisation(name string) *c4v1.Party {
	return &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.PartySpec{Kind: c4v1.PartyOrganisation, DisplayName: name,
		Made: c4v1.PartyAct{By: "ines", At: metav1.Now()}}}
}

// Before Parties, the people's issuer reads K2's stand-ins as it always
// has: the c4-sign-ins Secret, and the farm binding's owner as its admin.
func TestIssuerBeforeParties(t *testing.T) {
	r := newIssuerFarm(t, "noor")
	ins := r.signIns(t)
	if *r.secrets != 1 || ins.Person(sso, "00u-noor") != "noor" {
		t.Errorf("the Secret is not read before Parties: %d reads, %v", *r.secrets, ins)
	}
	admin := farmAdmins(r.reader, r.book)
	if !admin(context.Background(), "noor") || admin(context.Background(), "lee") {
		t.Error("before Parties, the farm binding's owner is not the one admin")
	}
}

// With Parties, the issuer reads nothing of K2's stand-ins: the sign-ins
// are the person Parties' provider subjects, and the c4-sign-ins Secret
// is never read; the admins are those holding admin by name in the farm
// owner's Party, in force in the book, and never the binding's owner by
// that alone.
func TestIssuerReadsParties(t *testing.T) {
	ctx := context.Background()
	// the farm bound to a person, who has a Party
	r := newIssuerFarm(t, "noor", person("noor", c4v1.PartySignIn{Issuer: sso, Subject: "00u-noor-2"}, c4v1.PartySignIn{Subject: "noor"}))
	ins := r.signIns(t)
	switch {
	case *r.secrets != 0:
		t.Errorf("the Secret is read %d times with Parties", *r.secrets)
	case ins.Person(sso, "00u-noor") != "" || ins.Person(sso, "00u-lee") != "":
		t.Errorf("the Secret's sign-ins hold with Parties: %v", ins)
	case ins.Person(sso, "00u-noor-2") != "noor" || ins.Person("", "noor") != "":
		t.Errorf("the issuer's sign-ins are not noor's Party's provider subjects: %v", ins)
	}
	admin := farmAdmins(r.reader, r.book)
	if !admin(ctx, "noor") || admin(ctx, "lee") {
		t.Error("a person's Party: its person is not the one admin")
	}

	// the farm bound to an organisation: its admins by name, in the book
	r = newIssuerFarm(t, "kestrel", organisation("kestrel"), person("dana"))
	admin = farmAdmins(r.reader, r.book)
	if admin(ctx, "kestrel") || admin(ctx, "dana") {
		t.Fatal("an admin before the book names one: the binding's owner, or a person with no membership")
	}
	r.bookControl(t, "make-party", farmbook.PartyKey("kestrel"), map[string]any{"name": "kestrel", "member": "dana", "purposes": []string{"member", "admin"}})
	r.bookControl(t, "add-member", farmbook.MemberKey("kestrel", "lee"), map[string]any{"party": "kestrel", "member": "lee", "purposes": []string{"member", "view-spend"}})
	if !admin(ctx, "dana") || admin(ctx, "lee") || admin(ctx, "kestrel") {
		t.Errorf("kestrel's admins: dana %v (want true), lee %v, kestrel %v", admin(ctx, "dana"), admin(ctx, "lee"), admin(ctx, "kestrel"))
	}
	r.bookControl(t, "end-member", farmbook.MemberKey("kestrel", "dana"), map[string]any{"party": "kestrel", "member": "dana", "purposes": []string{}})
	if admin(ctx, "dana") {
		t.Error("dana is an admin after her membership ended")
	}

	// a membership signed at another farm, whose CA the roots trust too,
	// makes no one an admin here
	r = newIssuerFarm(t, "kestrel", organisation("kestrel"))
	other := provtest.NewFarm(t, time.Now())
	r.f.Roots.AddCert(other.CA.Cert)
	r.bookUnder(t, other.CA, "make-party", farmbook.PartyKey("kestrel"), map[string]any{"name": "kestrel", "member": "mallory", "purposes": []string{"member", "admin"}})
	if farmAdmins(r.reader, r.book)(ctx, "mallory") {
		t.Error("another farm's record makes mallory an admin")
	}

	// the farm bound to someone with no Party: no one, not the owner
	r = newIssuerFarm(t, "noor", organisation("kestrel"))
	if farmAdmins(r.reader, r.book)(ctx, "noor") {
		t.Error("with Parties, the farm binding's owner is an admin by that alone")
	}
}

// One subject is one person's, across every Party: the issuer's sign-ins
// name no one for a subject two people's sign-ins hold, and an enrolment
// on it is refused (enroll.go, 409).
func TestIssuerOneSubjectOnePerson(t *testing.T) {
	both := c4v1.PartySignIn{Issuer: sso, Subject: "00u-shared"}
	r := newIssuerFarm(t, "noor", person("noor", both), person("lee", both))
	ins := r.signIns(t)
	if got := ins.People(sso, "00u-shared"); len(got) != 2 || ins.Person(sso, "00u-shared") != "" {
		t.Errorf("a subject in two people's sign-ins: holders %v, person %q", got, ins.Person(sso, "00u-shared"))
	}
}
