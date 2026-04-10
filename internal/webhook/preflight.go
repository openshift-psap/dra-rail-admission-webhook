package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
)

// NodeAvailability summarizes available GPU-NIC pairs per NUMA zone on a node.
type NodeAvailability struct {
	NodeName string
	// PairsPerNUMA maps NUMA zone ID to the count of available pairs in that zone.
	PairsPerNUMA map[int]int
	// TotalPairs is the total available pairs across all NUMA zones.
	TotalPairs int
}

// PreflightChecker validates that cluster resources can satisfy a GPU-NIC pair request
// before the pod is admitted. This avoids pods stuck in Pending.
type PreflightChecker struct {
	ResourceClient resourceclient.ResourceV1Interface
}

// CheckAvailability verifies that at least one node can satisfy the requested
// GPU-NIC pair count with the given NUMA constraint.
func (p *PreflightChecker) CheckAvailability(ctx context.Context, count int, numaConstrained bool, cfg Config) error {
	availability, err := p.getClusterAvailability(ctx, cfg)
	if err != nil {
		// On error, log warning but don't block admission — the scheduler
		// will handle it. Preflight is best-effort.
		klog.ErrorS(err, "Preflight check failed to read cluster state, skipping")
		return nil
	}

	if len(availability) == 0 {
		return fmt.Errorf("preflight: no nodes found with GPU-NIC pair resources")
	}

	// Check if any node can satisfy the request
	var reasons []string
	for _, node := range availability {
		if numaConstrained {
			// Need 'count' pairs on a single NUMA zone
			for numaID, available := range node.PairsPerNUMA {
				if available >= count {
					klog.V(3).InfoS("Preflight passed",
						"node", node.NodeName, "numaZone", numaID,
						"available", available, "requested", count)
					return nil
				}
			}
			reasons = append(reasons, fmt.Sprintf(
				"%s: %s", node.NodeName, formatNUMAAvailability(node.PairsPerNUMA)))
		} else {
			// Cross-NUMA: need 'count' pairs total on one node
			if node.TotalPairs >= count {
				klog.V(3).InfoS("Preflight passed (cross-NUMA)",
					"node", node.NodeName,
					"available", node.TotalPairs, "requested", count)
				return nil
			}
			reasons = append(reasons, fmt.Sprintf(
				"%s: %d available", node.NodeName, node.TotalPairs))
		}
	}

	if numaConstrained {
		return fmt.Errorf(
			"preflight: no node has %d available GPU-NIC pairs on a single NUMA zone. "+
				"Per-node availability: %s. "+
				"Set annotation %s=true to allow cross-NUMA allocation",
			count, strings.Join(reasons, "; "), AnnotationAllowCrossNUMA)
	}
	return fmt.Errorf(
		"preflight: no node has %d available GPU-NIC pairs. "+
			"Per-node availability: %s",
		count, strings.Join(reasons, "; "))
}

// getClusterAvailability reads ResourceSlices to determine available GPU-NIC pairs
// per node and NUMA zone. A NIC is considered available if it has the "dra.net/ifName"
// attribute present (allocated NICs have operational attributes stripped by the driver).
func (p *PreflightChecker) getClusterAvailability(ctx context.Context, cfg Config) ([]NodeAvailability, error) {
	// Get NIC ResourceSlices
	nicSlices, err := p.ResourceClient.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list resource slices: %w", err)
	}

	// Build per-node NIC availability: count available NICs per NUMA zone
	// A NIC is available if it has dra.net/ifName AND dra.net/rdma == true
	type nicInfo struct {
		numaZone int
		pcieRoot string
	}
	// nodeAvailableNICs[nodeName] = list of available NIC info
	nodeAvailableNICs := make(map[string][]nicInfo)

	for _, slice := range nicSlices.Items {
		if slice.Spec.Driver != "dra.net" {
			continue
		}
		nodeName := ""
		if slice.Spec.NodeName != nil {
			nodeName = *slice.Spec.NodeName
		}
		if nodeName == "" {
			continue
		}

		for _, device := range slice.Spec.Devices {
			if !isNICAvailable(device, cfg) {
				continue
			}

			numa := getNUMAZone(device)
			pcie := getPCIeRoot(device)
			nodeAvailableNICs[nodeName] = append(nodeAvailableNICs[nodeName], nicInfo{
				numaZone: numa,
				pcieRoot: pcie,
			})
		}
	}

	// Build per-node GPU availability: count GPUs with matching PCIe roots
	nodeAvailableGPUPCIeRoots := make(map[string]map[string]bool) // node -> set of PCIe roots with available GPUs

	for _, slice := range nicSlices.Items {
		if slice.Spec.Driver != cfg.GPUDeviceClassName {
			continue
		}
		nodeName := ""
		if slice.Spec.NodeName != nil {
			nodeName = *slice.Spec.NodeName
		}
		if nodeName == "" {
			continue
		}

		if nodeAvailableGPUPCIeRoots[nodeName] == nil {
			nodeAvailableGPUPCIeRoots[nodeName] = make(map[string]bool)
		}

		for _, device := range slice.Spec.Devices {
			pcie := getPCIeRoot(device)
			if pcie != "" {
				nodeAvailableGPUPCIeRoots[nodeName][pcie] = true
			}
		}
	}

	// Combine: a GPU-NIC pair is available when an available NIC has a
	// matching GPU on the same PCIe root
	var result []NodeAvailability
	for nodeName, nics := range nodeAvailableNICs {
		gpuRoots := nodeAvailableGPUPCIeRoots[nodeName]

		nodeAvail := NodeAvailability{
			NodeName:     nodeName,
			PairsPerNUMA: make(map[int]int),
		}

		for _, nic := range nics {
			// If we have GPU PCIe root data, verify there's a GPU on the same root
			if gpuRoots != nil && !gpuRoots[nic.pcieRoot] {
				continue
			}
			nodeAvail.PairsPerNUMA[nic.numaZone]++
			nodeAvail.TotalPairs++
		}

		if nodeAvail.TotalPairs > 0 {
			result = append(result, nodeAvail)
		}
	}

	return result, nil
}

// isNICAvailable checks if a NIC device is available (not allocated to another pod).
// Available NICs have operational attributes (ifName, ipv4, state, type) present.
// Allocated NICs are stripped to identification-only attributes by the dra.net driver.
func isNICAvailable(device resourcev1.Device, cfg Config) bool {
	attrs := device.Attributes

	// Must have dra.net/ifName — this is the primary availability indicator.
	// When a NIC is allocated, the driver strips ifName along with other
	// operational attributes.
	if _, ok := attrs[resourcev1.QualifiedName("dra.net/ifName")]; !ok {
		return false
	}

	// Must have dra.net/rdma if RDMA is required
	if cfg.NICConfig.RDMARequired {
		rdmaAttr, ok := attrs[resourcev1.QualifiedName("dra.net/rdma")]
		if !ok || rdmaAttr.BoolValue == nil || !*rdmaAttr.BoolValue {
			return false
		}
	}

	return true
}

// getNUMAZone extracts the NUMA zone from a device's attributes.
// Returns -1 if not found.
func getNUMAZone(device resourcev1.Device) int {
	attr, ok := device.Attributes[resourcev1.QualifiedName("dra.net/numaNode")]
	if !ok || attr.IntValue == nil {
		return -1
	}
	return int(*attr.IntValue)
}

// getPCIeRoot extracts the PCIe root from a device's attributes.
func getPCIeRoot(device resourcev1.Device) string {
	attr, ok := device.Attributes[resourcev1.QualifiedName(PCIeRootAttribute)]
	if !ok || attr.StringValue == nil {
		return ""
	}
	return *attr.StringValue
}

func formatNUMAAvailability(pairsPerNUMA map[int]int) string {
	if len(pairsPerNUMA) == 0 {
		return "no NUMA zones"
	}

	// Sort NUMA IDs for consistent output
	ids := make([]int, 0, len(pairsPerNUMA))
	for id := range pairsPerNUMA {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("NUMA %d: %d available", id, pairsPerNUMA[id]))
	}
	return strings.Join(parts, ", ")
}
