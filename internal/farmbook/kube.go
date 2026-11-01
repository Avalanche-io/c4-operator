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

// StateConfigMap is the book's State in the operator's namespace: the
// sealed head and the pending entries, by C4 ID, beside the transition
// log's c4-work-book. It holds IDs only; the entries and records are in
// the system store. Whoever can write it can roll the head back to an
// older signed segment, which a reader keeping its last head refuses.
const StateConfigMap = "c4-farm-book"

// ConfigMapPointer is the manager's Pointer: StateConfigMap, written by
// resourceVersion, so writers and the sealer move it one at a time.
type ConfigMapPointer struct {
	Client client.Client
	// Reader, when set, reads the ConfigMap uncached (the manager's API
	// reader), so a write isn't made from a version the cache hasn't
	// caught up with.
	Reader    client.Reader
	Namespace string
}

func (p *ConfigMapPointer) Read(ctx context.Context) (State, error) {
	var cm corev1.ConfigMap
	var r client.Reader = p.Client
	if p.Reader != nil {
		r = p.Reader
	}
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: StateConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	s := State{Version: cm.ResourceVersion}
	if v := cm.Data["head"]; v != "" {
		if err := json.Unmarshal([]byte(v), &s.Head); err != nil {
			return State{}, fmt.Errorf("%s head: %w", StateConfigMap, err)
		}
	}
	if v := cm.Data["pending"]; v != "" {
		if err := json.Unmarshal([]byte(v), &s.Pending); err != nil {
			return State{}, fmt.Errorf("%s pending: %w", StateConfigMap, err)
		}
	}
	return s, nil
}

func (p *ConfigMapPointer) Write(ctx context.Context, from, to State) error {
	head, _ := json.Marshal(to.Head)
	pending, _ := json.Marshal(to.Pending)
	if to.Pending == nil {
		pending = []byte("[]")
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: StateConfigMap, ResourceVersion: from.Version,
		Labels: map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "queue"}},
		Data: map[string]string{"head": string(head), "pending": string(pending)}}
	var err error
	if from.Version == "" {
		err = p.Client.Create(ctx, cm)
	} else {
		err = p.Client.Update(ctx, cm)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ErrMoved
	}
	return err
}
