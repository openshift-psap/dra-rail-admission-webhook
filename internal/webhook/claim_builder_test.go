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
	spec, err := BuildClaimTemplateSpec(1, true, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(spec.Devices.Requests))
	}

	if spec.Devices.Requests[0].Name != "gpu-0" {
		t.Errorf("expected first request name 'gpu-0', got %q", spec.Devices.Requests[0].Name)
	}
	if spec.Devices.Requests[1].Name != "nic-0" {
		t.Errorf("expected second request name 'nic-0', got %q", spec.Devices.Requests[1].Name)
	}

	gpu := spec.Devices.Requests[0].Exactly
	if gpu.DeviceClassName != "gpu.nvidia.com" {
		t.Errorf("GPU device class = %q, want gpu.nvidia.com", gpu.DeviceClassName)
	}
	if gpu.Count != 1 {
		t.Errorf("GPU count = %d, want 1", gpu.Count)
	}

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

	if len(spec.Devices.Config) != 1 {
		t.Fatalf("expected 1 config entry, got %d", len(spec.Devices.Config))
	}
}

func TestBuildClaimTemplateSpec_FourPairsWithNUMA(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(4, true, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Requests) != 8 {
		t.Fatalf("expected 8 requests, got %d", len(spec.Devices.Requests))
	}

	if len(spec.Devices.Constraints) != 5 {
		t.Fatalf("expected 5 constraints, got %d", len(spec.Devices.Constraints))
	}

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

	if len(spec.Devices.Config) != 4 {
		t.Fatalf("expected 4 config entries, got %d", len(spec.Devices.Config))
	}
}

func TestBuildClaimTemplateSpec_CrossNUMA(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(6, false, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Requests) != 12 {
		t.Fatalf("expected 12 requests, got %d", len(spec.Devices.Requests))
	}

	if len(spec.Devices.Constraints) != 6 {
		t.Fatalf("expected 6 constraints (PCIe only, no NUMA), got %d", len(spec.Devices.Constraints))
	}

	for _, c := range spec.Devices.Constraints {
		if string(*c.MatchAttribute) == NUMANodeAttribute {
			t.Error("should not have NUMA constraint when numaConstrained=false")
		}
	}
}

func TestBuildClaimTemplateSpec_NICConfig(t *testing.T) {
	cfg := testConfig()
	spec, err := BuildClaimTemplateSpec(2, true, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Config) != 2 {
		t.Fatalf("expected 2 config entries, got %d", len(spec.Devices.Config))
	}

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

func testRailConfig() Config {
	return Config{
		MaxPairsPerNUMA:    4,
		MaxPairsPerNode:    8,
		GPUDeviceClassName: "gpu.nvidia.com",
		NICDeviceClassName: "dranet",
		NICConfig: NICConfig{
			MTU:             9000,
			RDMARequired:    true,
			InterfacePrefix: "net",
			StartingTableID: 100,
			Rails: []RailConfig{
				{Subnet: "10.0.0.0/16", Gateway: "10.0.0.1", IPv4Prefix: "10.0."},
				{Subnet: "10.1.0.0/16", Gateway: "10.1.0.1", IPv4Prefix: "10.1."},
				{Subnet: "10.2.0.0/16", Gateway: "10.2.0.1", IPv4Prefix: "10.2."},
				{Subnet: "10.3.0.0/16", Gateway: "10.3.0.1", IPv4Prefix: "10.3."},
				{Subnet: "10.4.0.0/16", Gateway: "10.4.0.1", IPv4Prefix: "10.4."},
				{Subnet: "10.5.0.0/16", Gateway: "10.5.0.1", IPv4Prefix: "10.5."},
				{Subnet: "10.6.0.0/16", Gateway: "10.6.0.1", IPv4Prefix: "10.6."},
				{Subnet: "10.7.0.0/16", Gateway: "10.7.0.1", IPv4Prefix: "10.7."},
			},
		},
	}
}

func TestBuildClaimTemplateSpec_RailRouting(t *testing.T) {
	cfg := testRailConfig()
	// Explicitly select rails 0 and 1
	spec, err := BuildClaimTemplateSpec(2, true, cfg, []int{0, 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Config) != 2 {
		t.Fatalf("expected 2 config entries, got %d", len(spec.Devices.Config))
	}

	var params0 NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params0); err != nil {
		t.Fatalf("failed to unmarshal NIC 0 parameters: %v", err)
	}

	if len(params0.Rules) != 1 {
		t.Fatalf("rail 0: expected 1 rule, got %d", len(params0.Rules))
	}
	if params0.Rules[0].Source != "10.0.0.0/16" {
		t.Errorf("rail 0 rule source = %q, want 10.0.0.0/16", params0.Rules[0].Source)
	}
	if params0.Rules[0].Table != 100 {
		t.Errorf("rail 0 rule table = %d, want 100", params0.Rules[0].Table)
	}
	if params0.Rules[0].Priority != 32765 {
		t.Errorf("rail 0 rule priority = %d, want 32765", params0.Rules[0].Priority)
	}

	// own-subnet(1) + cross-subnet(7) + default(1) = 9
	if len(params0.Routes) != 9 {
		t.Fatalf("rail 0: expected 9 routes, got %d", len(params0.Routes))
	}

	if params0.Routes[0].Destination != "10.0.0.0/16" || params0.Routes[0].Scope != 253 || params0.Routes[0].Table != 100 {
		t.Errorf("rail 0 link route = %+v, want {10.0.0.0/16 scope:253 table:100}", params0.Routes[0])
	}

	if params0.Routes[1].Destination != "10.1.0.0/16" || params0.Routes[1].Gateway != "10.0.0.1" {
		t.Errorf("rail 0 cross-subnet route[1] = %+v, want {10.1.0.0/16 gw:10.0.0.1}", params0.Routes[1])
	}

	lastRoute := params0.Routes[len(params0.Routes)-1]
	if lastRoute.Destination != "0.0.0.0/0" || lastRoute.Gateway != "10.0.0.1" || lastRoute.Table != 100 {
		t.Errorf("rail 0 default route = %+v, want {0.0.0.0/0 gw:10.0.0.1 table:100}", lastRoute)
	}

	var params1 NICParameters
	if err := json.Unmarshal(spec.Devices.Config[1].Opaque.Parameters.Raw, &params1); err != nil {
		t.Fatalf("failed to unmarshal NIC 1 parameters: %v", err)
	}

	if params1.Rules[0].Source != "10.1.0.0/16" || params1.Rules[0].Table != 101 {
		t.Errorf("rail 1 rule = %+v, want {source:10.1.0.0/16 table:101}", params1.Rules[0])
	}
	if params1.Routes[0].Destination != "10.1.0.0/16" || params1.Routes[0].Table != 101 {
		t.Errorf("rail 1 link route = %+v, want {10.1.0.0/16 table:101}", params1.Routes[0])
	}
	if params1.Routes[1].Destination != "10.0.0.0/16" || params1.Routes[1].Gateway != "10.1.0.1" {
		t.Errorf("rail 1 cross-subnet route[1] = %+v, want {10.0.0.0/16 gw:10.1.0.1}", params1.Routes[1])
	}
}

func TestBuildClaimTemplateSpec_NonSequentialRails(t *testing.T) {
	cfg := testRailConfig()
	// Select rails 3 and 5 (non-sequential, simulating availability-based selection)
	spec, err := BuildClaimTemplateSpec(2, true, cfg, []int{3, 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// NIC 0 should be pinned to rail 3 (10.3.0.0/16)
	nic0 := spec.Devices.Requests[1].Exactly
	expected0 := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.3.")`
	if nic0.Selectors[0].CEL.Expression != expected0 {
		t.Errorf("nic-0 selector = %q, want %q", nic0.Selectors[0].CEL.Expression, expected0)
	}

	// NIC 1 should be pinned to rail 5 (10.5.0.0/16)
	nic1 := spec.Devices.Requests[3].Exactly
	expected1 := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.5.")`
	if nic1.Selectors[0].CEL.Expression != expected1 {
		t.Errorf("nic-1 selector = %q, want %q", nic1.Selectors[0].CEL.Expression, expected1)
	}

	// Verify routing uses rail-specific tables (103 and 105, not 100 and 101)
	var params0 NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params0); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if params0.Rules[0].Table != 103 {
		t.Errorf("nic-0 (rail 3) table = %d, want 103", params0.Rules[0].Table)
	}
	if params0.Rules[0].Source != "10.3.0.0/16" {
		t.Errorf("nic-0 (rail 3) source = %q, want 10.3.0.0/16", params0.Rules[0].Source)
	}
	// Interface name should still be net0 (NIC position, not rail index)
	if params0.Interface.Name != "net0" {
		t.Errorf("interface name = %q, want net0", params0.Interface.Name)
	}

	var params1 NICParameters
	if err := json.Unmarshal(spec.Devices.Config[1].Opaque.Parameters.Raw, &params1); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if params1.Rules[0].Table != 105 {
		t.Errorf("nic-1 (rail 5) table = %d, want 105", params1.Rules[0].Table)
	}
	if params1.Interface.Name != "net1" {
		t.Errorf("interface name = %q, want net1", params1.Interface.Name)
	}
}

func TestBuildClaimTemplateSpec_RailCELSelectors(t *testing.T) {
	cfg := testRailConfig()
	spec, err := BuildClaimTemplateSpec(2, true, cfg, []int{0, 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nic0 := spec.Devices.Requests[1].Exactly
	if len(nic0.Selectors) != 1 {
		t.Fatalf("nic-0: expected 1 selector, got %d", len(nic0.Selectors))
	}
	expected0 := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.0.")`
	if nic0.Selectors[0].CEL.Expression != expected0 {
		t.Errorf("nic-0 selector = %q, want %q", nic0.Selectors[0].CEL.Expression, expected0)
	}

	nic1 := spec.Devices.Requests[3].Exactly
	if len(nic1.Selectors) != 1 {
		t.Fatalf("nic-1: expected 1 selector, got %d", len(nic1.Selectors))
	}
	expected1 := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.1.")`
	if nic1.Selectors[0].CEL.Expression != expected1 {
		t.Errorf("nic-1 selector = %q, want %q", nic1.Selectors[0].CEL.Expression, expected1)
	}
}

func TestBuildClaimTemplateSpec_RailNoRDMA(t *testing.T) {
	cfg := testRailConfig()
	cfg.NICConfig.RDMARequired = false

	spec, err := BuildClaimTemplateSpec(1, true, cfg, []int{0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nic := spec.Devices.Requests[1].Exactly
	if len(nic.Selectors) != 1 {
		t.Fatalf("expected 1 selector (ipv4 only), got %d", len(nic.Selectors))
	}
	expected := `device.attributes["dra.net"].ipv4.startsWith("10.0.")`
	if nic.Selectors[0].CEL.Expression != expected {
		t.Errorf("selector = %q, want %q", nic.Selectors[0].CEL.Expression, expected)
	}
}

func TestBuildClaimTemplateSpec_NoRDMA(t *testing.T) {
	cfg := testConfig()
	cfg.NICConfig.RDMARequired = false

	spec, err := BuildClaimTemplateSpec(1, true, cfg, nil)
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

	name1 := TemplateName(4, true, cfg, nil)
	name2 := TemplateName(4, true, cfg, nil)
	if name1 != name2 {
		t.Errorf("template names should be deterministic: %q vs %q", name1, name2)
	}

	name3 := TemplateName(2, true, cfg, nil)
	if name1 == name3 {
		t.Error("different counts should produce different template names")
	}

	name4 := TemplateName(4, false, cfg, nil)
	if name1 == name4 {
		t.Error("different NUMA modes should produce different template names")
	}
}

func TestTemplateName_DifferentRails(t *testing.T) {
	cfg := testRailConfig()

	name1 := TemplateName(1, true, cfg, []int{0})
	name2 := TemplateName(1, true, cfg, []int{3})
	if name1 == name2 {
		t.Error("different rail indices should produce different template names")
	}

	// Same rails should produce same name
	name3 := TemplateName(1, true, cfg, []int{3})
	if name2 != name3 {
		t.Errorf("same rail indices should produce same name: %q vs %q", name2, name3)
	}
}
