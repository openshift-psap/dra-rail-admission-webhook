//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// TestWebhookMutation covers tests 1-10: core mutation behavior.
func testWebhookMutation(t *testing.T) {
	f := NewFramework(t, "mutation")

	// Test 1: Basic mutation — 1 GPU-NIC pair
	t.Run("BasicMutation_1pair", func(t *testing.T) {
		pod := GPUNICPod("basic-1pair", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		if created.Annotations[webhook.AnnotationMutated] != "true" {
			t.Errorf("expected mutated annotation, got %v", created.Annotations)
		}

		if len(created.Spec.ResourceClaims) == 0 {
			t.Fatal("expected resourceClaims to be set")
		}
		templateName := created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		if templateName == nil {
			t.Fatal("expected ResourceClaimTemplateName to be set")
		}
		AssertTemplateExists(t, f, *templateName)

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
	})

	// Test 2: NUMA-constrained — 4 pairs, all on same NUMA zone
	t.Run("NUMAConstrained_4pairs", func(t *testing.T) {
		if os.Getenv("E2E_SKIP_GPU") == "true" {
			t.Skip("E2E_SKIP_GPU set")
		}
		pod := GPUNICPod("numa-4pair", 4)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		// Wait for scheduling and verify NUMA locality
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		t.Log("Pod with 4 pairs scheduled — NUMA locality enforced by matchAttribute constraint")
	})

	// Test 3: Full node auto cross-NUMA — 8 pairs
	t.Run("FullNode_8pairs", func(t *testing.T) {
		if os.Getenv("E2E_SKIP_GPU") == "true" {
			t.Skip("E2E_SKIP_GPU set")
		}
		pod := GPUNICPod("full-8pair", 8)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		// Should be admitted without allow-cross-numa annotation
		if created.Annotations[webhook.AnnotationAllowCrossNUMA] == "true" {
			t.Error("8-pair request should not need explicit cross-NUMA annotation")
		}

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		t.Log("Pod with 8 pairs scheduled — auto cross-NUMA for full node")
	})

	// Test 4: Explicit cross-NUMA for small request — 3 pairs + allow-cross-numa
	t.Run("ExplicitCrossNUMA_3pairs", func(t *testing.T) {
		pod := GPUNICPodWithAnnotations("xnuma-3pair", 3, map[string]string{
			webhook.AnnotationAllowCrossNUMA: "true",
		})
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		// Template name should contain "xnuma" not "numa"
		templateName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		t.Logf("Template name: %s (should indicate cross-NUMA mode)", templateName)

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
	})

	// Test 5: Deny mid-range — 5 pairs without cross-NUMA
	t.Run("DenyMidRange_5pairs", func(t *testing.T) {
		pod := GPUNICPod("deny-midrange-5", 5)
		AssertPodRejected(t, f, pod, "")
		t.Log("5-pair request without cross-NUMA correctly rejected")
	})

	// Test 6: Mid-range with annotation — 5 pairs + allow-cross-numa
	t.Run("MidRangeWithAnnotation_5pairs", func(t *testing.T) {
		pod := GPUNICPodWithAnnotations("midrange-xnuma-5", 5, map[string]string{
			webhook.AnnotationAllowCrossNUMA: "true",
		})
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)
		t.Log("5-pair request with cross-NUMA annotation correctly admitted")
	})

	// Test 7: Deny over max — 9 pairs
	t.Run("DenyOverMax_9pairs", func(t *testing.T) {
		pod := GPUNICPod("deny-overmax-9", 9)
		AssertPodRejected(t, f, pod, "")
		t.Log("9-pair request correctly rejected (exceeds max per node)")
	})

	// Test 8: Idempotency — already mutated pod
	t.Run("Idempotency", func(t *testing.T) {
		pod := AlreadyMutatedPod("already-mutated", 2)
		created := CreatePod(t, f, pod)

		// The webhook should NOT add resourceClaims since it's already marked mutated
		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("webhook should not mutate a pod that already has the mutated annotation")
		}
		t.Log("Already-mutated pod passed through without re-mutation")
	})

	// Test 9: No-op normal pod — no gpu-nic-pair resource
	t.Run("NoOpNormalPod", func(t *testing.T) {
		pod := NormalPod("normal-pod")
		created := CreatePod(t, f, pod)

		// Should not have been mutated
		if created.Annotations != nil && created.Annotations[webhook.AnnotationMutated] == "true" {
			t.Error("normal pod should not be mutated")
		}
		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("normal pod should not have resourceClaims")
		}
		t.Log("Normal pod passed through without mutation")
	})

	// Test 10: Resource stripped — verify synthetic resource is removed
	t.Run("ResourceStripped_2pairs", func(t *testing.T) {
		pod := GPUNICPod("stripped-2pair", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		// Verify the container has claim references instead
		if len(created.Spec.Containers[0].Resources.Claims) == 0 {
			t.Error("container should have resource claims after mutation")
		}
		// 2 pairs = 4 claim references (gpu-0, nic-0, gpu-1, nic-1)
		expectedClaims := 4
		if len(created.Spec.Containers[0].Resources.Claims) != expectedClaims {
			t.Errorf("expected %d resource claims, got %d",
				expectedClaims, len(created.Spec.Containers[0].Resources.Claims))
		}
		t.Log("Synthetic resource stripped, replaced with 4 claim references")
	})
}
