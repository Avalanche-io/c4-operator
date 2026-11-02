package localfarm

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RotateInstallerToken gives farm name a new installer token: the
// manager's Secret first, then the manager restarted to read it and the
// HUD waited for with the new one, and only then the person's copy. The
// old token works no more, and a browser signed in with it signs in again
// (c4 farm open). c4 farm touch-key --reopen rotates it once the issuer
// has agreed to reopen the touch-key window. A run that stops before the
// person's copy is written leaves the old one there, which c4 farm up puts
// back in the Secret and the manager.
func RotateInstallerToken(ctx context.Context, name, hud string) (string, error) {
	c, err := kubeClient(name)
	if err != nil {
		return "", err
	}
	p, err := installerTokenPath(name)
	if err != nil {
		return "", err
	}
	t, err := newToken()
	if err != nil {
		return "", err
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: InstallerSecret}, &sec); err != nil {
		return "", fmt.Errorf("the manager's copy of the installer token: %w (c4 farm up puts the new one there)", err)
	}
	sec.Data = map[string][]byte{"token": []byte(t)}
	sec.StringData = nil
	if err := c.Update(ctx, &sec); err != nil {
		return "", fmt.Errorf("the manager's copy of the installer token: %w (c4 farm up puts the new one there)", err)
	}
	// the manager reads its token when it starts
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: "c4-manager"}, &dep); err != nil {
		return "", err
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"c4.avalanche.io/installer-rotated":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
	if err := c.Patch(ctx, &dep, client.RawPatch(types.StrategicMergePatchType, []byte(patch))); err != nil {
		return "", fmt.Errorf("restarting the manager: %w (c4 farm up restarts it)", err)
	}
	if err := waitDeployment(ctx, c, "c4-system", "c4-manager", 5*time.Minute); err != nil {
		return "", err
	}
	if err := waitHUD(ctx, c, hud, t, 5*time.Minute); err != nil {
		return "", err
	}
	if err := writeToken(p, t); err != nil {
		return "", fmt.Errorf("the farm's installer token is rotated, and this machine's copy of it is not written: %w; c4 farm up puts the old one back", err)
	}
	return t, nil
}
