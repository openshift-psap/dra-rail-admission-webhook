package dryrun

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

func fakeNICDevice(name, ipv4 string, numaZone int, rdma bool) resourcev1.Device {
	numa := int64(numaZone)
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"dra.net/ipv4":     {StringValue: &ipv4},
			"dra.net/rdma":     {BoolValue: &rdma},
			"dra.net/numaNode": {IntValue: &numa},
		},
	}
}

func fakeNICDeviceWithIfName(name, ifName string, numaZone int, rdma bool) resourcev1.Device {
	numa := int64(numaZone)
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"dra.net/ifName":   {StringValue: &ifName},
			"dra.net/rdma":     {BoolValue: &rdma},
			"dra.net/numaNode": {IntValue: &numa},
		},
	}
}

func fakeGPUDevice(name, pciBusID string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"resource.kubernetes.io/pciBusID": {StringValue: &pciBusID},
		},
	}
}

func autoModeState() *ClusterState {
	nodeName := "node-1"
	devices := make([]resourcev1.Device, 8)
	for i := 0; i < 8; i++ {
		numaZone := 0
		if i >= 4 {
			numaZone = 1
		}
		ipv4 := "10." + itoa(i) + ".100.1"
		devices[i] = fakeNICDevice("nic-"+itoa(i), ipv4, numaZone, true)
	}

	return &ClusterState{
		ClusterName: "test-cluster",
		ResourceSlices: []resourcev1.ResourceSlice{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node-1-nics"},
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   "dra.net",
					NodeName: &nodeName,
					Pool:     resourcev1.ResourcePool{Name: "nic-pool"},
					Devices:  devices,
				},
			},
		},
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{
				Name:   "node-1",
				Labels: map[string]string{"kubernetes.io/hostname": "node-1"},
			}},
		},
	}
}

func autoModeCfg() webhook.Config {
	return webhook.Config{
		MaxPairsPerNUMA:    4,
		MaxPairsPerNode:    8,
		GPUDeviceClassName: "gpu.nvidia.com",
		NICDeviceClassName: "dranet",
		NICConfig: webhook.NICConfig{
			MTU:             9000,
			RDMARequired:    true,
			InterfacePrefix: "net",
			StartingTableID: 100,
			Rails: []webhook.RailConfig{
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

func TestSimulate_AutoMode_SinglePair(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    autoModeCfg(),
		State:     autoModeState(),
		Count:     1,
		Namespace: "default",
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}
	if result.NodeName != "node-1" {
		t.Errorf("node = %q, want node-1", result.NodeName)
	}
	if len(result.Templates) != 1 {
		t.Errorf("templates = %d, want 1", len(result.Templates))
	}
	if len(result.PatchOps) < 4 {
		t.Errorf("patch ops = %d, want >= 4", len(result.PatchOps))
	}
}

func TestSimulate_AutoMode_FourPairs(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    autoModeCfg(),
		State:     autoModeState(),
		Count:     4,
		Namespace: "default",
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}
	if len(result.Templates) != 4 {
		t.Errorf("templates = %d, want 4", len(result.Templates))
	}
	for _, tmpl := range result.Templates {
		if !tmpl.HasPCIeConstraint {
			t.Errorf("template %s should have PCIe constraint in auto mode", tmpl.Name)
		}
	}
}

func TestSimulate_AutoMode_ExceedsNUMA(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    autoModeCfg(),
		State:     autoModeState(),
		Count:     5,
		Namespace: "default",
	})

	if result.Error == "" {
		t.Fatal("expected error for 5 pairs without cross-NUMA")
	}
}

func TestSimulate_AutoMode_CrossNUMA(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    autoModeCfg(),
		State:     autoModeState(),
		Count:     6,
		Namespace: "default",
		CrossNUMA: true,
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}
	if len(result.Templates) != 6 {
		t.Errorf("templates = %d, want 6", len(result.Templates))
	}
}

func TestSimulate_AutoMode_NoNodes(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config: autoModeCfg(),
		State: &ClusterState{
			Nodes: []corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "empty-node", Labels: map[string]string{"kubernetes.io/hostname": "empty-node"}}},
			},
		},
		Count:     1,
		Namespace: "default",
	})

	if result.Error == "" {
		t.Fatal("expected error with no NIC devices")
	}
}

func explicitModeState() *ClusterState {
	nodeName := "gpu-h100-node"

	nicDevices := []resourcev1.Device{
		fakeNICDeviceWithIfName("nic-0", "ens1f0np0", 0, true),
		fakeNICDeviceWithIfName("nic-1", "ens1f1np1", 0, true),
		fakeNICDeviceWithIfName("nic-2", "ens12f0np0", 1, true),
		fakeNICDeviceWithIfName("nic-3", "ens12f1np1", 1, true),
	}

	gpuDevices := []resourcev1.Device{
		fakeGPUDevice("gpu-0", "0008:06:00.0"),
		fakeGPUDevice("gpu-1", "0008:07:00.0"),
		fakeGPUDevice("gpu-2", "0009:06:00.0"),
		fakeGPUDevice("gpu-3", "0009:07:00.0"),
	}

	return &ClusterState{
		ClusterName: "aks-cluster",
		ResourceSlices: []resourcev1.ResourceSlice{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node-nics"},
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   "dra.net",
					NodeName: &nodeName,
					Pool:     resourcev1.ResourcePool{Name: "nic-pool"},
					Devices:  nicDevices,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "node-gpus"},
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   "gpu.nvidia.com",
					NodeName: &nodeName,
					Pool:     resourcev1.ResourcePool{Name: "gpu-pool"},
					Devices:  gpuDevices,
				},
			},
		},
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{
				Name:   "gpu-h100-node",
				Labels: map[string]string{"agentpool": "gpu-h100", "kubernetes.io/hostname": "gpu-h100-node"},
			}},
		},
	}
}

func explicitModeCfg() webhook.Config {
	return webhook.Config{
		MaxPairsPerNUMA:    4,
		MaxPairsPerNode:    8,
		GPUDeviceClassName: "gpu.nvidia.com",
		NICDeviceClassName: "dranet",
		NICConfig: webhook.NICConfig{
			MTU:             9000,
			RDMARequired:    true,
			InterfacePrefix: "net",
			StartingTableID: 100,
		},
		PairingMode: webhook.PairingModeExplicit,
		PairingConfig: &webhook.PairingConfig{
			DeviceSelectors: map[string]webhook.DeviceSelectorConfig{
				"gpu": {DeviceClassName: "gpu.nvidia.com", AttributeDomain: "resource.kubernetes.io", AttributeName: "pciBusID"},
				"nic": {DeviceClassName: "dra.net", AttributeDomain: "dra.net", AttributeName: "ifName"},
			},
			NodePoolLabelKey: "agentpool",
			NodePools: []webhook.NodePoolMapping{
				{
					NodePoolLabel: "gpu-h100",
					Pairs: []webhook.ExplicitPairMapping{
						{Devices: map[string]string{"gpu": "0008:06:00.0", "nic": "ens1f0np0"}, Rail: 0},
						{Devices: map[string]string{"gpu": "0008:07:00.0", "nic": "ens1f1np1"}, Rail: 1},
						{Devices: map[string]string{"gpu": "0009:06:00.0", "nic": "ens12f0np0"}, Rail: 2},
						{Devices: map[string]string{"gpu": "0009:07:00.0", "nic": "ens12f1np1"}, Rail: 3},
					},
				},
			},
		},
	}
}

func TestSimulate_ExplicitMode_SinglePair(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    explicitModeCfg(),
		State:     explicitModeState(),
		Count:     1,
		Namespace: "default",
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}
	if result.NodeName != "gpu-h100-node" {
		t.Errorf("node = %q, want gpu-h100-node", result.NodeName)
	}
	if len(result.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(result.Templates))
	}

	tmpl := result.Templates[0]
	if tmpl.HasPCIeConstraint {
		t.Error("explicit mode should NOT have PCIe MatchAttribute constraint")
	}
	if len(tmpl.Requests) != 2 {
		t.Errorf("requests = %d, want 2 (gpu + nic)", len(tmpl.Requests))
	}

	// Check CEL pin selectors present
	for _, req := range tmpl.Requests {
		if len(req.Selectors) == 0 {
			t.Errorf("request %q should have CEL pin selectors", req.Name)
		}
	}
}

func TestSimulate_ExplicitMode_AllFourPairs(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    explicitModeCfg(),
		State:     explicitModeState(),
		Count:     4,
		Namespace: "default",
		CrossNUMA: true,
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}
	if len(result.Templates) != 4 {
		t.Errorf("templates = %d, want 4", len(result.Templates))
	}

	// All should use CEL pinning, no PCIe constraint
	for _, tmpl := range result.Templates {
		if tmpl.HasPCIeConstraint {
			t.Errorf("template %s should not have PCIe constraint", tmpl.Name)
		}
	}
}

func TestSimulate_ExplicitMode_ExceedsPoolSize(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    explicitModeCfg(),
		State:     explicitModeState(),
		Count:     5,
		Namespace: "default",
		CrossNUMA: true,
	})

	if result.Error == "" {
		t.Fatal("expected error: pool only has 4 pairs")
	}
}

func TestSimulate_ExplicitMode_CELSelectorsCorrect(t *testing.T) {
	result := Simulate(context.Background(), SimulateRequest{
		Config:    explicitModeCfg(),
		State:     explicitModeState(),
		Count:     2,
		Namespace: "default",
	})

	if result.Error != "" {
		t.Fatalf("simulation failed: %s", result.Error)
	}

	// Check that GPU selectors contain pciBusID pin
	for _, tmpl := range result.Templates {
		for _, req := range tmpl.Requests {
			if req.Name == "gpu" {
				found := false
				for _, sel := range req.Selectors {
					if strings.Contains(sel, "pciBusID") {
						found = true
					}
				}
				if !found {
					t.Errorf("GPU request should have pciBusID CEL selector, got %v", req.Selectors)
				}
			}
			if req.Name == "nic" {
				found := false
				for _, sel := range req.Selectors {
					if strings.Contains(sel, "ifName") {
						found = true
					}
				}
				if !found {
					t.Errorf("NIC request should have ifName CEL selector, got %v", req.Selectors)
				}
			}
		}
	}
}

func TestPrintResult_AutoMode(t *testing.T) {
	cfg := autoModeCfg()
	result := Simulate(context.Background(), SimulateRequest{
		Config:    cfg,
		State:     autoModeState(),
		Count:     2,
		Namespace: "default",
	})

	var buf bytes.Buffer
	PrintResult(&buf, result, cfg)
	output := buf.String()

	if !strings.Contains(output, "auto (MatchAttribute)") {
		t.Error("output should mention auto mode")
	}
	if !strings.Contains(output, "PASSED") {
		t.Error("output should contain PASSED")
	}
	if !strings.Contains(output, "node-1") {
		t.Error("output should mention allocated node")
	}
}

func TestPrintResult_ExplicitMode(t *testing.T) {
	cfg := explicitModeCfg()
	result := Simulate(context.Background(), SimulateRequest{
		Config:    cfg,
		State:     explicitModeState(),
		Count:     1,
		Namespace: "default",
	})

	var buf bytes.Buffer
	PrintResult(&buf, result, cfg)
	output := buf.String()

	if !strings.Contains(output, "explicit") {
		t.Error("output should mention explicit mode")
	}
	if !strings.Contains(output, "CEL pinning") {
		t.Error("output should mention CEL pinning")
	}
	if !strings.Contains(output, "PASSED") {
		t.Error("output should contain PASSED")
	}
}

func TestPrintResult_Error(t *testing.T) {
	cfg := autoModeCfg()
	result := Simulate(context.Background(), SimulateRequest{
		Config:    cfg,
		State:     autoModeState(),
		Count:     5, // exceeds NUMA without cross-NUMA
		Namespace: "default",
	})

	var buf bytes.Buffer
	PrintResult(&buf, result, cfg)
	output := buf.String()

	if !strings.Contains(output, "FAILED") {
		t.Error("output should contain FAILED")
	}
}

func itoa(i int) string {
	return string(rune('0'+i)) + ""
}
