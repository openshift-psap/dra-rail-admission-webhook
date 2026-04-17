package webhook

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// testConfigWithRails returns a Config with 8 rails matching the typical
// ibmcluster topology (4 per NUMA, 8 per node).
func testConfigWithRails() Config {
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

// fakeNICDevice creates a ResourceSlice device representing a free RDMA NIC.
func fakeNICDevice(name string, ipv4 string, numaZone int) resourcev1.Device {
	rdma := true
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

// fakeResourceSlice creates a ResourceSlice with NIC devices on a node.
func fakeResourceSlice(name, nodeName string, devices []resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "dra.net",
			NodeName: &nodeName,
			Pool: resourcev1.ResourcePool{
				Name: "nic-pool",
			},
			Devices: devices,
		},
	}
}

// fakeNode creates a minimal Node object.
func fakeNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

// newTestMutatorWithAllocator creates a Mutator backed by fake clients
// pre-populated with ResourceSlices and Nodes so the Allocator can scan
// and allocate slots.
//
// Default setup: 1 node ("node-1") with 8 free NICs (rails 0-7),
// NUMA zone 0 for rails 0-3 and NUMA zone 1 for rails 4-7.
func newTestMutatorWithAllocator(cfg Config) *Mutator {
	devices := make([]resourcev1.Device, 8)
	for i := 0; i < 8; i++ {
		numaZone := 0
		if i >= 4 {
			numaZone = 1
		}
		ipv4 := "10." + intToStr(i) + ".100.1"
		devices[i] = fakeNICDevice("nic-"+intToStr(i), ipv4, numaZone)
	}

	slice := fakeResourceSlice("node-1-nics", "node-1", devices)
	node := fakeNode("node-1", map[string]string{
		"kubernetes.io/hostname": "node-1",
	})

	client := fake.NewSimpleClientset(slice, node)
	allocator := NewAllocator(client.ResourceV1(), client, cfg)

	return &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
		Allocator:      allocator,
	}
}

func newTestMutator(cfg Config) *Mutator {
	return newTestMutatorWithAllocator(cfg)
}

func podWithGPUNICPairs(count int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "workload",
					Image: "nvidia/cuda:12.3.0-base-ubuntu22.04",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceName(ResourceGPUNICPair): resource.MustParse(intToStr(count)),
						},
					},
				},
			},
		},
	}
}

func TestMutate_BasicMutation(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(2)
	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("invalid patch JSON: %v", err)
	}

	// Should have: remove resource request, add resourceClaims, add claims to container, add affinity, add annotation
	if len(ops) < 4 {
		t.Errorf("expected at least 4 patch operations, got %d", len(ops))
	}

	hasRemove := false
	hasAddResourceClaims := false
	hasAddAnnotation := false
	hasAddAffinity := false
	for _, op := range ops {
		if op.Op == "remove" && op.Path == "/spec/containers/0/resources/requests/dra.llm-d.io~1gpu-nic-pair" {
			hasRemove = true
		}
		if op.Op == "add" && op.Path == "/spec/resourceClaims" {
			hasAddResourceClaims = true
		}
		if op.Op == "add" && op.Path == "/metadata/annotations" {
			hasAddAnnotation = true
		}
		if op.Op == "add" && op.Path == "/spec/affinity" {
			hasAddAffinity = true
		}
	}

	if !hasRemove {
		t.Error("patch missing remove operation for gpu-nic-pair resource")
	}
	if !hasAddResourceClaims {
		t.Error("patch missing add operation for resourceClaims")
	}
	if !hasAddAnnotation {
		t.Error("patch missing add operation for annotations")
	}
	if !hasAddAffinity {
		t.Error("patch missing add operation for affinity (node pinning)")
	}
}

func TestMutate_SeparateClaims(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(3)
	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("invalid patch JSON: %v", err)
	}

	// Verify resourceClaims has 3 entries (one per pair)
	for _, op := range ops {
		if op.Op == "add" && op.Path == "/spec/resourceClaims" {
			claims, ok := op.Value.([]interface{})
			if !ok {
				// JSON unmarshal produces []interface{} from json.Unmarshal
				t.Fatalf("resourceClaims value is not an array: %T", op.Value)
			}
			if len(claims) != 3 {
				t.Errorf("expected 3 separate resourceClaims, got %d", len(claims))
			}
		}
	}
}

func TestMutate_SkipAlreadyMutated(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(2)
	pod.Annotations = map[string]string{
		AnnotationMutated: "true",
	}

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch != nil {
		t.Error("expected nil patch for already-mutated pod")
	}
}

func TestMutate_NoPairRequest(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "no-gpu-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "nginx",
				},
			},
		},
	}

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch != nil {
		t.Error("expected nil patch for pod without gpu-nic-pair request")
	}
}

func TestMutate_DenyExceedsNUMA(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(5)

	_, err := m.Mutate(context.Background(), pod, "default")
	if err == nil {
		t.Fatal("expected error for 5 pairs without cross-NUMA")
	}
}

func TestMutate_AllowCrossNUMA(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(5)
	pod.Annotations = map[string]string{
		AnnotationAllowCrossNUMA: "true",
	}

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch for cross-NUMA allowed")
	}
}

func TestMutate_AllowCrossNUMASmallCount(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	// 3 pairs with allow-cross-numa: should succeed and use cross-NUMA slot selection
	pod := podWithGPUNICPairs(3)
	pod.Annotations = map[string]string{
		AnnotationAllowCrossNUMA: "true",
	}

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	// Verify that separate claim templates were created (one per pair)
	client := m.KubeClient.(*fake.Clientset)
	templates, err := client.ResourceV1().ResourceClaimTemplates("default").List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list templates: %v", err)
	}
	if len(templates.Items) != 3 {
		t.Errorf("expected 3 separate templates, got %d", len(templates.Items))
	}
}

func TestMutate_FullNodeAutoAllowsCrossNUMA(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	// Requesting all 8 pairs (full node) should auto-allow cross-NUMA
	pod := podWithGPUNICPairs(8)

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("expected 8 pairs to auto-allow cross-NUMA, got error: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch for full-node request")
	}
}

func TestMutate_DenyExceedsNodeMax(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(9)
	pod.Annotations = map[string]string{
		AnnotationAllowCrossNUMA: "true",
	}

	_, err := m.Mutate(context.Background(), pod, "default")
	if err == nil {
		t.Fatal("expected error for 9 pairs (exceeds node max)")
	}
}

func TestMutate_CreatesTemplatePerPair(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(2)
	_, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify 2 separate ResourceClaimTemplates were created
	client := m.KubeClient.(*fake.Clientset)
	templates, err := client.ResourceV1().ResourceClaimTemplates("default").List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list templates: %v", err)
	}

	if len(templates.Items) != 2 {
		t.Errorf("expected 2 templates (one per pair), got %d", len(templates.Items))
	}

	for _, tmpl := range templates.Items {
		if tmpl.Labels["app.kubernetes.io/managed-by"] != "dra-gpu-nic-webhook" {
			t.Errorf("template %s missing managed-by label", tmpl.Name)
		}
		// Each single-pair template should have exactly 2 requests (1 gpu + 1 nic)
		if len(tmpl.Spec.Spec.Devices.Requests) != 2 {
			t.Errorf("template %s should have 2 requests (1 GPU + 1 NIC), got %d",
				tmpl.Name, len(tmpl.Spec.Spec.Devices.Requests))
		}
		// Each should have 1 PCIe constraint
		if len(tmpl.Spec.Spec.Devices.Constraints) != 1 {
			t.Errorf("template %s should have 1 constraint (PCIe root), got %d",
				tmpl.Name, len(tmpl.Spec.Spec.Devices.Constraints))
		}
	}
}

func TestMutate_NodeAffinityInjected(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(1)
	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("invalid patch JSON: %v", err)
	}

	// Find the affinity patch and verify it pins to "node-1"
	found := false
	for _, op := range ops {
		if op.Op == "add" && op.Path == "/spec/affinity" {
			found = true
			// The value should contain node-1
			b, _ := json.Marshal(op.Value)
			if !contains(string(b), "node-1") {
				t.Errorf("affinity patch does not contain node-1: %s", string(b))
			}
		}
	}
	if !found {
		t.Error("patch missing affinity operation for node pinning")
	}
}

func TestMutate_NoAvailableSlots(t *testing.T) {
	cfg := testConfigWithRails()
	// Create a node but no ResourceSlices (no NICs available)
	node := fakeNode("empty-node", map[string]string{
		"kubernetes.io/hostname": "empty-node",
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
		t.Fatal("expected error when no NIC slots available")
	}
}

func TestMutate_NodeSelectorRespected(t *testing.T) {
	cfg := testConfigWithRails()

	// Two nodes: node-1 has GPUs, node-2 has GPUs, but pod's nodeSelector only matches node-2
	devices1 := make([]resourcev1.Device, 4)
	devices2 := make([]resourcev1.Device, 4)
	for i := 0; i < 4; i++ {
		devices1[i] = fakeNICDevice("nic-"+intToStr(i), "10."+intToStr(i)+".100.1", 0)
		devices2[i] = fakeNICDevice("nic-"+intToStr(i), "10."+intToStr(i)+".200.1", 0)
	}

	slice1 := fakeResourceSlice("node-1-nics", "node-1", devices1)
	slice2 := fakeResourceSlice("node-2-nics", "node-2", devices2)
	node1 := fakeNode("node-1", map[string]string{"gpu-type": "a100"})
	node2 := fakeNode("node-2", map[string]string{"gpu-type": "h100"})

	client := fake.NewSimpleClientset(slice1, slice2, node1, node2)
	allocator := NewAllocator(client.ResourceV1(), client, cfg)
	m := &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
		Allocator:      allocator,
	}

	pod := podWithGPUNICPairs(2)
	pod.Spec.NodeSelector = map[string]string{"gpu-type": "h100"}

	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ops []jsonPatchOp
	_ = json.Unmarshal(patch, &ops)

	// Verify the affinity pins to node-2 (the one matching nodeSelector)
	for _, op := range ops {
		if op.Op == "add" && op.Path == "/spec/affinity" {
			b, _ := json.Marshal(op.Value)
			if !contains(string(b), "node-2") {
				t.Errorf("expected allocation on node-2 (h100), got: %s", string(b))
			}
			if contains(string(b), "node-1") {
				t.Error("should not allocate on node-1 (a100)")
			}
		}
	}
}

func TestMutate_PendingDeconfliction(t *testing.T) {
	cfg := testConfigWithRails()
	m := newTestMutator(cfg)

	// First allocation should succeed
	pod1 := podWithGPUNICPairs(4)
	pod1.Name = "pod-1"
	patch1, err := m.Mutate(context.Background(), pod1, "default")
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}
	if patch1 == nil {
		t.Fatal("expected non-nil patch for first pod")
	}

	// Second allocation should get different rails (pending tracking)
	pod2 := podWithGPUNICPairs(4)
	pod2.Name = "pod-2"
	patch2, err := m.Mutate(context.Background(), pod2, "default")
	if err != nil {
		t.Fatalf("second allocation failed: %v", err)
	}
	if patch2 == nil {
		t.Fatal("expected non-nil patch for second pod")
	}

	// Third allocation should fail — all 8 slots consumed
	pod3 := podWithGPUNICPairs(1)
	pod3.Name = "pod-3"
	_, err = m.Mutate(context.Background(), pod3, "default")
	if err == nil {
		t.Fatal("expected error when all slots are consumed by pending allocations")
	}
}

func TestExtractGPUNICPairCount(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		wantCount   int
		wantIndices []int
		wantErr     bool
	}{
		{
			name:        "single container with request",
			pod:         podWithGPUNICPairs(4),
			wantCount:   4,
			wantIndices: []int{0},
		},
		{
			name: "no request",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx"},
					},
				},
			},
			wantCount:   0,
			wantIndices: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, indices, err := extractGPUNICPairCount(tt.pod)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if count != tt.wantCount {
				t.Errorf("count = %d, want %d", count, tt.wantCount)
			}
			if len(indices) != len(tt.wantIndices) {
				t.Errorf("indices = %v, want %v", indices, tt.wantIndices)
			}
		})
	}
}

func TestEscapeJSONPointer(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"dra.llm-d.io/gpu-nic-pair", "dra.llm-d.io~1gpu-nic-pair"},
		{"a~b", "a~0b"},
		{"a/b/c", "a~1b~1c"},
	}

	for _, tt := range tests {
		got := escapeJSONPointer(tt.input)
		if got != tt.want {
			t.Errorf("escapeJSONPointer(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// contains checks if substr is in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || searchString(s, substr))
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
