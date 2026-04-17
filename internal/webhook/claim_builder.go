package webhook

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NICParameters is the opaque configuration passed to the dra.net driver.
type NICParameters struct {
	Interface NICInterface `json:"interface"`
	Rules     []Rule       `json:"rules,omitempty"`
	Routes    []Route      `json:"routes,omitempty"`
}

// NICInterface holds per-NIC interface config.
type NICInterface struct {
	Name string `json:"name"`
	MTU  int    `json:"mtu"`
}

// buildNICSelectors returns CEL device selectors for a NIC.
// railIndex identifies which configured rail to pin to (-1 means no rail).
// When a rail is selected, the RDMA check and IPv4 prefix are combined into
// a single CEL expression (matching the cluster template pattern). Otherwise,
// only the RDMA selector is applied when required.
func buildNICSelectors(railIndex int, cfg Config) []resourcev1.DeviceSelector {
	var selectors []resourcev1.DeviceSelector

	hasRail := railIndex >= 0 && railIndex < len(cfg.NICConfig.Rails)

	if hasRail {
		rail := cfg.NICConfig.Rails[railIndex]
		// Combined RDMA + subnet selector (matches cluster template pattern)
		if cfg.NICConfig.RDMARequired {
			selectors = append(selectors, resourcev1.DeviceSelector{
				CEL: &resourcev1.CELDeviceSelector{
					Expression: fmt.Sprintf(
						`device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith(%q)`,
						rail.IPv4Prefix,
					),
				},
			})
		} else {
			selectors = append(selectors, resourcev1.DeviceSelector{
				CEL: &resourcev1.CELDeviceSelector{
					Expression: fmt.Sprintf(
						`device.attributes["dra.net"].ipv4.startsWith(%q)`,
						rail.IPv4Prefix,
					),
				},
			})
		}
	} else if cfg.NICConfig.RDMARequired {
		selectors = append(selectors, resourcev1.DeviceSelector{
			CEL: &resourcev1.CELDeviceSelector{
				Expression: `device.attributes["dra.net"].rdma == true`,
			},
		})
	}

	return selectors
}

// BuildClaimTemplateSpec builds the ResourceClaimSpec for N GPU+NIC pairs.
// railIndices maps each NIC position to a configured rail index (e.g., [2, 5]
// means nic-0 uses rail 2, nic-1 uses rail 5). When rails are not configured,
// railIndices should be nil.
// When numaConstrained is true, a matchAttribute constraint on dra.net/numaNode
// is added across all NIC requests.
func BuildClaimTemplateSpec(count int, numaConstrained bool, cfg Config, railIndices []int) (resourcev1.ResourceClaimSpec, error) {
	requests := make([]resourcev1.DeviceRequest, 0, count*2)
	constraints := make([]resourcev1.DeviceConstraint, 0, count+1)
	configs := make([]resourcev1.DeviceClaimConfiguration, 0, count)

	nicRequests := make([]string, 0, count)

	for i := 0; i < count; i++ {
		gpuName := fmt.Sprintf("gpu-%d", i)
		nicName := fmt.Sprintf("nic-%d", i)
		nicRequests = append(nicRequests, nicName)

		// GPU request
		requests = append(requests, resourcev1.DeviceRequest{
			Name: gpuName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: cfg.GPUDeviceClassName,
				Count:           1,
				AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
			},
		})

		// Determine rail index for this NIC position
		railIdx := -1
		if len(railIndices) > i {
			railIdx = railIndices[i]
		}

		// NIC request with selectors
		nicReq := resourcev1.DeviceRequest{
			Name: nicName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: cfg.NICDeviceClassName,
				Count:           1,
				AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
			},
		}
		nicSelectors := buildNICSelectors(railIdx, cfg)
		if len(nicSelectors) > 0 {
			nicReq.Exactly.Selectors = nicSelectors
		}
		requests = append(requests, nicReq)

		// PCIe root pairing constraint
		pcieRoot := resourcev1.FullyQualifiedName(PCIeRootAttribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			Requests:       []string{gpuName, nicName},
			MatchAttribute: &pcieRoot,
		})

		// NIC opaque config
		nicParams := buildNICParameters(i, railIdx, cfg)
		paramsJSON, err := json.Marshal(nicParams)
		if err != nil {
			return resourcev1.ResourceClaimSpec{}, fmt.Errorf("failed to marshal NIC parameters for %s: %w", nicName, err)
		}

		configs = append(configs, resourcev1.DeviceClaimConfiguration{
			Requests: []string{nicName},
			DeviceConfiguration: resourcev1.DeviceConfiguration{
				Opaque: &resourcev1.OpaqueDeviceConfiguration{
					Driver:     "dra.net",
					Parameters: runtime.RawExtension{Raw: paramsJSON},
				},
			},
		})
	}

	// NUMA co-location constraint across all NICs
	if numaConstrained && count > 1 {
		numaAttr := resourcev1.FullyQualifiedName(NUMANodeAttribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			Requests:       nicRequests,
			MatchAttribute: &numaAttr,
		})
	}

	return resourcev1.ResourceClaimSpec{
		Devices: resourcev1.DeviceClaim{
			Requests:    requests,
			Constraints: constraints,
			Config:      configs,
		},
	}, nil
}

// buildNICParameters creates the opaque driver parameters for a single NIC.
// nicIndex is the position (0, 1, ...) used for interface naming (net0, net1).
// railIndex identifies which configured rail to use for routing (-1 means no rail).
// The table ID is derived from the rail index so that policy routing tables
// correspond to subnets, not NIC positions.
func buildNICParameters(nicIndex int, railIndex int, cfg Config) NICParameters {
	params := NICParameters{
		Interface: NICInterface{
			Name: fmt.Sprintf("%s%d", cfg.NICConfig.InterfacePrefix, nicIndex),
			MTU:  cfg.NICConfig.MTU,
		},
	}

	if railIndex >= 0 && railIndex < len(cfg.NICConfig.Rails) {
		rail := cfg.NICConfig.Rails[railIndex]
		tableID := cfg.NICConfig.StartingTableID + railIndex

		// Source-based policy routing rule
		params.Rules = []Rule{
			{
				Source:   rail.Subnet,
				Table:    tableID,
				Priority: defaultRulePriority,
			},
		}

		// Own subnet link-scope route in policy table
		params.Routes = append(params.Routes, Route{
			Destination: rail.Subnet,
			Scope:       253,
			Table:       tableID,
		})

		// Cross-subnet routes to all other rails via this rail's gateway
		for j, otherRail := range cfg.NICConfig.Rails {
			if j == railIndex {
				continue
			}
			params.Routes = append(params.Routes, Route{
				Destination: otherRail.Subnet,
				Gateway:     rail.Gateway,
			})
		}

		// Default route in policy table
		params.Routes = append(params.Routes, Route{
			Destination: "0.0.0.0/0",
			Gateway:     rail.Gateway,
			Table:       tableID,
		})
	} else {
		// Legacy flat routing (no rails)
		tableID := cfg.NICConfig.StartingTableID + nicIndex

		if cfg.NICConfig.SourceSubnet != "" {
			params.Rules = []Rule{
				{
					Source:   cfg.NICConfig.SourceSubnet,
					Table:    tableID,
					Priority: tableID,
				},
			}
		}

		for _, r := range cfg.NICConfig.Routes {
			route := Route{
				Destination: r.Destination,
				Gateway:     r.Gateway,
				Table:       tableID,
			}
			params.Routes = append(params.Routes, route)
		}
	}

	return params
}

// BuildSinglePairClaimSpec builds a ResourceClaimSpec for one GPU+NIC pair.
// nicIndex is the position (0, 1, ...) used for interface naming (net0, net1).
// railIndex identifies which configured rail to use for routing and CEL selection.
func BuildSinglePairClaimSpec(nicIndex int, railIndex int, cfg Config) (resourcev1.ResourceClaimSpec, error) {
	gpuReq := resourcev1.DeviceRequest{
		Name: "gpu",
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: cfg.GPUDeviceClassName,
			Count:           1,
			AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
		},
	}

	nicReq := resourcev1.DeviceRequest{
		Name: "nic",
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: cfg.NICDeviceClassName,
			Count:           1,
			AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
		},
	}
	nicSelectors := buildNICSelectors(railIndex, cfg)
	if len(nicSelectors) > 0 {
		nicReq.Exactly.Selectors = nicSelectors
	}

	// PCIe root pairing constraint
	pcieRoot := resourcev1.FullyQualifiedName(PCIeRootAttribute)
	constraint := resourcev1.DeviceConstraint{
		Requests:       []string{"gpu", "nic"},
		MatchAttribute: &pcieRoot,
	}

	// NIC opaque config
	nicParams := buildNICParameters(nicIndex, railIndex, cfg)
	paramsJSON, err := json.Marshal(nicParams)
	if err != nil {
		return resourcev1.ResourceClaimSpec{}, fmt.Errorf("failed to marshal NIC parameters: %w", err)
	}

	nicConfig := resourcev1.DeviceClaimConfiguration{
		Requests: []string{"nic"},
		DeviceConfiguration: resourcev1.DeviceConfiguration{
			Opaque: &resourcev1.OpaqueDeviceConfiguration{
				Driver:     "dra.net",
				Parameters: runtime.RawExtension{Raw: paramsJSON},
			},
		},
	}

	return resourcev1.ResourceClaimSpec{
		Devices: resourcev1.DeviceClaim{
			Requests:    []resourcev1.DeviceRequest{gpuReq, nicReq},
			Constraints: []resourcev1.DeviceConstraint{constraint},
			Config:      []resourcev1.DeviceClaimConfiguration{nicConfig},
		},
	}, nil
}

// SinglePairTemplateName returns a deterministic name for a single-pair
// ResourceClaimTemplate based on NIC position, rail index, and config.
func SinglePairTemplateName(nicIndex int, railIndex int, cfg Config) string {
	h := sha256.New()
	data, _ := json.Marshal(cfg)
	h.Write(data)
	_, _ = fmt.Fprintf(h, "nic:%d:rail:%d", nicIndex, railIndex)
	hash := fmt.Sprintf("%x", h.Sum(nil))[:8]
	return fmt.Sprintf("gpu-nic-pair-%d-rail%d-%s", nicIndex, railIndex, hash)
}

// TemplateName returns a deterministic name for a ResourceClaimTemplate
// based on count, NUMA mode, config, and selected rail indices.
func TemplateName(count int, numaConstrained bool, cfg Config, railIndices []int) string {
	numaStr := "numa"
	if !numaConstrained {
		numaStr = "xnuma"
	}

	// Hash the config + rail indices to make the name unique
	h := sha256.New()
	data, _ := json.Marshal(cfg)
	h.Write(data)
	railData, _ := json.Marshal(railIndices)
	h.Write(railData)
	hash := fmt.Sprintf("%x", h.Sum(nil))[:8]

	return fmt.Sprintf("gpu-nic-%d-%s-%s", count, numaStr, hash)
}
