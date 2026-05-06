//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// TestAllocationVerification covers tests 11-15: verifying actual DRA allocations.
// Each subtest cleans up pods+claims before the next to avoid resource exhaustion.
func testAllocationVerification(t *testing.T) {
	if os.Getenv("E2E_SKIP_GPU") == "true" {
		t.Skip("E2E_SKIP_GPU set — skipping allocation verification tests")
	}

	f := NewFramework(t, "alloc")

	// Wait for allocator pending entries from previous suite to expire (2min TTL)
	t.Log("Waiting 150s for allocator pending entries to expire")
	time.Sleep(150 * time.Second)

	t.Run("PCIePairing", func(t *testing.T) {
		pod := GPUNICPod("pcie-pair-2", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		claimName := findClaimForPod(t, f, created.Name)
		AssertClaimPCIePairing(t, f, claimName)
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("NUMALocality", func(t *testing.T) {
		pod := GPUNICPod("numa-local-3", 3)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		claimName := findClaimForPod(t, f, created.Name)
		AssertNUMALocality(t, f, claimName)
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("NICConfigApplied", func(t *testing.T) {
		pod := NetworkTestPod("nic-config-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		stdout, _ := PodExec(t, f, created.Name, "test", []string{"ip", "link", "show"})
		if !strings.Contains(stdout, "net0") {
			t.Errorf("expected interface net0 in pod, got:\n%s", stdout)
		}

		stdout, _ = PodExec(t, f, created.Name, "test", []string{"cat", "/sys/class/net/net0/mtu"})
		mtu := strings.TrimSpace(stdout)
		if mtu != "9000" && mtu != "2044" {
			t.Errorf("expected MTU 9000 or 2044, got %s", mtu)
		}

		stdout, _ = PodExec(t, f, created.Name, "test", []string{"ip", "rule", "show"})
		t.Logf("Routing rules:\n%s", stdout)
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("TemplateReuse", func(t *testing.T) {
		pod1 := GPUNICPod("reuse-a", 1)
		pod2 := GPUNICPod("reuse-b", 1)

		created1 := CreatePod(t, f, pod1)
		created2 := CreatePod(t, f, pod2)

		AssertPodMutated(t, created1)
		AssertPodMutated(t, created2)

		tmpl1 := *created1.Spec.ResourceClaims[0].ResourceClaimTemplateName
		tmpl2 := *created2.Spec.ResourceClaims[0].ResourceClaimTemplateName

		if tmpl1 != tmpl2 {
			t.Errorf("expected same template name, got %q and %q", tmpl1, tmpl2)
		} else {
			t.Logf("Both pods share template: %s", tmpl1)
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("ClaimLifecycle", func(t *testing.T) {
		pod := GPUNICPod("lifecycle-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		claimName := findClaimForPod(t, f, created.Name)
		t.Logf("ResourceClaim %s exists while pod is running", claimName)

		err := f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
			context.Background(), created.Name, metav1.DeleteOptions{
				GracePeriodSeconds: int64Ptr(0),
			})
		if err != nil {
			t.Fatalf("failed to delete pod: %v", err)
		}

		WaitForDeletion(t, f, "ResourceClaim", claimName, 2*time.Minute)
		t.Logf("ResourceClaim %s was garbage collected after pod deletion", claimName)
	})
}

func findClaimForPod(t *testing.T, f *Framework, podName string) string {
	t.Helper()

	claimName := fmt.Sprintf("%s-%s", podName, webhook.ResourceClaimName)

	_, err := f.ResourceClient.ResourceClaims(f.Namespace).Get(
		context.Background(), claimName, metav1.GetOptions{})
	if err != nil {
		claims, listErr := f.ResourceClient.ResourceClaims(f.Namespace).List(
			context.Background(), metav1.ListOptions{})
		if listErr != nil {
			t.Fatalf("failed to find claim for pod %s: %v (list error: %v)", podName, err, listErr)
		}
		for _, c := range claims.Items {
			if strings.Contains(c.Name, podName) {
				t.Logf("Found claim %s for pod %s", c.Name, podName)
				return c.Name
			}
		}
		t.Fatalf("no ResourceClaim found for pod %s (tried %s, listed %d claims)", podName, claimName, len(claims.Items))
	}

	return claimName
}
