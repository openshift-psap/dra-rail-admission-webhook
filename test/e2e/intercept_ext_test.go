//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// testInterceptionExt covers the /mutate-ext endpoint which serves unlabeled
// namespaces. Only intercepted extended resources are processed; gpu-nic-pair
// requests are ignored.
func testInterceptionExt(t *testing.T) {
	// Unlabeled namespace — hits /mutate-ext, not /mutate
	f := NewUnlabeledFramework(t, "ext")

	// Use the labeled framework for config patching (shares the same webhook)
	cfgF := NewFrameworkWithoutNamespace(t)

	restore := SaveConfigMap(t, cfgF)
	t.Cleanup(restore)

	// --- Phase 1: Interception disabled — passthrough ---

	t.Run("Disabled_Passthrough", func(t *testing.T) {
		DisableInterception(t, cfgF)

		pod := NvidiaGPUPod("ext-passthrough", 1)
		created := CreatePod(t, f, pod)

		AssertPodNotMutated(t, created)
		t.Log("/mutate-ext: nvidia.com/gpu passed through (interception disabled)")
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 2: Interception enabled ---

	t.Run("Enabled_InterceptsGPU", func(t *testing.T) {
		EnableInterception(t, cfgF, []map[string]interface{}{
			{
				"resourceName":    "nvidia.com/gpu",
				"deviceClassName": "gpu.nvidia.com",
			},
		})

		pod := NvidiaGPUPod("ext-intercept-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertInterceptedResourceStripped(t, created, webhook.ResourceNvidiaGPU)

		if len(created.Spec.ResourceClaims) == 0 {
			t.Fatal("expected resourceClaims from /mutate-ext")
		}

		for _, c := range created.Spec.Containers {
			for _, ref := range c.Resources.Claims {
				if ref.Request != "device" {
					t.Errorf("claim ref request = %q, want device", ref.Request)
				}
			}
		}

		t.Log("/mutate-ext: nvidia.com/gpu intercepted in unlabeled namespace")

		if os.Getenv("E2E_SKIP_GPU") != "true" {
			WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
			t.Log("Pod reached Running via /mutate-ext")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 3: gpu-nic-pair ignored by /mutate-ext ---

	t.Run("Enabled_IgnoresGPUNICPair", func(t *testing.T) {
		pod := GPUNICPod("ext-pair-ignored", 1)
		created := CreatePod(t, f, pod)

		// /mutate-ext should NOT process gpu-nic-pair
		if created.Annotations != nil && created.Annotations[webhook.AnnotationMutated] == "true" {
			if len(created.Spec.ResourceClaims) > 0 {
				t.Error("/mutate-ext should not mutate gpu-nic-pair requests")
			}
		}

		// Synthetic resource should still be present
		hasPair := false
		for _, c := range created.Spec.Containers {
			if c.Resources.Requests != nil {
				if _, ok := c.Resources.Requests[webhook.ResourceGPUNICPair]; ok {
					hasPair = true
				}
			}
		}
		if !hasPair {
			t.Error("gpu-nic-pair should remain in requests (not processed by /mutate-ext)")
		}

		t.Log("/mutate-ext: gpu-nic-pair correctly ignored in unlabeled namespace")
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 4: Mixed pod — /mutate-ext only intercepts the GPU, leaves pair alone ---

	t.Run("Enabled_MixedPod_OnlyGPUIntercepted", func(t *testing.T) {
		pod := MixedResourcePod("ext-mixed", 2, 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		AssertInterceptedResourceStripped(t, created, webhook.ResourceNvidiaGPU)

		// gpu-nic-pair should still be in requests (not touched by /mutate-ext)
		hasPair := false
		for _, c := range created.Spec.Containers {
			if c.Resources.Requests != nil {
				if _, ok := c.Resources.Requests[webhook.ResourceGPUNICPair]; ok {
					hasPair = true
				}
			}
		}
		if !hasPair {
			t.Error("gpu-nic-pair should remain in requests via /mutate-ext")
		}

		t.Log("/mutate-ext: mixed pod — only nvidia.com/gpu intercepted, gpu-nic-pair left alone")
		CleanupAllPods(t, f, 60*time.Second)
	})

	// --- Phase 5: Disable revert ---

	t.Run("Disable_Revert", func(t *testing.T) {
		DisableInterception(t, cfgF)

		pod := NvidiaGPUPod("ext-after-disable", 1)
		created := CreatePod(t, f, pod)

		AssertPodNotMutated(t, created)
		t.Log("/mutate-ext: interception disabled → passthrough again")
		CleanupAllPods(t, f, 60*time.Second)
	})
}
