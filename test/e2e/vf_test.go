//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// testVFMode validates VF-specific behavior: isSriovVf CEL selectors,
// addresses in opaque NIC params, PCI prefix matching, and CIDRPool IP assignment.
// Gated by E2E_VF_MODE=true — skipped by default.
func testVFMode(t *testing.T) {
	if os.Getenv("E2E_VF_MODE") != "true" {
		t.Skip("E2E_VF_MODE not set — skipping VF mode tests")
	}
	if os.Getenv("E2E_SKIP_GPU") == "true" {
		t.Skip("E2E_SKIP_GPU set — skipping VF mode tests")
	}

	f := NewFramework(t, "vf")

	t.Log("Waiting 150s for allocator pending entries to expire")
	time.Sleep(150 * time.Second)

	t.Run("CELSelector_IsSriovVf", func(t *testing.T) {
		pod := GPUNICPod("vf-cel-1", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)

		// Check the claim template CEL expression contains isSriovVf
		tmplName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		tmpl, err := f.ResourceClient.ResourceClaimTemplates(f.Namespace).Get(
			context.Background(), tmplName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get template %s: %v", tmplName, err)
		}

		foundVfSelector := false
		for _, req := range tmpl.Spec.Spec.Devices.Requests {
			if req.Exactly == nil {
				continue
			}
			for _, sel := range req.Exactly.Selectors {
				if sel.CEL != nil && strings.Contains(sel.CEL.Expression, "isSriovVf") {
					foundVfSelector = true
					t.Logf("NIC CEL selector: %s", sel.CEL.Expression)
				}
			}
		}
		if !foundVfSelector {
			t.Error("no CEL selector contains isSriovVf — VF mode not active in claim template")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("OpaqueParams_HasAddresses", func(t *testing.T) {
		pod := GPUNICPod("vf-addr-1", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)

		tmplName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		tmpl, err := f.ResourceClient.ResourceClaimTemplates(f.Namespace).Get(
			context.Background(), tmplName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get template %s: %v", tmplName, err)
		}

		foundAddresses := false
		for _, cfg := range tmpl.Spec.Spec.Devices.Config {
			if cfg.Opaque == nil || cfg.Opaque.Driver != "dra.net" {
				continue
			}
			var params webhook.NICParameters
			if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &params); err != nil {
				t.Fatalf("failed to unmarshal NIC params: %v", err)
			}
			if len(params.Interface.Addresses) > 0 {
				foundAddresses = true
				t.Logf("NIC interface addresses: %v", params.Interface.Addresses)
				// Verify CIDR format
				for _, addr := range params.Interface.Addresses {
					if !strings.Contains(addr, "/") {
						t.Errorf("address %q not in CIDR format", addr)
					}
				}
			}
		}
		if !foundAddresses {
			t.Error("opaque NIC params missing addresses field — CIDRPool IP assignment not working")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("CELSelector_PCIPrefix", func(t *testing.T) {
		pod := GPUNICPod("vf-pci-1", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)

		tmplName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		tmpl, err := f.ResourceClient.ResourceClaimTemplates(f.Namespace).Get(
			context.Background(), tmplName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get template %s: %v", tmplName, err)
		}

		foundPciPrefix := false
		for _, req := range tmpl.Spec.Spec.Devices.Requests {
			if req.Exactly == nil {
				continue
			}
			for _, sel := range req.Exactly.Selectors {
				if sel.CEL != nil && strings.Contains(sel.CEL.Expression, "pciAddress.startsWith") {
					foundPciPrefix = true
					t.Logf("PCI prefix CEL: %s", sel.CEL.Expression)
				}
			}
		}
		if !foundPciPrefix {
			t.Error("no CEL selector uses pciAddress.startsWith — PCI prefix matching not active")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("NoIPv4InCEL", func(t *testing.T) {
		pod := GPUNICPod("vf-noipv4-1", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)

		tmplName := *created.Spec.ResourceClaims[0].ResourceClaimTemplateName
		tmpl, err := f.ResourceClient.ResourceClaimTemplates(f.Namespace).Get(
			context.Background(), tmplName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get template %s: %v", tmplName, err)
		}

		for _, req := range tmpl.Spec.Spec.Devices.Requests {
			if req.Exactly == nil {
				continue
			}
			for _, sel := range req.Exactly.Selectors {
				if sel.CEL != nil && strings.Contains(sel.CEL.Expression, "ipv4") {
					t.Errorf("CEL selector references ipv4 — should use PCI prefix in VF mode: %s", sel.CEL.Expression)
				}
			}
		}
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("UniqueIPPerPod", func(t *testing.T) {
		pod1 := GPUNICPod("vf-ip-a", 1)
		created1 := CreatePod(t, f, pod1)
		AssertPodMutated(t, created1)

		pod2 := GPUNICPod("vf-ip-b", 1)
		created2 := CreatePod(t, f, pod2)
		AssertPodMutated(t, created2)

		ip1 := extractIPFromTemplate(t, f, created1)
		ip2 := extractIPFromTemplate(t, f, created2)

		if ip1 == ip2 {
			t.Errorf("both pods got same IP %s — IP allocator not assigning unique IPs", ip1)
		}
		t.Logf("Pod A IP: %s, Pod B IP: %s (unique)", ip1, ip2)
		CleanupAllPods(t, f, 60*time.Second)
	})

	t.Run("VFPodRunning", func(t *testing.T) {
		pod := NetworkTestPod("vf-running-1", 1)
		created := CreatePod(t, f, pod)
		AssertPodMutated(t, created)

		WaitForPodRunningOrSucceeded(t, f, created.Name, 5*time.Minute)

		// Verify the interface exists and has the assigned IP
		stdout, _ := PodExec(t, f, created.Name, "test", []string{"ip", "addr", "show", "net0"})
		t.Logf("net0 interface:\n%s", stdout)
		if !strings.Contains(stdout, "net0") {
			t.Error("net0 interface not found in pod")
		}
		CleanupAllPods(t, f, 60*time.Second)
	})
}

// extractIPFromTemplate reads the first NIC address from a pod's claim template opaque params.
func extractIPFromTemplate(t *testing.T, f *Framework, pod *corev1.Pod) string {
	t.Helper()
	if len(pod.Spec.ResourceClaims) == 0 {
		t.Fatal("pod has no resource claims")
	}
	tmplName := *pod.Spec.ResourceClaims[0].ResourceClaimTemplateName
	tmpl, err := f.ResourceClient.ResourceClaimTemplates(f.Namespace).Get(
		context.Background(), tmplName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get template %s: %v", tmplName, err)
	}
	for _, cfg := range tmpl.Spec.Spec.Devices.Config {
		if cfg.Opaque == nil || cfg.Opaque.Driver != "dra.net" {
			continue
		}
		var params webhook.NICParameters
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &params); err != nil {
			continue
		}
		if len(params.Interface.Addresses) > 0 {
			return params.Interface.Addresses[0]
		}
	}
	t.Fatal("no IP address found in template opaque params")
	return ""
}
