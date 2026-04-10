package webhook

import (
	"encoding/json"
	"testing"
)

func testConfig() Config {
	return Config{
		MaxPairsPerNUMA:    4,
		MaxPairsPerNode:    8,
		GPUDeviceClassName: "gpu.nvidia.com",
		NICDeviceClassName: "dranet",
		NICConfig: NICConfig{
			MTU:             9000,
			RDMARequired:    true,
			InterfacePrefix: "net",
			SourceSubnet:    "10.0.0.0/16",
			StartingTableID: 100,
			Routes: []Route{
				{Destination: "0.0.0.0/0", Gateway: "10.0.0.1"},
			},
		},
	}
}

func TestBuildClaimTemplateSpec_SinglePair(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(1, true, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have 2 requests: gpu-0 and nic-0
	if len(spec.Devices.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(spec.Devices.Requests))
	}

	if spec.Devices.Requests[0].Name != "gpu-0" {
		t.Errorf("expected first request name 'gpu-0', got %q", spec.Devices.Requests[0].Name)
	}
	if spec.Devices.Requests[1].Name != "nic-0" {
		t.Errorf("expected second request name 'nic-0', got %q", spec.Devices.Requests[1].Name)
	}

	// GPU request details
	gpu := spec.Devices.Requests[0].Exactly
	if gpu.DeviceClassName != "gpu.nvidia.com" {
		t.Errorf("GPU device class = %q, want gpu.nvidia.com", gpu.DeviceClassName)
	}
	if gpu.Count != 1 {
		t.Errorf("GPU count = %d, want 1", gpu.Count)
	}

	// NIC request details
	nic := spec.Devices.Requests[1].Exactly
	if nic.DeviceClassName != "dranet" {
		t.Errorf("NIC device class = %q, want dranet", nic.DeviceClassName)
	}
	if len(nic.Selectors) != 1 {
		t.Fatalf("expected 1 NIC selector, got %d", len(nic.Selectors))
	}
	if nic.Selectors[0].CEL == nil {
		t.Fatal("expected CEL selector for NIC")
	}

	// Should have 1 PCIe constraint only (no NUMA for single pair)
	if len(spec.Devices.Constraints) != 1 {
		t.Fatalf("expected 1 constraint for single pair, got %d", len(spec.Devices.Constraints))
	}

	constraint := spec.Devices.Constraints[0]
	if len(constraint.Requests) != 2 || constraint.Requests[0] != "gpu-0" || constraint.Requests[1] != "nic-0" {
		t.Errorf("PCIe constraint requests = %v, want [gpu-0 nic-0]", constraint.Requests)
	}
	if string(*constraint.MatchAttribute) != PCIeRootAttribute {
		t.Errorf("PCIe constraint attribute = %q, want %q", *constraint.MatchAttribute, PCIeRootAttribute)
	}

	// Should have 1 config entry for NIC
	if len(spec.Devices.Config) != 1 {
		t.Fatalf("expected 1 config entry, got %d", len(spec.Devices.Config))
	}
}

func TestBuildClaimTemplateSpec_FourPairsWithNUMA(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(4, true, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 8 requests: 4 GPUs + 4 NICs
	if len(spec.Devices.Requests) != 8 {
		t.Fatalf("expected 8 requests, got %d", len(spec.Devices.Requests))
	}

	// 5 constraints: 4 PCIe + 1 NUMA
	if len(spec.Devices.Constraints) != 5 {
		t.Fatalf("expected 5 constraints, got %d", len(spec.Devices.Constraints))
	}

	// Last constraint should be NUMA
	numaConstraint := spec.Devices.Constraints[4]
	if string(*numaConstraint.MatchAttribute) != NUMANodeAttribute {
		t.Errorf("NUMA constraint attribute = %q, want %q", *numaConstraint.MatchAttribute, NUMANodeAttribute)
	}
	if len(numaConstraint.Requests) != 4 {
		t.Errorf("NUMA constraint should reference 4 NICs, got %d", len(numaConstraint.Requests))
	}
	for i, req := range numaConstraint.Requests {
		expected := "nic-" + string(rune('0'+i))
		if req != expected {
			t.Errorf("NUMA constraint request[%d] = %q, want %q", i, req, expected)
		}
	}

	// 4 config entries for NICs
	if len(spec.Devices.Config) != 4 {
		t.Fatalf("expected 4 config entries, got %d", len(spec.Devices.Config))
	}
}

func TestBuildClaimTemplateSpec_CrossNUMA(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(6, false, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 12 requests: 6 GPUs + 6 NICs
	if len(spec.Devices.Requests) != 12 {
		t.Fatalf("expected 12 requests, got %d", len(spec.Devices.Requests))
	}

	// 6 constraints: 6 PCIe only (no NUMA)
	if len(spec.Devices.Constraints) != 6 {
		t.Fatalf("expected 6 constraints (PCIe only, no NUMA), got %d", len(spec.Devices.Constraints))
	}

	// Verify all constraints are PCIe, not NUMA
	for _, c := range spec.Devices.Constraints {
		if string(*c.MatchAttribute) == NUMANodeAttribute {
			t.Error("should not have NUMA constraint when numaConstrained=false")
		}
	}
}

func TestBuildClaimTemplateSpec_NICConfig(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(2, true, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Config) != 2 {
		t.Fatalf("expected 2 config entries, got %d", len(spec.Devices.Config))
	}

	// Verify first NIC config
	nicCfg := spec.Devices.Config[0]
	if len(nicCfg.Requests) != 1 || nicCfg.Requests[0] != "nic-0" {
		t.Errorf("config[0] requests = %v, want [nic-0]", nicCfg.Requests)
	}
	if nicCfg.Opaque == nil {
		t.Fatal("expected opaque config")
	}
	if nicCfg.Opaque.Driver != "dra.net" {
		t.Errorf("opaque driver = %q, want dra.net", nicCfg.Opaque.Driver)
	}

	// Unmarshal and verify parameters
	var params NICParameters
	if err := json.Unmarshal(nicCfg.Opaque.Parameters.Raw, &params); err != nil {
		t.Fatalf("failed to unmarshal NIC parameters: %v", err)
	}
	if params.Interface.Name != "net0" {
		t.Errorf("interface name = %q, want net0", params.Interface.Name)
	}
	if params.Interface.MTU != 9000 {
		t.Errorf("interface MTU = %d, want 9000", params.Interface.MTU)
	}
	if len(params.Rules) != 1 || params.Rules[0].Table != 100 {
		t.Errorf("rules = %+v, want table 100", params.Rules)
	}
	if len(params.Routes) != 1 || params.Routes[0].Table != 100 {
		t.Errorf("routes = %+v, want table 100", params.Routes)
	}

	// Verify second NIC config has incremented table ID
	var params2 NICParameters
	if err := json.Unmarshal(spec.Devices.Config[1].Opaque.Parameters.Raw, &params2); err != nil {
		t.Fatalf("failed to unmarshal NIC parameters: %v", err)
	}
	if params2.Interface.Name != "net1" {
		t.Errorf("interface name = %q, want net1", params2.Interface.Name)
	}
	if params2.Rules[0].Table != 101 {
		t.Errorf("rule table = %d, want 101", params2.Rules[0].Table)
	}
}

func TestBuildClaimTemplateSpec_NoRDMA(t *testing.T) {
	cfg := testConfig()
	cfg.NICConfig.RDMARequired = false

	spec, err := BuildClaimTemplateSpec(1, true, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nic := spec.Devices.Requests[1].Exactly
	if len(nic.Selectors) != 0 {
		t.Errorf("expected no selectors when RDMA not required, got %d", len(nic.Selectors))
	}
}

func TestTemplateName_Deterministic(t *testing.T) {
	cfg := testConfig()

	name1 := TemplateName(4, true, cfg)
	name2 := TemplateName(4, true, cfg)
	if name1 != name2 {
		t.Errorf("template names should be deterministic: %q vs %q", name1, name2)
	}

	// Different count should produce different name
	name3 := TemplateName(2, true, cfg)
	if name1 == name3 {
		t.Error("different counts should produce different template names")
	}

	// Different NUMA mode should produce different name
	name4 := TemplateName(4, false, cfg)
	if name1 == name4 {
		t.Error("different NUMA modes should produce different template names")
	}
}
