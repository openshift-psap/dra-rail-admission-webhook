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

// SelectAvailableRails reads ResourceSlices to determine which configured rails
// have free NICs and returns rail indices that can satisfy the request. This
// prevents the webhook from always pinning to rails 0..N-1, which would strand
// NICs on other subnets.
//
// On any error, it falls back to sequential indices 0..count-1 so that the
// scheduler can still attempt allocation.
// SelectAvailableRails reads ResourceSlices to find rails with free NICs,
// excluding any rails in claimedRails (already assigned to other pods in
// the same namespace). Falls back to sequential indices on error.
func (p *PreflightChecker) SelectAvailableRails(ctx context.Context, count int, numaConstrained bool, cfg Config, claimedRails map[int]bool) []int {
	if len(cfg.NICConfig.Rails) == 0 {
		return sequentialRails(count)
	}

	railIndices, err := p.findAvailableRails(ctx, count, numaConstrained, cfg, claimedRails)
	if err != nil {
		klog.ErrorS(err, "Rail selection failed, falling back to sequential assignment")
		return sequentialRails(count)
	}

	klog.V(2).InfoS("Selected available rails", "railIndices", railIndices)
	return railIndices
}

// findAvailableRails checks ResourceSlices for NICs with ipv4 attributes
// (indicating they're unallocated) and maps them to configured rails.
func (p *PreflightChecker) findAvailableRails(ctx context.Context, count int, numaConstrained bool, cfg Config, claimedRails map[int]bool) ([]int, error) {
	slices, err := p.ResourceClient.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list resource slices: %w", err)
	}

	// Build prefix → rail index map from config
	prefixToRail := make(map[string]int, len(cfg.NICConfig.Rails))
	for i, rail := range cfg.NICConfig.Rails {
		prefixToRail[rail.IPv4Prefix] = i
	}

	// Collect available rails per node per NUMA zone
	type nodeRails struct {
		perNUMA map[int][]int // numaZone → list of available rail indices
		all     []int         // all available rail indices (for cross-NUMA)
	}
	nodes := make(map[string]*nodeRails)

	for _, slice := range slices.Items {
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
			// Must have ipv4 (absent = allocated) and rdma if required
			ipv4 := getIPv4(device)
			if ipv4 == "" {
				continue
			}
			if cfg.NICConfig.RDMARequired {
				rdmaAttr, ok := device.Attributes[resourcev1.QualifiedName("dra.net/rdma")]
				if !ok || rdmaAttr.BoolValue == nil || !*rdmaAttr.BoolValue {
					continue
				}
			}

			// Match ipv4 to a configured rail
			railIdx := -1
			for prefix, idx := range prefixToRail {
				if strings.HasPrefix(ipv4, prefix) {
					railIdx = idx
					break
				}
			}
			if railIdx < 0 {
				continue // NIC on a subnet not in our rail config
			}

			numa := getNUMAZone(device)

			if nodes[nodeName] == nil {
				nodes[nodeName] = &nodeRails{perNUMA: make(map[int][]int)}
			}
			nodes[nodeName].perNUMA[numa] = append(nodes[nodeName].perNUMA[numa], railIdx)
			nodes[nodeName].all = append(nodes[nodeName].all, railIdx)
		}
	}

	// First pass: try to find rails that are NOT already claimed by existing
	// templates. This prevents concurrent pods from colliding on the same rail.
	for nodeName, nr := range nodes {
		if numaConstrained {
			for numaID, rails := range nr.perNUMA {
				unclaimed := filterRails(rails, claimedRails)
				if len(unclaimed) >= count {
					klog.V(3).InfoS("Found unclaimed available rails",
						"node", nodeName, "numaZone", numaID,
						"available", len(unclaimed), "selected", unclaimed[:count])
					return unclaimed[:count], nil
				}
			}
		} else {
			unclaimed := filterRails(nr.all, claimedRails)
			if len(unclaimed) >= count {
				klog.V(3).InfoS("Found unclaimed available rails (cross-NUMA)",
					"node", nodeName,
					"available", len(unclaimed), "selected", unclaimed[:count])
				return unclaimed[:count], nil
			}
		}
	}

	// Fallback: ignore claimed rails. This handles multi-node scenarios
	// where a rail is "claimed" on one node but free on another.
	for nodeName, nr := range nodes {
		if numaConstrained {
			for numaID, rails := range nr.perNUMA {
				if len(rails) >= count {
					klog.V(3).InfoS("Falling back to claimed rails",
						"node", nodeName, "numaZone", numaID,
						"available", len(rails), "selected", rails[:count])
					return rails[:count], nil
				}
			}
		} else {
			if len(nr.all) >= count {
				klog.V(3).InfoS("Falling back to claimed rails (cross-NUMA)",
					"node", nodeName,
					"available", len(nr.all), "selected", nr.all[:count])
				return nr.all[:count], nil
			}
		}
	}

	return nil, fmt.Errorf("no node has %d available rails", count)
}

// getIPv4 extracts the IPv4 address string from a device's attributes.
// Returns "" if not present (device is allocated).
func getIPv4(device resourcev1.Device) string {
	attr, ok := device.Attributes[resourcev1.QualifiedName("dra.net/ipv4")]
	if !ok || attr.StringValue == nil {
		return ""
	}
	return *attr.StringValue
}

// sequentialRails returns [0, 1, ..., count-1].
func sequentialRails(count int) []int {
	rails := make([]int, count)
	for i := range rails {
		rails[i] = i
	}
	return rails
}

// filterRails returns elements of rails that are not in the exclude set.
func filterRails(rails []int, exclude map[int]bool) []int {
	var result []int
	for _, r := range rails {
		if !exclude[r] {
			result = append(result, r)
		}
	}
	return result
}

// CheckExplicitAvailability verifies that at least one node has enough
// available mapped device pairs to satisfy the request in explicit mode.
func (p *PreflightChecker) CheckExplicitAvailability(ctx context.Context, count int, cfg Config) error {
	if cfg.PairingConfig == nil {
		return fmt.Errorf("preflight: explicit pairing config not set")
	}

	slices, err := p.ResourceClient.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.ErrorS(err, "Preflight explicit check failed to read cluster state, skipping")
		return nil
	}

	// Build per-node per-driver device availability
	type deviceKey struct {
		driver string
		value  string
	}
	nodeDevices := make(map[string]map[deviceKey]bool)

	for _, slice := range slices.Items {
		nodeName := ""
		if slice.Spec.NodeName != nil {
			nodeName = *slice.Spec.NodeName
		}
		if nodeName == "" {
			continue
		}

		if nodeDevices[nodeName] == nil {
			nodeDevices[nodeName] = make(map[deviceKey]bool)
		}

		for _, device := range slice.Spec.Devices {
			for role, sel := range cfg.PairingConfig.DeviceSelectors {
				if slice.Spec.Driver != sel.DeviceClassName {
					continue
				}
				qualName := resourcev1.QualifiedName(sel.AttributeDomain + "/" + sel.AttributeName)
				attr, ok := device.Attributes[qualName]
				if !ok || attr.StringValue == nil {
					continue
				}
				if role == "nic" {
					if _, hasIF := device.Attributes[resourcev1.QualifiedName("dra.net/ifName")]; !hasIF {
						continue
					}
					if cfg.NICConfig.RDMARequired {
						rdmaAttr, ok := device.Attributes[resourcev1.QualifiedName("dra.net/rdma")]
						if !ok || rdmaAttr.BoolValue == nil || !*rdmaAttr.BoolValue {
							continue
						}
					}
				}
				nodeDevices[nodeName][deviceKey{driver: sel.DeviceClassName, value: *attr.StringValue}] = true
			}
		}
	}

	// Check each pool's pairs against node availability
	for _, pool := range cfg.PairingConfig.NodePools {
		for nodeName, devices := range nodeDevices {
			availCount := 0
			for _, pair := range pool.Pairs {
				allAvail := true
				for role, deviceID := range pair.Devices {
					sel := cfg.PairingConfig.DeviceSelectors[role]
					if !devices[deviceKey{driver: sel.DeviceClassName, value: deviceID}] {
						allAvail = false
						break
					}
				}
				if allAvail {
					availCount++
				}
			}
			if availCount >= count {
				klog.V(3).InfoS("Preflight explicit check passed",
					"node", nodeName, "pool", pool.NodePoolLabel,
					"available", availCount, "requested", count)
				return nil
			}
		}
	}

	return fmt.Errorf("preflight: no node has %d available explicit device pairs", count)
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
