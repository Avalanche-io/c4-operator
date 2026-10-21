package farmkey

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Setting is the farm's telemetry setting, a ConfigMap in the operator's
// namespace (DESIGN.md section 6.9). Only the manager writes it, and the
// admission policy c4-telemetry-setting refuses anyone else. Its data
// holds the tier and the signed control record that last changed it, by
// its C4 ID and whole, so the record stays with the setting it signs.
const Setting = "c4-telemetry"

// LoadTier reads the farm's tier and the ID of the record that set it; ok
// is false when the farm has none yet, and the install's preset applies.
func LoadTier(ctx context.Context, kube kubernetes.Interface, ns string) (tier, record string, ok bool, err error) {
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, Setting, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	tier, ok = cm.Data["tier"]
	return tier, cm.Data["record"], ok, nil
}

// SaveTier writes the farm's tier and the record of the change.
func SaveTier(ctx context.Context, kube kubernetes.Interface, ns, tier, recordID string, record []byte) error {
	data := map[string]string{"tier": tier}
	if recordID != "" {
		data["record"] = recordID
		data["record.json"] = string(record)
	}
	cms := kube.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, Setting, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: Setting, Namespace: ns}, Data: data}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.Data = data
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
