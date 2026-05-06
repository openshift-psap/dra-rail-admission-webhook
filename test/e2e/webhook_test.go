//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// TestWebhookMutation covers core mutation behavior.
// Heavy tests (that allocate devices) run sequentially with cleanup between them.
// Admission-only tests (reject/idempotency/no-op) grouped together — no devices consumed.
func testWebhookMutation(t *testing.T) {
	f := NewFramework(t, "mutation")

	// --- Admission-only tests (no GPU allocation, can run without cleanup) ---

	t.Run("DenyMidRange_5pairs", func(t *testing.T) {
		pod := GPUNICPod("deny-midrange-5", 5)
		AssertPodRejected(t, f, pod, "")
		t.Log("5-pair request without cross-NUMA correctly rejected")
	})

	t.Run("DenyOverMax_9pairs", func(t *testing.T) {
		pod := GPUNICPod("deny-overmax-9", 9)
		AssertPodRejected(t, f, pod, "")
		t.Log("9-pair request correctly rejected (exceeds max per node)")
	})

	t.Run("Idempotency", func(t *testing.T) {
		pod := AlreadyMutatedPod("already-mutated", 2)
		created := CreatePod(t, f, pod)
		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("webhook should not mutate a pod that already has the mutated annotation")
		}
		t.Log("Already-mutated pod passed through without re-mutation")
	})

	t.Run("NoOpNormalPod", func(t *testing.T) {
		pod := NormalPod("normal-pod")
		created := CreatePod(t, f, pod)
		if created.Annotations != nil && created.Annotations[webhook.AnnotationMutated] == "true" {
			t.Error("normal pod should not be mutated")
		}
		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("normal pod should not have resourceClaims")
		}
		t.Log("Normal pod passed through without mutation")
	})

	// --- Heavy tests (allocate devices, cleanup between each) ---

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
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("ResourceStripped_2pairs", func(t *testing.T) {
		pod := GPUNICPod("stripped-2pair", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		if len(created.Spec.Containers[0].Resources.Claims) == 0 {
			t.Error("container should have resource claims after mutation")
		}
		expectedClaims := 4
		if len(created.Spec.Containers[0].Resources.Claims) != expectedClaims {
			t.Errorf("expected %d resource claims, got %d",
				expectedClaims, len(created.Spec.Containers[0].Resources.Claims))
		}

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
		t.Log("Synthetic resource stripped, replaced with 4 claim references")
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("NUMAConstrained_4pairs", func(t *testing.T) {
		if os.Getenv("E2E_SKIP_GPU") == "true" {
			t.Skip("E2E_SKIP_GPU set")
		}
		pod := GPUNICPod("numa-4pair", 4)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		t.Log("Pod with 4 pairs scheduled — NUMA locality enforced")
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("MidRangeWithAnnotation_5pairs", func(t *testing.T) {
		pod := GPUNICPodWithAnnotations("midrange-xnuma-5", 5, map[string]string{
			webhook.AnnotationAllowCrossNUMA: "true",
		})
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
		t.Log("5-pair request with cross-NUMA annotation correctly admitted")
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("ExplicitCrossNUMA_3pairs", func(t *testing.T) {
		pod := GPUNICPodWithAnnotations("xnuma-3pair", 3, map[string]string{
			webhook.AnnotationAllowCrossNUMA: "true",
		})
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		templateName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		t.Logf("Template name: %s (should indicate cross-NUMA mode)", templateName)

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("FullNode_8pairs", func(t *testing.T) {
		if os.Getenv("E2E_SKIP_GPU") == "true" {
			t.Skip("E2E_SKIP_GPU set")
		}
		// Wait for allocator pending entries to expire (2min TTL)
		t.Log("Waiting 150s for allocator pending entries to expire before full-node test")
		time.Sleep(150 * time.Second)
		pod := GPUNICPod("full-8pair", 8)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		if created.Annotations[webhook.AnnotationAllowCrossNUMA] == "true" {
			t.Error("8-pair request should not need explicit cross-NUMA annotation")
		}

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		t.Log("Pod with 8 pairs scheduled — auto cross-NUMA for full node")
		CleanupAllPods(t, f, 60*time.Second)
	})
}
