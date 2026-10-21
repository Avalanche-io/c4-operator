// Package farmkey keeps the farm's telemetry key: 256 random bits and an
// epoch, in a Secret in the operator's own namespace that only the manager
// reads, beside the signing CA (DESIGN.md section 6.9). It is
// made the first time the farm produces any telemetry, never printed, and
// never written into an output; rotating it starts a new epoch, after
// which pseudonyms are unrelated to the earlier ones.
package farmkey

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
)

// Secret is the key's Secret, in the operator's namespace.
const Secret = "c4-telemetry-key"

// LoadOrCreate reads the farm's key, making it with epoch 1 if there is
// none.
func LoadOrCreate(ctx context.Context, kube kubernetes.Interface, ns string) (telemetry.Key, error) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, Secret, metav1.GetOptions{})
	if err == nil {
		return parse(sec)
	}
	if !apierrors.IsNotFound(err) {
		return telemetry.Key{}, err
	}
	sec, err = newSecret(ns, 1)
	if err != nil {
		return telemetry.Key{}, err
	}
	if _, err := kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica made it first.
			return LoadOrCreate(ctx, kube, ns)
		}
		return telemetry.Key{}, err
	}
	return parse(sec)
}

// Rotate replaces the key with a new one in the next epoch.
func Rotate(ctx context.Context, kube kubernetes.Interface, ns string) (telemetry.Key, error) {
	old, err := LoadOrCreate(ctx, kube, ns)
	if err != nil {
		return telemetry.Key{}, err
	}
	cur, err := kube.CoreV1().Secrets(ns).Get(ctx, Secret, metav1.GetOptions{})
	if err != nil {
		return telemetry.Key{}, err
	}
	next, err := newSecret(ns, old.Epoch+1)
	if err != nil {
		return telemetry.Key{}, err
	}
	cur.Data = next.Data
	if _, err := kube.CoreV1().Secrets(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return telemetry.Key{}, err
	}
	return parse(cur)
}

func newSecret(ns string, epoch uint32) (*corev1.Secret, error) {
	key := make([]byte, telemetry.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: Secret, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": key, "epoch": []byte(strconv.FormatUint(uint64(epoch), 10))},
	}, nil
}

func parse(sec *corev1.Secret) (telemetry.Key, error) {
	epoch, err := strconv.ParseUint(string(sec.Data["epoch"]), 10, 32)
	if err != nil {
		return telemetry.Key{}, fmt.Errorf("telemetry key: epoch: %v", err)
	}
	return telemetry.NewKey(sec.Data["key"], uint32(epoch))
}
