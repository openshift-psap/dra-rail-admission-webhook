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

// BuildClaimTemplateSpec builds the ResourceClaimSpec for N GPU+NIC pairs.
// When numaConstrained is true, a matchAttribute constraint on dra.net/numaNode
// is added across all NIC requests.
func BuildClaimTemplateSpec(count int, numaConstrained bool, cfg Config) (resourcev1.ResourceClaimSpec, error) {
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

		// NIC request with selectors
		nicReq := resourcev1.DeviceRequest{
			Name: nicName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: cfg.NICDeviceClassName,
				Count:           1,
				AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
			},
		}
		if cfg.NICConfig.RDMARequired {
			nicReq.Exactly.Selectors = []resourcev1.DeviceSelector{
				{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: `device.attributes["dra.net"].rdma == true`,
					},
				},
			}
		}
		requests = append(requests, nicReq)

		// PCIe root pairing constraint
		pcieRoot := resourcev1.FullyQualifiedName(PCIeRootAttribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			Requests:       []string{gpuName, nicName},
			MatchAttribute: &pcieRoot,
		})

		// NIC opaque config
		nicParams := buildNICParameters(i, cfg)
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
func buildNICParameters(index int, cfg Config) NICParameters {
	params := NICParameters{
		Interface: NICInterface{
			Name: fmt.Sprintf("%s%d", cfg.NICConfig.InterfacePrefix, index),
			MTU:  cfg.NICConfig.MTU,
		},
	}

	tableID := cfg.NICConfig.StartingTableID + index

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

	return params
}

// TemplateName returns a deterministic name for a ResourceClaimTemplate
// based on count, NUMA mode, and a hash of the config.
func TemplateName(count int, numaConstrained bool, cfg Config) string {
	numaStr := "numa"
	if !numaConstrained {
		numaStr = "xnuma"
	}

	// Hash the config to make the name unique per config revision
	h := sha256.New()
	data, _ := json.Marshal(cfg)
	h.Write(data)
	hash := fmt.Sprintf("%x", h.Sum(nil))[:8]

	return fmt.Sprintf("gpu-nic-%d-%s-%s", count, numaStr, hash)
}
