//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// testInterception covers extended resource interception behavior.
// Requires a cluster with nvidia.com/gpu reported as an extended resource.
func testInterception(t *testing.T) {
	f := NewFramework(t, "intercept")

	// Save original config and restore after all subtests
	restore := SaveConfigMap(t, f)
	t.Cleanup(restore)

	// --- Phase 1: Interception disabled (default) ---

	t.Run("Disabled_Passthrough", func(t *testing.T) {
		DisableInterception(t, f)

		pod := NvidiaGPUPod("gpu-passthrough", 1)
		created := CreatePod(t, f, pod)

		AssertPodNotMutated(t, created)

		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("pod should not have resourceClaims when interception disabled")
		}

		// nvidia.com/gpu should still be in requests (not stripped)
		for _, c := range created.Spec.Containers {
			if c.Resources.Requests != nil {
				if _, ok := c.Resources.Requests[webhook.ResourceNvidiaGPU]; !ok {
					t.Error("nvidia.com/gpu should remain in requests when interception disabled")
				}
			}
		}

		t.Log("nvidia.com/gpu pod passed through unmutated (interception disabled)")
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 2: Enable interception ---

	t.Run("Enabled_BasicMutation", func(t *testing.T) {
		EnableInterception(t, f, []map[string]interface{}{
			{
				"resourceName":    "nvidia.com/gpu",
				"deviceClassName": "gpu.nvidia.com",
			},
		})

		pod := NvidiaGPUPod("gpu-intercept-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertInterceptedResourceStripped(t, created, webhook.ResourceNvidiaGPU)

		if len(created.Spec.ResourceClaims) == 0 {
			t.Fatal("expected resourceClaims to be set")
		}

		templateName := created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		if templateName == nil {
			t.Fatal("expected ResourceClaimTemplateName to be set")
		}
		AssertTemplateExists(t, f, *templateName)

		// Verify claim ref points to "device" request (not "gpu" or "nic")
		for _, c := range created.Spec.Containers {
			if c.Resources.Claims != nil {
				for _, ref := range c.Resources.Claims {
					if ref.Request != "device" {
						t.Errorf("claim ref request = %q, want device", ref.Request)
					}
				}
			}
		}

		t.Log("nvidia.com/gpu intercepted → DRA ResourceClaim created")

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
			t.Log("Pod reached Running with intercepted GPU claim")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("Enabled_MultiGPU", func(t *testing.T) {
		pod := NvidiaGPUPod("gpu-intercept-2", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertInterceptedResourceStripped(t, created, webhook.ResourceNvidiaGPU)

		if len(created.Spec.ResourceClaims) != 2 {
			t.Fatalf("expected 2 resourceClaims, got %d", len(created.Spec.ResourceClaims))
		}

		t.Logf("2-GPU request intercepted → %d ResourceClaims created", len(created.Spec.ResourceClaims))

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 3: Mutual exclusivity ---

	t.Run("Enabled_MutuallyExclusive", func(t *testing.T) {
		pod := MixedResourcePod("mixed-denied", 2, 1)
		AssertPodRejected(t, f, pod, "mutually exclusive")
		t.Log("Pod with both gpu-nic-pair and nvidia.com/gpu correctly denied")
	})

	// --- Phase 4: gpu-nic-pair still works with interception enabled ---

	t.Run("Enabled_PairsStillWork", func(t *testing.T) {
		pod := GPUNICPod("pairs-with-intercept", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertResourceStripped(t, created)

		if len(created.Spec.ResourceClaims) == 0 {
			t.Fatal("expected resourceClaims for gpu-nic-pair")
		}

		t.Log("gpu-nic-pair mutation still works with interception enabled")

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 5: Disable interception, verify revert ---

	t.Run("Disable_Revert", func(t *testing.T) {
		DisableInterception(t, f)

		pod := NvidiaGPUPod("gpu-after-disable", 1)
		created := CreatePod(t, f, pod)

		AssertPodNotMutated(t, created)

		if len(created.Spec.ResourceClaims) > 0 {
			t.Error("pod should not have resourceClaims after disabling interception")
		}

		t.Log("Interception disabled → nvidia.com/gpu passes through again")
		CleanupAllPods(t, f, 60*time.Second)
	})
}
