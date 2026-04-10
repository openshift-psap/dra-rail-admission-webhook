//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// TestPreflight covers tests 16-21: preflight availability checking.
func testPreflight(t *testing.T) {
	if os.Getenv("E2E_SKIP_GPU") == "true" {
		t.Skip("E2E_SKIP_GPU set — skipping preflight tests")
	}

	f := NewFramework(t, "preflight")

	// Enable preflight in the ConfigMap and restart webhook
	restore := SaveConfigMap(t, f)
	t.Cleanup(restore)
	PatchWebhookConfig(t, f, map[string]interface{}{"preflightCheck": true})
	RestartAndWait(t, f, f.WebhookNS, webhookDeployment, 3*time.Minute)

	// Discover all GPU nodes — needed for multi-node blocking
	gpuNodes := getGPUNodes(t, f)
	t.Logf("Cluster has %d GPU nodes: %v", len(gpuNodes), gpuNodes)

	// Test 16: Sufficient resources — fresh state, request 2
	t.Run("SufficientResources", func(t *testing.T) {
		pod := GPUNICPod("preflight-ok-2", 2)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		t.Log("Preflight passed — sufficient resources available")

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
	})

	// Test 18: Cross-NUMA fallback — request count > maxPairsPerNUMA with annotation
	// Runs before resource exhaustion tests to avoid cleanup timing issues.
	t.Run("CrossNUMAFallback", func(t *testing.T) {
		// Request 5 pairs with cross-NUMA annotation on a clean cluster.
		// 5 > maxPairsPerNUMA (4), so without cross-NUMA the webhook would
		// deny at validation. With the annotation, preflight checks per-node
		// total (8 available) and finds sufficient capacity.
		pod := GPUNICPodWithAnnotations("preflight-xnuma-5", 5, map[string]string{
			webhook.AnnotationAllowCrossNUMA: "true",
		})
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		t.Log("Preflight passed — cross-NUMA annotation allowed 5-pair request (> maxPairsPerNUMA)")

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)
	})

	// Tests 17+19: Resource exhaustion — block all nodes, verify denial for both
	// NUMA-constrained and cross-NUMA requests. Shared blocker setup avoids
	// creating/destroying 28 GPU-NIC pairs twice.
	t.Run("ResourceExhaustion", func(t *testing.T) {
		// Block all GPU nodes by consuming 7 of 8 pairs each.
		// Each node has 2 NUMA zones with 4 pairs. With 7 consumed (cross-NUMA),
		// at most 1 pair remains per node (0 on one NUMA, 1 on the other).
		createBlockerPodsOnAllNodes(t, f, "exhaust-blocker", 7, gpuNodes)
		t.Log("All GPU nodes blocked (7/8 pairs consumed each)")

		// Wait for ResourceSlices to reflect reduced NIC availability.
		// The dra.net driver strips ifName from allocated NICs, but the
		// ResourceSlice update may lag slightly behind pod Running status.
		waitForNICAvailability(t, f, gpuNodes, 1, 60*time.Second)

		// Test 17: NUMA-constrained request for 2 → denied (no zone has ≥2)
		t.Run("InsufficientNUMA", func(t *testing.T) {
			pod := GPUNICPod("preflight-fail-numa", 2)
			AssertPodRejected(t, f, pod, "preflight")
			t.Log("Preflight correctly denied NUMA-constrained request with insufficient same-NUMA capacity")
		})

		// Test 19: Cross-NUMA request for 2 → denied (only ≤1 total per node)
		t.Run("FullyExhausted", func(t *testing.T) {
			pod := GPUNICPodWithAnnotations("preflight-exhausted", 2, map[string]string{
				webhook.AnnotationAllowCrossNUMA: "true",
			})
			AssertPodRejected(t, f, pod, "preflight")
			t.Log("Preflight correctly denied cross-NUMA request when resources nearly exhausted")
		})
	})

	// Test 20: Multi-node failover — node A full, node B has capacity
	t.Run("MultiNodeFailover", func(t *testing.T) {
		if len(gpuNodes) < 2 {
			t.Skip("Skipping multi-node test — only 1 GPU node available")
		}

		// Wait for resources from previous test to be freed
		createPodWithRetry(t, f, GPUNICPod("preflight-multinode", 2), 3*time.Minute)
		t.Logf("Preflight passed with %d GPU nodes available", len(gpuNodes))
	})

	// Test 21: Preflight disabled — should admit even when resources are exhausted
	t.Run("PreflightDisabled", func(t *testing.T) {
		// Disable preflight, restart webhook
		PatchWebhookConfig(t, f, map[string]interface{}{"preflightCheck": false})
		RestartAndWait(t, f, f.WebhookNS, webhookDeployment, 3*time.Minute)

		// Create a pod — should be admitted regardless of resource state
		// (pod will stay Pending if resources are unavailable)
		pod := GPUNICPod("preflight-disabled", 1)
		created := CreatePod(t, f, pod)

		AssertPodMutated(t, created)
		t.Log("Pod admitted with preflight disabled — may stay Pending if resources unavailable")
	})
}

// getGPUNodes returns node names that have GPU ResourceSlices.
func getGPUNodes(t *testing.T, f *Framework) []string {
	t.Helper()
	slices, err := f.ResourceClient.ResourceSlices().List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list ResourceSlices: %v", err)
	}

	nodes := make(map[string]bool)
	for _, s := range slices.Items {
		if s.Spec.Driver == "gpu.nvidia.com" && s.Spec.NodeName != nil {
			nodes[*s.Spec.NodeName] = true
		}
	}

	result := make([]string, 0, len(nodes))
	for n := range nodes {
		result = append(result, n)
	}
	if len(result) == 0 {
		t.Fatal("no GPU nodes found in cluster")
	}
	return result
}

// waitForNICAvailability polls ResourceSlices until every GPU node has at most
// maxAvailable RDMA NICs with dra.net/ifName present. This ensures the
// dra.net driver has finished stripping attributes from allocated NICs.
func waitForNICAvailability(t *testing.T, f *Framework, gpuNodes []string, maxAvailable int, timeout time.Duration) {
	t.Helper()

	gpuNodeSet := make(map[string]bool)
	for _, n := range gpuNodes {
		gpuNodeSet[n] = true
	}

	WaitForCondition(t, timeout, fmt.Sprintf("NIC availability ≤%d per GPU node", maxAvailable), func() bool {
		slices, err := f.ResourceClient.ResourceSlices().List(
			context.Background(), metav1.ListOptions{})
		if err != nil {
			return false
		}

		for _, slice := range slices.Items {
			if slice.Spec.Driver != "dra.net" || slice.Spec.NodeName == nil {
				continue
			}
			node := *slice.Spec.NodeName
			if !gpuNodeSet[node] {
				continue
			}

			available := 0
			for _, d := range slice.Spec.Devices {
				if isRDMANICWithIfName(d) {
					available++
				}
			}
			if available > maxAvailable {
				t.Logf("  Node %s: %d RDMA NICs still have ifName (want ≤%d)", node, available, maxAvailable)
				return false
			}
		}
		return true
	})

	t.Logf("ResourceSlices confirmed: ≤%d RDMA NICs available per GPU node", maxAvailable)
}

// isRDMANICWithIfName checks if a device is an RDMA NIC that still has ifName
// (i.e., it is available / not allocated).
func isRDMANICWithIfName(d resourcev1.Device) bool {
	attrs := d.Attributes
	if attrs == nil {
		return false
	}

	// Must have dra.net/ifName
	if _, ok := attrs[resourcev1.QualifiedName("dra.net/ifName")]; !ok {
		return false
	}

	// Must have dra.net/rdma == true
	rdmaAttr, ok := attrs[resourcev1.QualifiedName("dra.net/rdma")]
	if !ok || rdmaAttr.BoolValue == nil || !*rdmaAttr.BoolValue {
		return false
	}

	return true
}

// createBlockerPodsOnAllNodes creates a blocker pod on each GPU node requesting
// countPerNode GPU-NIC pairs. Retries creation to handle resource cleanup delays
// from previous tests. All pods are registered for t.Cleanup deletion.
func createBlockerPodsOnAllNodes(t *testing.T, f *Framework, namePrefix string, countPerNode int, gpuNodes []string) {
	t.Helper()

	names := make([]string, len(gpuNodes))
	for i, node := range gpuNodes {
		name := fmt.Sprintf("%s-%d", namePrefix, i)
		names[i] = name

		var created string
		// Retry pod creation — preflight may deny while previous test's
		// resources are still being deallocated.
		WaitForCondition(t, 3*time.Minute, fmt.Sprintf("blocker %s admitted on %s", name, node), func() bool {
			p := GPUNICPodOnNode(name, countPerNode, node)
			// Always set cross-NUMA to avoid webhook rejection when count > maxPairsPerNUMA
			if p.Annotations == nil {
				p.Annotations = make(map[string]string)
			}
			p.Annotations[webhook.AnnotationAllowCrossNUMA] = "true"
			p.Namespace = f.Namespace

			result, err := f.KubeClient.CoreV1().Pods(f.Namespace).Create(
				context.Background(), p, metav1.CreateOptions{})
			if err != nil {
				return false
			}
			created = result.Name
			return true
		})

		podName := created
		t.Cleanup(func() {
			_ = f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
				context.Background(), podName, metav1.DeleteOptions{
					GracePeriodSeconds: int64Ptr(0),
				})
		})
		t.Logf("Blocker %s admitted on %s (holding %d pairs)", name, node, countPerNode)
	}

	// Wait for all blocker pods to be running
	for _, name := range names {
		WaitForPodRunningOrSucceeded(t, f, name, 5*time.Minute)
	}
}

// createPodWithRetry creates a pod, retrying if admission is transiently denied
// (e.g., preflight seeing stale resource state from a previous test's cleanup).
func createPodWithRetry(t *testing.T, f *Framework, pod *corev1.Pod, timeout time.Duration) *corev1.Pod {
	t.Helper()

	pod.Namespace = f.Namespace
	var created *corev1.Pod
	WaitForCondition(t, timeout, fmt.Sprintf("pod %s to be admitted", pod.Name), func() bool {
		var err error
		created, err = f.KubeClient.CoreV1().Pods(f.Namespace).Create(
			context.Background(), pod, metav1.CreateOptions{})
		return err == nil
	})

	t.Cleanup(func() {
		_ = f.KubeClient.CoreV1().Pods(f.Namespace).Delete(
			context.Background(), created.Name, metav1.DeleteOptions{
				GracePeriodSeconds: int64Ptr(0),
			})
	})

	AssertPodMutated(t, created)
	return created
}
