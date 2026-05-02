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

func testExplicitConfig() Config {
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
			},
		},
		PairingMode: PairingModeExplicit,
		PairingConfig: &PairingConfig{
			DeviceSelectors: map[string]DeviceSelectorConfig{
				"gpu": {DeviceClassName: "gpu.nvidia.com", AttributeDomain: "resource.kubernetes.io", AttributeName: "pciBusID"},
				"nic": {DeviceClassName: "dranet", AttributeDomain: "dra.net", AttributeName: "rdmaDevice"},
			},
			NodePoolLabelKey: "agentpool",
			NodePools: []NodePoolMapping{
				{
					NodePoolLabel: "gpu-h100",
					Pairs: []ExplicitPairMapping{
						{Devices: map[string]string{"gpu": "0008:06:00.0", "nic": "mlx5_0"}, Rail: 0},
						{Devices: map[string]string{"gpu": "0008:07:00.0", "nic": "mlx5_1"}, Rail: 1},
					},
				},
			},
		},
	}
}

func TestBuildExplicitPairClaimSpec_GPUAndNICSelectors(t *testing.T) {
	cfg := testExplicitConfig()
	pair := cfg.PairingConfig.NodePools[0].Pairs[0]

	spec, err := BuildExplicitPairClaimSpec(0, 0, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have 2 requests: gpu and nic (sorted alphabetically)
	if len(spec.Devices.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(spec.Devices.Requests))
	}

	gpuReq := spec.Devices.Requests[0]
	if gpuReq.Name != "gpu" {
		t.Errorf("first request name = %q, want gpu", gpuReq.Name)
	}
	if gpuReq.Exactly.DeviceClassName != "gpu.nvidia.com" {
		t.Errorf("GPU device class = %q, want gpu.nvidia.com", gpuReq.Exactly.DeviceClassName)
	}
	if len(gpuReq.Exactly.Selectors) != 1 {
		t.Fatalf("GPU: expected 1 selector, got %d", len(gpuReq.Exactly.Selectors))
	}
	expectedGPUCEL := `"resource.kubernetes.io" in device.attributes && "pciBusID" in device.attributes["resource.kubernetes.io"] && device.attributes["resource.kubernetes.io"]["pciBusID"] == "0008:06:00.0"`
	if gpuReq.Exactly.Selectors[0].CEL.Expression != expectedGPUCEL {
		t.Errorf("GPU CEL = %q, want %q", gpuReq.Exactly.Selectors[0].CEL.Expression, expectedGPUCEL)
	}

	nicReq := spec.Devices.Requests[1]
	if nicReq.Name != "nic" {
		t.Errorf("second request name = %q, want nic", nicReq.Name)
	}
	if nicReq.Exactly.DeviceClassName != "dranet" {
		t.Errorf("NIC device class = %q, want dranet", nicReq.Exactly.DeviceClassName)
	}
	// NIC should have pin selector + rail/RDMA selector = 2 selectors
	if len(nicReq.Exactly.Selectors) != 2 {
		t.Fatalf("NIC: expected 2 selectors (pin + rail/RDMA), got %d", len(nicReq.Exactly.Selectors))
	}
	expectedNICPin := `"dra.net" in device.attributes && "rdmaDevice" in device.attributes["dra.net"] && device.attributes["dra.net"]["rdmaDevice"] == "mlx5_0"`
	if nicReq.Exactly.Selectors[0].CEL.Expression != expectedNICPin {
		t.Errorf("NIC pin CEL = %q, want %q", nicReq.Exactly.Selectors[0].CEL.Expression, expectedNICPin)
	}
}

func TestBuildExplicitPairClaimSpec_NoMatchAttributeConstraint(t *testing.T) {
	cfg := testExplicitConfig()
	pair := cfg.PairingConfig.NodePools[0].Pairs[0]

	spec, err := BuildExplicitPairClaimSpec(0, 0, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Constraints) != 0 {
		t.Errorf("explicit mode should have 0 constraints, got %d", len(spec.Devices.Constraints))
	}
}

func TestBuildExplicitPairClaimSpec_NICParameters(t *testing.T) {
	cfg := testExplicitConfig()
	pair := cfg.PairingConfig.NodePools[0].Pairs[1]

	spec, err := BuildExplicitPairClaimSpec(1, 1, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spec.Devices.Config) != 1 {
		t.Fatalf("expected 1 config entry, got %d", len(spec.Devices.Config))
	}

	var params NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params); err != nil {
		t.Fatalf("failed to unmarshal NIC parameters: %v", err)
	}
	if params.Interface.Name != "net1" {
		t.Errorf("interface name = %q, want net1", params.Interface.Name)
	}
	if params.Interface.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", params.Interface.MTU)
	}
	if params.Rules[0].Table != 101 {
		t.Errorf("rule table = %d, want 101", params.Rules[0].Table)
	}
}

func TestExplicitPairTemplateName_Deterministic(t *testing.T) {
	cfg := testExplicitConfig()
	pair := cfg.PairingConfig.NodePools[0].Pairs[0]

	name1 := ExplicitPairTemplateName(0, 0, pair, cfg)
	name2 := ExplicitPairTemplateName(0, 0, pair, cfg)
	if name1 != name2 {
		t.Errorf("names should be deterministic: %q vs %q", name1, name2)
	}
}

func TestExplicitPairTemplateName_DifferentPairs(t *testing.T) {
	cfg := testExplicitConfig()
	pair0 := cfg.PairingConfig.NodePools[0].Pairs[0]
	pair1 := cfg.PairingConfig.NodePools[0].Pairs[1]

	name0 := ExplicitPairTemplateName(0, 0, pair0, cfg)
	name1 := ExplicitPairTemplateName(1, 1, pair1, cfg)
	if name0 == name1 {
		t.Error("different pairs should produce different template names")
	}
}

func TestBuildExplicitPairClaimSpec_ThreeDeviceRoles(t *testing.T) {
	cfg := testExplicitConfig()
	cfg.PairingConfig.DeviceSelectors["fpga"] = DeviceSelectorConfig{
		DeviceClassName: "fpga.vendor.com",
		AttributeDomain: "fpga.vendor.com",
		AttributeName:   "serialNumber",
	}
	pair := ExplicitPairMapping{
		Devices: map[string]string{
			"gpu":  "0008:06:00.0",
			"nic":  "mlx5_0",
			"fpga": "FPGA-001",
		},
		Rail: 0,
	}

	spec, err := BuildExplicitPairClaimSpec(0, 0, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 device roles → 3 requests (sorted: fpga, gpu, nic)
	if len(spec.Devices.Requests) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(spec.Devices.Requests))
	}
	if spec.Devices.Requests[0].Name != "fpga" {
		t.Errorf("first request = %q, want fpga", spec.Devices.Requests[0].Name)
	}
	expectedFPGA := `"fpga.vendor.com" in device.attributes && "serialNumber" in device.attributes["fpga.vendor.com"] && device.attributes["fpga.vendor.com"]["serialNumber"] == "FPGA-001"`
	if spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression != expectedFPGA {
		t.Errorf("FPGA CEL = %q, want %q", spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression, expectedFPGA)
	}
	// FPGA should have only 1 selector (pin only, no NIC-specific selectors)
	if len(spec.Devices.Requests[0].Exactly.Selectors) != 1 {
		t.Errorf("FPGA: expected 1 selector, got %d", len(spec.Devices.Requests[0].Exactly.Selectors))
	}
}

// ibmCloudH100Config returns a Config mirroring real IBM Cloud H100 topology.
// 8 GPU-NIC pairs across 2 NUMA zones (4 per NUMA), 8 rails.
// GPUs matched by resource.kubernetes.io/pciBusID.
// NICs matched by dra.net/ifName (NIC device names).
func ibmCloudH100Config() Config {
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
		PairingMode: PairingModeExplicit,
		PairingConfig: &PairingConfig{
			DeviceSelectors: map[string]DeviceSelectorConfig{
				"gpu": {DeviceClassName: "gpu.nvidia.com", AttributeDomain: "resource.kubernetes.io", AttributeName: "pciBusID"},
				"nic": {DeviceClassName: "dranet", AttributeDomain: "dra.net", AttributeName: "ifName"},
			},
			NodePoolLabelKey: "node.kubernetes.io/instance-type",
			NodePools: []NodePoolMapping{
				{
					NodePoolLabel: "h100-80gb",
					Pairs: []ExplicitPairMapping{
						// NUMA 0: gpu-7 ↔ pci-0000-a3-00-0
						{Devices: map[string]string{"gpu": "0000:a4:00.0", "nic": "enp163s0"}, Rail: 0},
						// NUMA 0: gpu-6 ↔ pci-0000-ad-00-0
						{Devices: map[string]string{"gpu": "0000:ae:00.0", "nic": "enp173s0"}, Rail: 1},
						// NUMA 0: gpu-5 ↔ pci-0000-b7-00-0
						{Devices: map[string]string{"gpu": "0000:b8:00.0", "nic": "enp183s0"}, Rail: 2},
						// NUMA 0: gpu-4 ↔ pci-0000-c1-00-0
						{Devices: map[string]string{"gpu": "0000:c2:00.0", "nic": "enp193s0"}, Rail: 3},
						// NUMA 1: gpu-3 ↔ pci-0000-cb-00-0
						{Devices: map[string]string{"gpu": "0000:cc:00.0", "nic": "enp203s0"}, Rail: 4},
						// NUMA 1: gpu-2 ↔ pci-0000-d5-00-0
						{Devices: map[string]string{"gpu": "0000:d6:00.0", "nic": "enp213s0"}, Rail: 5},
						// NUMA 1: gpu-1 ↔ pci-0000-df-00-0
						{Devices: map[string]string{"gpu": "0000:e0:00.0", "nic": "enp223s0"}, Rail: 6},
						// NUMA 1: gpu-0 ↔ pci-0000-e9-00-0
						{Devices: map[string]string{"gpu": "0000:ea:00.0", "nic": "enp233s0"}, Rail: 7},
					},
				},
			},
		},
	}
}

func TestBuildExplicitPairClaimSpec_IBMCloudH100_SinglePair(t *testing.T) {
	cfg := ibmCloudH100Config()
	pair := cfg.PairingConfig.NodePools[0].Pairs[0] // gpu-7 ↔ enp163s0, rail 0

	spec, err := BuildExplicitPairClaimSpec(0, 0, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 requests: gpu and nic (sorted)
	if len(spec.Devices.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(spec.Devices.Requests))
	}

	// GPU request: pinned by pciBusID
	gpu := spec.Devices.Requests[0]
	if gpu.Name != "gpu" {
		t.Errorf("first request = %q, want gpu", gpu.Name)
	}
	if gpu.Exactly.DeviceClassName != "gpu.nvidia.com" {
		t.Errorf("GPU class = %q, want gpu.nvidia.com", gpu.Exactly.DeviceClassName)
	}
	expectedGPU := `"resource.kubernetes.io" in device.attributes && "pciBusID" in device.attributes["resource.kubernetes.io"] && device.attributes["resource.kubernetes.io"]["pciBusID"] == "0000:a4:00.0"`
	if gpu.Exactly.Selectors[0].CEL.Expression != expectedGPU {
		t.Errorf("GPU CEL = %q\nwant    %q", gpu.Exactly.Selectors[0].CEL.Expression, expectedGPU)
	}
	if len(gpu.Exactly.Selectors) != 1 {
		t.Errorf("GPU should have 1 selector (pin only), got %d", len(gpu.Exactly.Selectors))
	}

	// NIC request: pinned by ifName + RDMA/rail selector
	nic := spec.Devices.Requests[1]
	if nic.Name != "nic" {
		t.Errorf("second request = %q, want nic", nic.Name)
	}
	if nic.Exactly.DeviceClassName != "dranet" {
		t.Errorf("NIC class = %q, want dranet", nic.Exactly.DeviceClassName)
	}
	expectedNICPin := `"dra.net" in device.attributes && "ifName" in device.attributes["dra.net"] && device.attributes["dra.net"]["ifName"] == "enp163s0"`
	if nic.Exactly.Selectors[0].CEL.Expression != expectedNICPin {
		t.Errorf("NIC pin CEL = %q\nwant         %q", nic.Exactly.Selectors[0].CEL.Expression, expectedNICPin)
	}
	expectedNICRail := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.0.")`
	if nic.Exactly.Selectors[1].CEL.Expression != expectedNICRail {
		t.Errorf("NIC rail CEL = %q\nwant          %q", nic.Exactly.Selectors[1].CEL.Expression, expectedNICRail)
	}

	// No MatchAttribute constraints
	if len(spec.Devices.Constraints) != 0 {
		t.Errorf("explicit mode: expected 0 constraints, got %d", len(spec.Devices.Constraints))
	}

	// NIC parameters: rail 0, interface net0
	if len(spec.Devices.Config) != 1 {
		t.Fatalf("expected 1 config entry, got %d", len(spec.Devices.Config))
	}
	var params NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params); err != nil {
		t.Fatalf("unmarshal NIC params: %v", err)
	}
	if params.Interface.Name != "net0" {
		t.Errorf("interface = %q, want net0", params.Interface.Name)
	}
	if params.Interface.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", params.Interface.MTU)
	}
	if params.Rules[0].Source != "10.0.0.0/16" || params.Rules[0].Table != 100 {
		t.Errorf("rule = %+v, want source=10.0.0.0/16 table=100", params.Rules[0])
	}
}

func TestBuildExplicitPairClaimSpec_IBMCloudH100_NUMA1Pair(t *testing.T) {
	cfg := ibmCloudH100Config()
	pair := cfg.PairingConfig.NodePools[0].Pairs[7] // gpu-0 ↔ enp233s0, rail 7

	spec, err := BuildExplicitPairClaimSpec(3, 7, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// GPU pinned to 0000:ea:00.0
	expectedGPU := `"resource.kubernetes.io" in device.attributes && "pciBusID" in device.attributes["resource.kubernetes.io"] && device.attributes["resource.kubernetes.io"]["pciBusID"] == "0000:ea:00.0"`
	if spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression != expectedGPU {
		t.Errorf("GPU CEL = %q\nwant    %q",
			spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression, expectedGPU)
	}

	// NIC pinned to enp233s0
	expectedNICPin := `"dra.net" in device.attributes && "ifName" in device.attributes["dra.net"] && device.attributes["dra.net"]["ifName"] == "enp233s0"`
	if spec.Devices.Requests[1].Exactly.Selectors[0].CEL.Expression != expectedNICPin {
		t.Errorf("NIC pin CEL = %q\nwant         %q",
			spec.Devices.Requests[1].Exactly.Selectors[0].CEL.Expression, expectedNICPin)
	}

	// Rail 7 routing
	expectedNICRail := `device.attributes["dra.net"].rdma == true && device.attributes["dra.net"].ipv4.startsWith("10.7.")`
	if spec.Devices.Requests[1].Exactly.Selectors[1].CEL.Expression != expectedNICRail {
		t.Errorf("NIC rail CEL = %q\nwant          %q",
			spec.Devices.Requests[1].Exactly.Selectors[1].CEL.Expression, expectedNICRail)
	}

	// NIC parameters: nicIndex=3 → net3, rail 7 → table 107
	var params NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if params.Interface.Name != "net3" {
		t.Errorf("interface = %q, want net3", params.Interface.Name)
	}
	if params.Rules[0].Table != 107 {
		t.Errorf("table = %d, want 107", params.Rules[0].Table)
	}
	if params.Rules[0].Source != "10.7.0.0/16" {
		t.Errorf("rule source = %q, want 10.7.0.0/16", params.Rules[0].Source)
	}
}

func TestBuildExplicitPairClaimSpec_IBMCloudH100_AllPairs(t *testing.T) {
	cfg := ibmCloudH100Config()
	pool := cfg.PairingConfig.NodePools[0]

	// Build all 8 pairs and verify uniqueness
	templateNames := make(map[string]bool)
	for i, pair := range pool.Pairs {
		spec, err := BuildExplicitPairClaimSpec(i, pair.Rail, pair, cfg)
		if err != nil {
			t.Fatalf("pair %d: unexpected error: %v", i, err)
		}

		// Each pair: 2 requests, 0 constraints, 1 config
		if len(spec.Devices.Requests) != 2 {
			t.Errorf("pair %d: expected 2 requests, got %d", i, len(spec.Devices.Requests))
		}
		if len(spec.Devices.Constraints) != 0 {
			t.Errorf("pair %d: expected 0 constraints, got %d", i, len(spec.Devices.Constraints))
		}
		if len(spec.Devices.Config) != 1 {
			t.Errorf("pair %d: expected 1 config, got %d", i, len(spec.Devices.Config))
		}

		// Template name uniqueness
		name := ExplicitPairTemplateName(i, pair.Rail, pair, cfg)
		if templateNames[name] {
			t.Errorf("pair %d: duplicate template name %q", i, name)
		}
		templateNames[name] = true
	}

	if len(templateNames) != 8 {
		t.Errorf("expected 8 unique template names, got %d", len(templateNames))
	}
}

func TestBuildExplicitPairClaimSpec_IBMCloudH100_CrossRailRouting(t *testing.T) {
	cfg := ibmCloudH100Config()
	pair := cfg.PairingConfig.NodePools[0].Pairs[2] // rail 2: 10.2.0.0/16

	spec, err := BuildExplicitPairClaimSpec(0, 2, pair, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var params NICParameters
	if err := json.Unmarshal(spec.Devices.Config[0].Opaque.Parameters.Raw, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Rule: source=10.2.0.0/16, table=102
	if params.Rules[0].Source != "10.2.0.0/16" || params.Rules[0].Table != 102 {
		t.Errorf("rule = %+v, want source=10.2.0.0/16 table=102", params.Rules[0])
	}

	// Routes: own-subnet(1) + cross-subnet(7) + default(1) = 9
	if len(params.Routes) != 9 {
		t.Fatalf("expected 9 routes, got %d", len(params.Routes))
	}

	// Own subnet link-scope route
	if params.Routes[0].Destination != "10.2.0.0/16" || params.Routes[0].Scope != 253 {
		t.Errorf("link route = %+v, want 10.2.0.0/16 scope=253", params.Routes[0])
	}

	// Cross-subnet routes: should include 10.0, 10.1, 10.3, 10.4, 10.5, 10.6, 10.7
	crossSubnets := make(map[string]bool)
	for _, r := range params.Routes[1 : len(params.Routes)-1] {
		crossSubnets[r.Destination] = true
		if r.Gateway != "10.2.0.1" {
			t.Errorf("cross-subnet route %q gateway = %q, want 10.2.0.1", r.Destination, r.Gateway)
		}
	}
	for _, sub := range []string{"10.0.0.0/16", "10.1.0.0/16", "10.3.0.0/16", "10.4.0.0/16", "10.5.0.0/16", "10.6.0.0/16", "10.7.0.0/16"} {
		if !crossSubnets[sub] {
			t.Errorf("missing cross-subnet route for %s", sub)
		}
	}

	// Default route in policy table
	last := params.Routes[len(params.Routes)-1]
	if last.Destination != "0.0.0.0/0" || last.Table != 102 {
		t.Errorf("default route = %+v, want 0.0.0.0/0 table=102", last)
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

func TestBuildClaimTemplateSpec_PCIeRootOnly(t *testing.T) {
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
		if string(*c.MatchAttribute) != PCIeRootAttribute {
			t.Errorf("expected only PCIe root constraints, got %q", *c.MatchAttribute)
		}
	}

	for _, c := range spec.Devices.Constraints {
		if string(*c.MatchAttribute) == NUMANodeAttribute {
			t.Error("pcieRootOnly mode should not have NUMA constraints")
		}
	}
}
