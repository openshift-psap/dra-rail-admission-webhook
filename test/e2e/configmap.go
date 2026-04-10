//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"gopkg.in/yaml.v3"
)

// SaveConfigMap reads the current ConfigMap and returns a restore function.
// Call t.Cleanup(restore) to ensure the ConfigMap is restored even on test failure.
func SaveConfigMap(t *testing.T, f *Framework) func() {
	t.Helper()

	cm, err := f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Get(
		context.Background(), f.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read ConfigMap %s/%s: %v", f.WebhookNS, f.ConfigMapName, err)
	}

	// Snapshot the data
	savedData := make(map[string]string)
	for k, v := range cm.Data {
		savedData[k] = v
	}

	return func() {
		cm, err := f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Get(
			context.Background(), f.ConfigMapName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// ConfigMap was deleted (e.g., by ConfigMapMissing test) — re-create it
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      f.ConfigMapName,
					Namespace: f.WebhookNS,
				},
				Data: savedData,
			}
			_, err = f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Create(
				context.Background(), newCM, metav1.CreateOptions{})
			if err != nil {
				t.Logf("WARNING: failed to re-create ConfigMap: %v", err)
			}
			return
		}
		if err != nil {
			t.Logf("WARNING: failed to get ConfigMap for restore: %v", err)
			return
		}
		cm.Data = savedData
		_, err = f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Update(
			context.Background(), cm, metav1.UpdateOptions{})
		if err != nil {
			t.Logf("WARNING: failed to restore ConfigMap: %v", err)
		}
	}
}

// PatchWebhookConfig updates specific fields in the config.yaml key of the ConfigMap.
func PatchWebhookConfig(t *testing.T, f *Framework, overrides map[string]interface{}) {
	t.Helper()
	patchConfigMapKey(t, f, "config.yaml", overrides)
}

// PatchReconcilerConfig updates specific fields in the reconciler.yaml key of the ConfigMap.
func PatchReconcilerConfig(t *testing.T, f *Framework, overrides map[string]interface{}) {
	t.Helper()
	patchConfigMapKey(t, f, "reconciler.yaml", overrides)
}

func patchConfigMapKey(t *testing.T, f *Framework, key string, overrides map[string]interface{}) {
	t.Helper()

	cm, err := f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Get(
		context.Background(), f.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	// Parse existing YAML
	existing := make(map[string]interface{})
	if data, ok := cm.Data[key]; ok {
		if err := yaml.Unmarshal([]byte(data), &existing); err != nil {
			t.Fatalf("failed to parse %s: %v", key, err)
		}
	}

	// Apply overrides
	for k, v := range overrides {
		existing[k] = v
	}

	// Marshal back
	updated, err := yaml.Marshal(existing)
	if err != nil {
		t.Fatalf("failed to marshal %s: %v", key, err)
	}

	cm.Data[key] = string(updated)
	_, err = f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Update(
		context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update ConfigMap: %v", err)
	}

	t.Logf("Patched ConfigMap %s key %s with %v", f.ConfigMapName, key, overrides)
}

// RestartDeployment triggers a rolling restart by patching the pod template annotation.
func RestartDeployment(t *testing.T, f *Framework, namespace, name string) {
	t.Helper()

	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
		time.Now().Format(time.RFC3339))

	_, err := f.KubeClient.AppsV1().Deployments(namespace).Patch(
		context.Background(), name, types.StrategicMergePatchType,
		[]byte(patch), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("failed to restart deployment %s/%s: %v", namespace, name, err)
	}

	t.Logf("Triggered restart of deployment %s/%s", namespace, name)
}

// RestartAndWait restarts a deployment and waits for it to become ready.
func RestartAndWait(t *testing.T, f *Framework, namespace, name string, timeout time.Duration) {
	t.Helper()
	RestartDeployment(t, f, namespace, name)
	// Brief pause to allow rollout to begin
	time.Sleep(3 * time.Second)
	WaitForDeploymentReady(t, f, namespace, name, timeout)
}
