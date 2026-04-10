package webhook

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestMutator(cfg Config) *Mutator {
	client := fake.NewSimpleClientset()
	return &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
	}
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
	cfg := testConfig()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(2)
	patch, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	// Verify patch is valid JSON
	var ops []jsonPatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("invalid patch JSON: %v", err)
	}

	// Should have: remove resource request, add resourceClaims, add claims to container, add annotation
	if len(ops) < 3 {
		t.Errorf("expected at least 3 patch operations, got %d", len(ops))
	}

	// Verify patch operations
	hasRemove := false
	hasAddResourceClaims := false
	hasAddAnnotation := false
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
}

func TestMutate_SkipAlreadyMutated(t *testing.T) {
	cfg := testConfig()
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
	cfg := testConfig()
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
	cfg := testConfig()
	m := newTestMutator(cfg)

	pod := podWithGPUNICPairs(5)

	_, err := m.Mutate(context.Background(), pod, "default")
	if err == nil {
		t.Fatal("expected error for 5 pairs without cross-NUMA")
	}
}

func TestMutate_AllowCrossNUMA(t *testing.T) {
	cfg := testConfig()
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
	cfg := testConfig()
	client := fake.NewSimpleClientset()
	m := &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
	}

	// 3 pairs with allow-cross-numa should produce template WITHOUT NUMA constraint
	pod := podWithGPUNICPairs(3)
	pod.Annotations = map[string]string{
		AnnotationAllowCrossNUMA: "true",
	}

	_, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	templateName := TemplateName(3, false, cfg) // numaConstrained=false
	template, err := client.ResourceV1().ResourceClaimTemplates("default").Get(
		context.Background(), templateName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected cross-NUMA template %q: %v", templateName, err)
	}

	// Should have 3 PCIe constraints only, no NUMA constraint
	for _, c := range template.Spec.Spec.Devices.Constraints {
		if string(*c.MatchAttribute) == NUMANodeAttribute {
			t.Error("should not have NUMA constraint when allow-cross-numa is set")
		}
	}
	if len(template.Spec.Spec.Devices.Constraints) != 3 {
		t.Errorf("expected 3 constraints (PCIe only), got %d", len(template.Spec.Spec.Devices.Constraints))
	}
}

func TestMutate_FullNodeAutoAllowsCrossNUMA(t *testing.T) {
	cfg := testConfig()
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
	cfg := testConfig()
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

func TestMutate_CreatesTemplate(t *testing.T) {
	cfg := testConfig()
	client := fake.NewSimpleClientset()
	m := &Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         cfg,
	}

	pod := podWithGPUNICPairs(2)
	_, err := m.Mutate(context.Background(), pod, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the ResourceClaimTemplate was created
	templateName := TemplateName(2, true, cfg)
	template, err := client.ResourceV1().ResourceClaimTemplates("default").Get(
		context.Background(), templateName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created template: %v", err)
	}

	if template.Labels["app.kubernetes.io/managed-by"] != "dra-gpu-nic-webhook" {
		t.Error("template missing managed-by label")
	}

	// Verify template has correct number of requests
	if len(template.Spec.Spec.Devices.Requests) != 4 {
		t.Errorf("template should have 4 requests (2 GPU + 2 NIC), got %d", len(template.Spec.Spec.Devices.Requests))
	}
}

func TestExtractGPUNICPairCount(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		wantCount     int
		wantIndices   []int
		wantErr       bool
	}{
		{
			name:      "single container with request",
			pod:       podWithGPUNICPairs(4),
			wantCount: 4,
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
