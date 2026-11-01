package farmbook

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The book's two mutable objects are Secrets in the signing namespace,
// which only the manager reads and writes, by name (resourceNames), as
// the people's index head was before the book: the State (StateSecret),
// the sealed head and the pending entries by C4 ID; and the sealer's own
// head (SealerSecret), which the sealer alone moves. Neither holds an
// entry or a record: those are in the system store.
const (
	StateSecret  = "c4-farm-book"
	SealerSecret = "c4-farm-book-sealer"
)

// secrets reads and writes one Secret by resourceVersion. The manager's
// client reads Secrets uncached.
type secrets struct {
	client    client.Client
	namespace string
	name      string
}

func (s secrets) read(ctx context.Context) (map[string][]byte, string, error) {
	var sec corev1.Secret
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: s.name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return sec.Data, sec.ResourceVersion, nil
}

func (s secrets) write(ctx context.Context, version string, data map[string][]byte) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: s.name, ResourceVersion: version,
		Labels: map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "farm-book"}},
		Type: corev1.SecretTypeOpaque, Data: data}
	var err error
	if version == "" {
		err = s.client.Create(ctx, sec)
	} else {
		err = s.client.Update(ctx, sec)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ErrMoved
	}
	return err
}

// SecretPointer is the manager's Pointer: StateSecret, written by
// resourceVersion, so writers and the sealer move it one at a time.
type SecretPointer struct {
	Client    client.Client
	Namespace string
}

func (p *SecretPointer) Read(ctx context.Context) (State, error) {
	data, version, err := secrets{p.Client, p.Namespace, StateSecret}.read(ctx)
	if err != nil {
		return State{}, err
	}
	s := State{Version: version}
	if v := data["head"]; len(v) > 0 {
		if err := json.Unmarshal(v, &s.Head); err != nil {
			return State{}, fmt.Errorf("%s head: %w", StateSecret, err)
		}
	}
	if v := data["pending"]; len(v) > 0 {
		if err := json.Unmarshal(v, &s.Pending); err != nil {
			return State{}, fmt.Errorf("%s pending: %w", StateSecret, err)
		}
	}
	return s, nil
}

func (p *SecretPointer) Write(ctx context.Context, from, to State) error {
	head, _ := json.Marshal(to.Head)
	pending, _ := json.Marshal(to.Pending)
	if to.Pending == nil {
		pending = []byte("[]")
	}
	return secrets{p.Client, p.Namespace, StateSecret}.write(ctx, from.Version, map[string][]byte{"head": head, "pending": pending})
}

// SecretSealer is the manager's SealerStore: SealerSecret, which only the
// sealer writes.
type SecretSealer struct {
	Client    client.Client
	Namespace string
}

func (p *SecretSealer) Read(ctx context.Context) (SealerState, error) {
	data, version, err := secrets{p.Client, p.Namespace, SealerSecret}.read(ctx)
	if err != nil {
		return SealerState{}, err
	}
	var s SealerState
	if v := data["sealer"]; len(v) > 0 {
		if err := json.Unmarshal(v, &s); err != nil {
			return SealerState{}, fmt.Errorf("%s: %w", SealerSecret, err)
		}
	}
	s.Version = version
	return s, nil
}

func (p *SecretSealer) Write(ctx context.Context, from, to SealerState) error {
	b, _ := json.Marshal(to)
	return secrets{p.Client, p.Namespace, SealerSecret}.write(ctx, from.Version, map[string][]byte{"sealer": b})
}
