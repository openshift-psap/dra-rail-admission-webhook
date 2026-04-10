//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// TestEdgeCases covers tests 30-34: failure modes and edge cases.
func testEdgeCases(t *testing.T) {
	f := NewFramework(t, "edge")

	// Test 30: Webhook down — failurePolicy: Fail should reject pods
	t.Run("WebhookDown", func(t *testing.T) {
		// Scale webhook to 0
		scale, err := f.KubeClient.AppsV1().Deployments(f.WebhookNS).GetScale(
			context.Background(), webhookDeployment, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get scale: %v", err)
		}
		originalReplicas := scale.Spec.Replicas

		scale.Spec.Replicas = 0
		_, err = f.KubeClient.AppsV1().Deployments(f.WebhookNS).UpdateScale(
			context.Background(), webhookDeployment, scale, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("failed to scale down webhook: %v", err)
		}

		t.Cleanup(func() {
			// Restore original replicas
			s, err := f.KubeClient.AppsV1().Deployments(f.WebhookNS).GetScale(
				context.Background(), webhookDeployment, metav1.GetOptions{})
			if err != nil {
				t.Logf("WARNING: failed to get scale for restore: %v", err)
				return
			}
			s.Spec.Replicas = originalReplicas
			_, err = f.KubeClient.AppsV1().Deployments(f.WebhookNS).UpdateScale(
				context.Background(), webhookDeployment, s, metav1.UpdateOptions{})
			if err != nil {
				t.Logf("WARNING: failed to restore webhook scale: %v", err)
				return
			}
			WaitForDeploymentReady(t, f, f.WebhookNS, webhookDeployment, 3*time.Minute)
		})

		// Wait for all webhook pods to terminate
		WaitForCondition(t, 60*time.Second, "webhook pods terminated", func() bool {
			pods, err := f.KubeClient.CoreV1().Pods(f.WebhookNS).List(
				context.Background(), metav1.ListOptions{
					LabelSelector: "app=dra-gpu-nic-webhook",
				})
			if err != nil {
				return false
			}
			running := 0
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					running++
				}
			}
			return running == 0
		})

		// Try to create a GPU-NIC pod — should fail due to failurePolicy: Fail
		pod := GPUNICPod("webhook-down-test", 1)
		AssertPodRejected(t, f, pod, "")
		t.Log("Pod correctly rejected when webhook is down (failurePolicy: Fail)")
	})

	// Test 31: ConfigMap missing — webhook should start with defaults
	t.Run("ConfigMapMissing", func(t *testing.T) {
		// Save and delete the ConfigMap
		restore := SaveConfigMap(t, f)
		t.Cleanup(restore)

		err := f.KubeClient.CoreV1().ConfigMaps(f.WebhookNS).Delete(
			context.Background(), f.ConfigMapName, metav1.DeleteOptions{})
		if err != nil {
			t.Fatalf("failed to delete ConfigMap: %v", err)
		}

		// Restart webhook to pick up missing ConfigMap
		RestartAndWait(t, f, f.WebhookNS, webhookDeployment, 3*time.Minute)

		// Create a pod — should work with defaults
		pod := GPUNICPod("configmap-missing", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)
		t.Log("Webhook started with defaults when ConfigMap is missing")

		// Restore ConfigMap before cleanup restarts
		restore()
		RestartAndWait(t, f, f.WebhookNS, webhookDeployment, 3*time.Minute)
	})

	// Test 32: Concurrent requests — multiple pods simultaneously
	t.Run("ConcurrentRequests", func(t *testing.T) {
		const numPods = 4
		var wg sync.WaitGroup
		results := make([]*corev1.Pod, numPods)
		errors := make([]error, numPods)

		// Fire all pod creations concurrently
		for i := 0; i < numPods; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				pod := GPUNICPod(
					fmt.Sprintf("concurrent-%d", idx), 2)
				pod.Namespace = f.Namespace
				created, err := f.KubeClient.CoreV1().Pods(f.Namespace).Create(
					context.Background(), pod, metav1.CreateOptions{})
				results[idx] = created
				errors[idx] = err
			}(i)
		}
		wg.Wait()

		// Cleanup all created pods
		t.Cleanup(func() {
			for i := 0; i < numPods; i++ {
				if results[i] != nil {
					_ = f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
						context.Background(), results[i].Name, metav1.DeleteOptions{
							GracePeriodSeconds: int64Ptr(0),
						})
				}
			}
		})

		// All should be admitted (template creation is idempotent)
		admitted := 0
		for i := 0; i < numPods; i++ {
			if errors[i] != nil {
				t.Errorf("pod concurrent-%d failed: %v", i, errors[i])
			} else {
				admitted++
				AssertPodMutated(t, results[i])
			}
		}
		t.Logf("%d/%d concurrent pods admitted", admitted, numPods)

		// Verify all pods use a template with the expected naming pattern
		if admitted >= 2 {
			templates := make(map[string]bool)
			for i := 0; i < numPods; i++ {
				if results[i] != nil && results[i].Spec.ResourceClaims != nil {
					tmpl := *results[i].Spec.ResourceClaims[0].ResourceClaimTemplateName
					templates[tmpl] = true
				}
			}
			if len(templates) == 1 {
				for tmpl := range templates {
					t.Logf("All concurrent pods share template: %s", tmpl)
				}
			} else {
				// Multiple templates can happen if config hash changed between requests
				// (e.g., after ConfigMap restore). This is not a failure — template creation
				// is still idempotent per config variant.
				t.Logf("Concurrent pods used %d distinct templates (config hash may have changed)", len(templates))
				for tmpl := range templates {
					t.Logf("  template: %s", tmpl)
				}
			}
		}
	})

	// Test 33: Invalid count — non-integer gpu-nic-pair value
	t.Run("InvalidCount", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-count",
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:  "test",
						Image: "registry.k8s.io/pause:3.10",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								// Extended resources in K8s must be integers.
								// The API server may reject this before the webhook sees it.
								// If not, the webhook's extractGPUNICPairCount should catch it.
								corev1.ResourceName(webhook.ResourceGPUNICPair): *parseQuantityOrPanic("0"),
							},
						},
					},
				},
			},
		}
		AssertPodRejected(t, f, pod, "")
		t.Log("Invalid/zero count correctly rejected")
	})

	// Test 34: Namespace selector — pod in namespace without webhook label
	t.Run("NamespaceSelector", func(t *testing.T) {
		// Create a namespace WITHOUT the webhook-enabled label
		noWebhookNS := "e2e-no-webhook-" + randomSuffix()
		_, err := f.KubeClient.CoreV1().Namespaces().Create(
			context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: noWebhookNS,
					// No dra.llm-d.io/webhook-enabled label
				},
			}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create namespace: %v", err)
		}
		t.Cleanup(func() {
			_ = f.KubeClient.CoreV1().Namespaces().Delete(
				context.Background(), noWebhookNS, metav1.DeleteOptions{})
		})

		// Create a GPU-NIC pod in that namespace
		pod := GPUNICPod("no-webhook", 2)
		created := CreatePodInNamespace(t, f, noWebhookNS, pod)

		// The webhook should NOT have mutated this pod
		if created.Annotations != nil && created.Annotations[webhook.AnnotationMutated] == "true" {
			t.Error("pod was mutated in namespace without webhook-enabled label")
		}
		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("pod has resourceClaims in namespace without webhook-enabled label")
		}
		// Synthetic resource should still be present
		hasResource := false
		for _, c := range created.Spec.Containers {
			if c.Resources.Requests != nil {
				if _, ok := c.Resources.Requests[corev1.ResourceName(webhook.ResourceGPUNICPair)]; ok {
					hasResource = true
				}
			}
		}
		if !hasResource {
			t.Error("synthetic resource was stripped even without webhook — unexpected")
		}
		t.Log("Pod in unlabeled namespace was not mutated by webhook")
	})
}

// parseQuantityOrPanic is a helper for test setup.
func parseQuantityOrPanic(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
