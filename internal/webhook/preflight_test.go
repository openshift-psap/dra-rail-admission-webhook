package webhook

import (
	"context"
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func strP(s string) *string { return &s }
func boolP(b bool) *bool    { return &b }
func intP(i int64) *int64   { return &i }

// makeNICDevice creates a NIC device for testing.
// If available is true, operational attributes (ifName, ipv4, state, type) are present.
// If available is false, only identification attributes remain (simulating allocation).
func makeNICDevice(name string, numaZone int, pcieRoot string, available bool) resourcev1.Device {
	attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		"dra.net/numaNode":                 {IntValue: intP(int64(numaZone))},
		"dra.net/pciAddress":               {StringValue: strP("0000:a3:00.0")},
		"dra.net/pciDevice":                {StringValue: strP("ConnectX Family mlx5Gen Virtual Function")},
		"dra.net/pciVendor":                {StringValue: strP("Mellanox Technologies")},
		"dra.net/rdma":                     {BoolValue: boolP(true)},
		"resource.kubernetes.io/pcieRoot":  {StringValue: strP(pcieRoot)},
	}

	if available {
		attrs["dra.net/ifName"] = resourcev1.DeviceAttribute{StringValue: strP("enp163s0")}
		attrs["dra.net/ipv4"] = resourcev1.DeviceAttribute{StringValue: strP("10.0.0.5/16")}
		attrs["dra.net/state"] = resourcev1.DeviceAttribute{StringValue: strP("up")}
		attrs["dra.net/type"] = resourcev1.DeviceAttribute{StringValue: strP("device")}
		attrs["dra.net/mac"] = resourcev1.DeviceAttribute{StringValue: strP("02:00:04:bc:50:f4")}
		attrs["dra.net/mtu"] = resourcev1.DeviceAttribute{IntValue: intP(9000)}
	}

	return resourcev1.Device{
		Name:       name,
		Attributes: attrs,
	}
}

func makeGPUDevice(name string, pcieRoot string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type":                            {StringValue: strP("gpu")},
			"productName":                     {StringValue: strP("NVIDIA H100 80GB HBM3")},
			"resource.kubernetes.io/pcieRoot": {StringValue: strP(pcieRoot)},
		},
	}
}

// makeNodeSlices creates a pair of ResourceSlices (NIC + GPU) for a node with
// the given topology: 2 NUMA zones, specified available/allocated NICs.
func makeNodeSlices(nodeName string, numa0Available, numa0Allocated, numa1Available, numa1Allocated int) []*resourcev1.ResourceSlice {
	pcieRootsNUMA0 := []string{"pci0000:a0", "pci0000:a4", "pci0000:a8", "pci0000:ac"}
	pcieRootsNUMA1 := []string{"pci0000:d0", "pci0000:d4", "pci0000:d8", "pci0000:dc"}

	var nicDevices []resourcev1.Device
	var gpuDevices []resourcev1.Device
	idx := 0

	// NUMA 0 available NICs
	for i := 0; i < numa0Available && i < len(pcieRootsNUMA0); i++ {
		nicDevices = append(nicDevices, makeNICDevice(
			"nic-"+string(rune('a'+idx)), 0, pcieRootsNUMA0[i], true))
		gpuDevices = append(gpuDevices, makeGPUDevice(
			"gpu-"+string(rune('a'+idx)), pcieRootsNUMA0[i]))
		idx++
	}
	// NUMA 0 allocated NICs
	for i := 0; i < numa0Allocated && i+numa0Available < len(pcieRootsNUMA0); i++ {
		nicDevices = append(nicDevices, makeNICDevice(
			"nic-"+string(rune('a'+idx)), 0, pcieRootsNUMA0[i+numa0Available], false))
		gpuDevices = append(gpuDevices, makeGPUDevice(
			"gpu-"+string(rune('a'+idx)), pcieRootsNUMA0[i+numa0Available]))
		idx++
	}
	// NUMA 1 available NICs
	for i := 0; i < numa1Available && i < len(pcieRootsNUMA1); i++ {
		nicDevices = append(nicDevices, makeNICDevice(
			"nic-"+string(rune('a'+idx)), 1, pcieRootsNUMA1[i], true))
		gpuDevices = append(gpuDevices, makeGPUDevice(
			"gpu-"+string(rune('a'+idx)), pcieRootsNUMA1[i]))
		idx++
	}
	// NUMA 1 allocated NICs
	for i := 0; i < numa1Allocated && i+numa1Available < len(pcieRootsNUMA1); i++ {
		nicDevices = append(nicDevices, makeNICDevice(
			"nic-"+string(rune('a'+idx)), 1, pcieRootsNUMA1[i+numa1Available], false))
		gpuDevices = append(gpuDevices, makeGPUDevice(
			"gpu-"+string(rune('a'+idx)), pcieRootsNUMA1[i+numa1Available]))
		idx++
	}

	return []*resourcev1.ResourceSlice{
		{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName + "-dra.net"},
			Spec: resourcev1.ResourceSliceSpec{
				Driver:   "dra.net",
				NodeName: strP(nodeName),
				Pool:     resourcev1.ResourcePool{Name: nodeName},
				Devices:  nicDevices,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName + "-gpu.nvidia.com"},
			Spec: resourcev1.ResourceSliceSpec{
				Driver:   "gpu.nvidia.com",
				NodeName: strP(nodeName),
				Pool:     resourcev1.ResourcePool{Name: nodeName},
				Devices:  gpuDevices,
			},
		},
	}
}

func TestPreflightCheck_Sufficient_NUMA(t *testing.T) {
	// Node with 4 available on NUMA 0, 4 available on NUMA 1
	slices := makeNodeSlices("worker-1", 4, 0, 4, 0)
	client := fake.NewSimpleClientset(slices[0], slices[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	// Request 4 with NUMA constraint — should pass (4 available on NUMA 0)
	err := checker.CheckAvailability(context.Background(), 4, true, cfg)
	if err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

func TestPreflightCheck_Insufficient_NUMA(t *testing.T) {
	// Node with 2 available on NUMA 0, 2 available on NUMA 1
	slices := makeNodeSlices("worker-1", 2, 2, 2, 2)
	client := fake.NewSimpleClientset(slices[0], slices[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	// Request 3 with NUMA constraint — should fail (max 2 per NUMA)
	err := checker.CheckAvailability(context.Background(), 3, true, cfg)
	if err == nil {
		t.Fatal("expected error for insufficient NUMA availability")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("error should mention preflight: %v", err)
	}
	if !strings.Contains(err.Error(), "NUMA") {
		t.Errorf("error should mention NUMA availability: %v", err)
	}
}

func TestPreflightCheck_CrossNUMA_Sufficient(t *testing.T) {
	// Node with 2 available on NUMA 0, 2 available on NUMA 1
	slices := makeNodeSlices("worker-1", 2, 2, 2, 2)
	client := fake.NewSimpleClientset(slices[0], slices[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	// Request 4 cross-NUMA — should pass (2+2 = 4 total)
	err := checker.CheckAvailability(context.Background(), 4, false, cfg)
	if err != nil {
		t.Fatalf("expected pass for cross-NUMA, got: %v", err)
	}
}

func TestPreflightCheck_CrossNUMA_Insufficient(t *testing.T) {
	// Node with 2 available on NUMA 0, 1 available on NUMA 1
	slices := makeNodeSlices("worker-1", 2, 2, 1, 3)
	client := fake.NewSimpleClientset(slices[0], slices[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	// Request 4 cross-NUMA — should fail (2+1 = 3 total)
	err := checker.CheckAvailability(context.Background(), 4, false, cfg)
	if err == nil {
		t.Fatal("expected error for insufficient cross-NUMA availability")
	}
}

func TestPreflightCheck_MultipleNodes(t *testing.T) {
	// Node 1: 1 available on NUMA 0, 0 on NUMA 1
	// Node 2: 3 available on NUMA 0, 4 on NUMA 1
	slices1 := makeNodeSlices("worker-1", 1, 3, 0, 4)
	slices2 := makeNodeSlices("worker-2", 3, 1, 4, 0)
	client := fake.NewSimpleClientset(slices1[0], slices1[1], slices2[0], slices2[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	// Request 3 with NUMA — node 1 can't, node 2 can (NUMA 1 has 4)
	err := checker.CheckAvailability(context.Background(), 3, true, cfg)
	if err != nil {
		t.Fatalf("expected pass (worker-2 NUMA 1 has 4), got: %v", err)
	}
}

func TestPreflightCheck_NoNodes(t *testing.T) {
	client := fake.NewSimpleClientset()

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	err := checker.CheckAvailability(context.Background(), 1, true, cfg)
	if err == nil {
		t.Fatal("expected error when no nodes have resources")
	}
	if !strings.Contains(err.Error(), "no nodes found") {
		t.Errorf("error should mention no nodes: %v", err)
	}
}

func TestPreflightCheck_AllAllocated(t *testing.T) {
	// Node with all 8 NICs allocated (0 available)
	slices := makeNodeSlices("worker-1", 0, 4, 0, 4)
	client := fake.NewSimpleClientset(slices[0], slices[1])

	checker := &PreflightChecker{ResourceClient: client.ResourceV1()}
	cfg := testConfig()

	err := checker.CheckAvailability(context.Background(), 1, true, cfg)
	if err == nil {
		t.Fatal("expected error when all devices are allocated")
	}
}

func TestPreflightCheck_DisabledByDefault(t *testing.T) {
	cfg := testConfig()
	// PreflightCheck defaults to false
	if cfg.PreflightCheck {
		t.Error("PreflightCheck should be false by default")
	}
}

func TestIsNICAvailable(t *testing.T) {
	cfg := testConfig()

	available := makeNICDevice("nic-0", 0, "pci0000:a0", true)
	if !isNICAvailable(available, cfg) {
		t.Error("expected available NIC to be detected as available")
	}

	allocated := makeNICDevice("nic-1", 0, "pci0000:a0", false)
	if isNICAvailable(allocated, cfg) {
		t.Error("expected allocated NIC to be detected as unavailable")
	}
}

func TestIsNICAvailable_NoRDMARequired(t *testing.T) {
	cfg := testConfig()
	cfg.NICConfig.RDMARequired = false

	// NIC without RDMA should be available when RDMA not required
	device := resourcev1.Device{
		Name: "nic-no-rdma",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"dra.net/ifName":   {StringValue: strP("eth0")},
			"dra.net/numaNode": {IntValue: intP(0)},
			"dra.net/rdma":     {BoolValue: boolP(false)},
		},
	}
	if !isNICAvailable(device, cfg) {
		t.Error("NIC without RDMA should be available when RDMARequired=false")
	}
}

func TestMutate_WithAvailableSlots(t *testing.T) {
	cfg := testConfigWithRails()

	// Use allocator-aware mutator with available NICs
	m := newTestMutatorWithAllocator(cfg)

	pod := podWithGPUNICPairs(2)
	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error with available slots: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}
}

func TestMutate_DeniesWhenNoSlots(t *testing.T) {
	cfg := testConfigWithRails()

	// Node exists but no ResourceSlices (no NICs available)
	node := fakeNode("worker-1", map[string]string{
		"kubernetes.io/hostname": "worker-1",
	})
	client := fake.NewSimpleClientset(node)
	allocator := NewAllocator(client.ResourceV1(), client, cfg)

	m := &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
		Allocator:      allocator,
	}

	pod := podWithGPUNICPairs(1)
	_, err := m.Mutate(context.Background(), pod, "default")
	if err == nil {
		t.Fatal("expected denial when all devices allocated")
	}
	if !strings.Contains(err.Error(), "allocation failed") {
		t.Errorf("error should be from allocation: %v", err)
	}
}
