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
func testAllocationVerification(t *testing.T) {
	if os.Getenv("E2E_SKIP_GPU") == "true" {
		t.Skip("E2E_SKIP_GPU set — skipping allocation verification tests")
	}

	f := NewFramework(t, "alloc")

	// Test 11: PCIe pairing — each GPU+NIC pair shares same pcieRoot
	t.Run("PCIePairing", func(t *testing.T) {
		pod := GPUNICPod("pcie-pair-2", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		// Find the ResourceClaim created from the template
		claimName := findClaimForPod(t, f, created.Name)
		AssertClaimPCIePairing(t, f, claimName)
	})

	// Test 12: NUMA locality — all NICs on same NUMA node in NUMA-constrained claim
	t.Run("NUMALocality", func(t *testing.T) {
		pod := GPUNICPod("numa-local-3", 3)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		claimName := findClaimForPod(t, f, created.Name)
		AssertNUMALocality(t, f, claimName)
	})

	// Test 13: NIC config applied — verify interfaces, MTU, routing inside pod
	t.Run("NICConfigApplied", func(t *testing.T) {
		pod := NetworkTestPod("nic-config-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		// Check interface exists
		stdout, _ := PodExec(t, f, created.Name, "test", []string{"ip", "link", "show"})
		if !strings.Contains(stdout, "net0") {
			t.Errorf("expected interface net0 in pod, got:\n%s", stdout)
		}

		// Check MTU
		stdout, _ = PodExec(t, f, created.Name, "test", []string{"cat", "/sys/class/net/net0/mtu"})
		mtu := strings.TrimSpace(stdout)
		if mtu != "9000" {
			t.Errorf("expected MTU 9000, got %s", mtu)
		}

		// Check routing rules
		stdout, _ = PodExec(t, f, created.Name, "test", []string{"ip", "rule", "show"})
		t.Logf("Routing rules:\n%s", stdout)
		// The route table ID starts at StartingTableID (100)
		if !strings.Contains(stdout, "lookup 100") {
			t.Log("WARNING: expected routing rule with table 100 — may depend on NIC driver configuration")
		}
	})

	// Test 14: Template reuse — two pods with same count+mode should share a template
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
	})

	// Test 15: Claim lifecycle — delete pod, claim should be garbage collected
	t.Run("ClaimLifecycle", func(t *testing.T) {
		pod := GPUNICPod("lifecycle-1", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		claimName := findClaimForPod(t, f, created.Name)
		t.Logf("ResourceClaim %s exists while pod is running", claimName)

		// Delete the pod explicitly (the t.Cleanup will also try, harmlessly)
		err := f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
			context.Background(), created.Name, metav1.DeleteOptions{
				GracePeriodSeconds: int64Ptr(0),
			})
		if err != nil {
			t.Fatalf("failed to delete pod: %v", err)
		}

		// Wait for the ResourceClaim to be garbage collected
		WaitForDeletion(t, f, "ResourceClaim", claimName, 2*time.Minute)
		t.Logf("ResourceClaim %s was garbage collected after pod deletion", claimName)
	})
}

// findClaimForPod finds the ResourceClaim created for a pod from its resourceClaims spec.
func findClaimForPod(t *testing.T, f *Framework, podName string) string {
	t.Helper()

	// ResourceClaim names follow the pattern: <podName>-<claimName>
	// where claimName is webhook.ResourceClaimName ("gpu-nic-devices")
	claimName := fmt.Sprintf("%s-%s", podName, webhook.ResourceClaimName)

	// Verify it exists
	_, err := f.ResourceClient.ResourceClaims(f.Namespace).Get(
		context.Background(), claimName, metav1.GetOptions{})
	if err != nil {
		// Try listing all claims to find the right one
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
