package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
)

// Mutator handles pod mutation for GPU-NIC pair requests.
type Mutator struct {
	KubeClient     kubernetes.Interface
	ResourceClient resourceclient.ResourceV1Interface
	Config         Config
	Allocator      *Allocator // cluster-level GPU-NIC pair allocator
}

// jsonPatchOp represents a single JSON Patch operation.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Mutate processes a pod and returns a JSON patch if mutation is needed.
// Returns nil patch if no mutation is required, or an error string for denial.
func (m *Mutator) Mutate(ctx context.Context, pod *corev1.Pod, namespace string) ([]byte, error) {
	// Skip already-mutated pods
	if pod.Annotations != nil && pod.Annotations[AnnotationMutated] == "true" {
		return nil, nil
	}

	// Find the gpu-nic-pair resource request across all containers
	count, containerIndices, err := extractGPUNICPairCount(pod)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}

	// Check cross-NUMA annotation
	allowCrossNUMA := pod.Annotations != nil && pod.Annotations[AnnotationAllowCrossNUMA] == "true"

	// Validate
	if err := ValidateRequest(count, allowCrossNUMA, m.Config); err != nil {
		return nil, err
	}

	// Determine NUMA constraint mode:
	// - Full node (count == max): always cross-NUMA (no point constraining)
	// - Explicit allow-cross-numa annotation: respect it for any count
	// - Otherwise: enforce NUMA locality
	numaConstrained := !allowCrossNUMA && count < m.Config.MaxPairsPerNode

	// Use the cluster-level allocator to pick a node and rails,
	// respecting pod affinity/anti-affinity constraints.
	if m.Allocator == nil {
		return nil, fmt.Errorf("allocator not configured")
	}

	var nodeName string
	templateNames := make([]string, count)

	if m.Config.IsExplicitMode() {
		result, err := m.Allocator.AllocateExplicit(ctx, pod, namespace, count, numaConstrained)
		if err != nil {
			return nil, fmt.Errorf("explicit allocation failed: %w", err)
		}
		nodeName = result.NodeName

		for i, pair := range result.Pairs {
			mapping := ExplicitPairMapping{Devices: pair.Devices, Rail: pair.RailIndex}
			spec, err := BuildExplicitPairClaimSpec(pair.NICIndex, pair.RailIndex, mapping, m.Config)
			if err != nil {
				return nil, fmt.Errorf("failed to build explicit claim spec for pair %d: %w", i, err)
			}

			name := ExplicitPairTemplateName(pair.NICIndex, pair.RailIndex, mapping, m.Config)
			if err := m.ensureClaimTemplate(ctx, namespace, name, spec); err != nil {
				return nil, fmt.Errorf("failed to ensure claim template for pair %d: %w", i, err)
			}
			templateNames[i] = name
		}

		klog.InfoS("Mutating pod (explicit mode)", "namespace", namespace, "pod", podName(pod),
			"gpu-nic-pairs", count, "numaConstrained", numaConstrained,
			"node", nodeName, "templates", templateNames)
	} else {
		result, err := m.Allocator.Allocate(ctx, pod, namespace, count, numaConstrained)
		if err != nil {
			return nil, fmt.Errorf("allocation failed: %w", err)
		}
		nodeName = result.NodeName

		for i := 0; i < count; i++ {
			railIdx := result.RailIndices[i]

			spec, err := BuildSinglePairClaimSpec(i, railIdx, m.Config)
			if err != nil {
				return nil, fmt.Errorf("failed to build claim spec for pair %d: %w", i, err)
			}

			name := SinglePairTemplateName(i, railIdx, m.Config)
			if err := m.ensureClaimTemplate(ctx, namespace, name, spec); err != nil {
				return nil, fmt.Errorf("failed to ensure claim template for pair %d: %w", i, err)
			}
			templateNames[i] = name
		}

		klog.InfoS("Mutating pod", "namespace", namespace, "pod", podName(pod),
			"gpu-nic-pairs", count, "numaConstrained", numaConstrained,
			"node", nodeName, "rails", result.RailIndices,
			"templates", templateNames)
	}

	// Build JSON patch: N separate ResourceClaims + node affinity
	requestNames := []string{"gpu", "nic"}
	if m.Config.IsExplicitMode() {
		requestNames = m.Config.DeviceSelectorKeys()
	}
	patch, err := buildSeparateClaimsPatch(pod, count, templateNames, nodeName, containerIndices, requestNames)
	if err != nil {
		return nil, fmt.Errorf("failed to build patch: %w", err)
	}

	return patch, nil
}

// extractGPUNICPairCount finds the gpu-nic-pair resource request in the pod's containers.
// Returns the total count and the indices of containers that had the request.
func extractGPUNICPairCount(pod *corev1.Pod) (int, []int, error) {
	totalCount := 0
	var containerIndices []int

	for i, c := range pod.Spec.Containers {
		if c.Resources.Requests != nil {
			if q, ok := c.Resources.Requests[corev1.ResourceName(ResourceGPUNICPair)]; ok {
				val, ok := q.AsInt64()
				if !ok {
					return 0, nil, fmt.Errorf("container %q: %s must be an integer, got %s",
						c.Name, ResourceGPUNICPair, q.String())
				}
				totalCount += int(val)
				containerIndices = append(containerIndices, i)
			}
		}
		if c.Resources.Limits != nil {
			if q, ok := c.Resources.Limits[corev1.ResourceName(ResourceGPUNICPair)]; ok {
				// If only limits is set (no requests), use limits value
				if c.Resources.Requests == nil || c.Resources.Requests[corev1.ResourceName(ResourceGPUNICPair)] == (c.Resources.Limits[corev1.ResourceName(ResourceGPUNICPair)]) {
					continue // already counted from requests
				}
				val, ok := q.AsInt64()
				if !ok {
					return 0, nil, fmt.Errorf("container %q: %s limit must be an integer, got %s",
						c.Name, ResourceGPUNICPair, q.String())
				}
				_ = val // limits match requests for extended resources
			}
		}
	}

	return totalCount, containerIndices, nil
}

// ensureClaimTemplate creates a ResourceClaimTemplate if it doesn't exist.
func (m *Mutator) ensureClaimTemplate(ctx context.Context, namespace, name string, spec resourcev1.ResourceClaimSpec) error {
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "dra-gpu-nic-webhook",
			},
		},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: spec,
		},
	}

	_, err := m.ResourceClient.ResourceClaimTemplates(namespace).Create(ctx, template, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			klog.V(2).InfoS("ResourceClaimTemplate already exists", "namespace", namespace, "name", name)
			return nil
		}
		return err
	}

	klog.InfoS("Created ResourceClaimTemplate", "namespace", namespace, "name", name)
	return nil
}

// buildSeparateClaimsPatch creates the JSON patch for N separate ResourceClaims
// (one per GPU-NIC pair) and adds a nodeAffinity to pin the pod to the
// allocator-selected node.
func buildSeparateClaimsPatch(pod *corev1.Pod, count int, templateNames []string, nodeName string, containerIndices []int, requestNames []string) ([]byte, error) {
	var patches []jsonPatchOp

	// 1. Remove dra.llm-d.io/gpu-nic-pair from resources.requests and limits
	for _, idx := range containerIndices {
		patches = append(patches, jsonPatchOp{
			Op:   "remove",
			Path: fmt.Sprintf("/spec/containers/%d/resources/requests/%s", idx, escapeJSONPointer(ResourceGPUNICPair)),
		})
		c := pod.Spec.Containers[idx]
		if c.Resources.Limits != nil {
			if _, ok := c.Resources.Limits[corev1.ResourceName(ResourceGPUNICPair)]; ok {
				patches = append(patches, jsonPatchOp{
					Op:   "remove",
					Path: fmt.Sprintf("/spec/containers/%d/resources/limits/%s", idx, escapeJSONPointer(ResourceGPUNICPair)),
				})
			}
		}
	}

	// 2. Add N separate spec.resourceClaims entries (one per pair)
	podResourceClaims := make([]corev1.PodResourceClaim, count)
	for i := 0; i < count; i++ {
		podResourceClaims[i] = corev1.PodResourceClaim{
			Name:                      fmt.Sprintf("gpu-nic-pair-%d", i),
			ResourceClaimTemplateName: strPtr(templateNames[i]),
		}
	}

	if pod.Spec.ResourceClaims == nil {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/resourceClaims",
			Value: podResourceClaims,
		})
	} else {
		for _, prc := range podResourceClaims {
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/resourceClaims/-",
				Value: prc,
			})
		}
	}

	// 3. Add resources.claims to each container — each pair contributes
	//    one request per device role from its own claim.
	for _, idx := range containerIndices {
		claims := make([]corev1.ResourceClaim, 0, count*len(requestNames))
		for i := 0; i < count; i++ {
			claimName := fmt.Sprintf("gpu-nic-pair-%d", i)
			for _, reqName := range requestNames {
				claims = append(claims, corev1.ResourceClaim{Name: claimName, Request: reqName})
			}
		}

		c := pod.Spec.Containers[idx]
		if c.Resources.Claims == nil {
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/containers/%d/resources/claims", idx),
				Value: claims,
			})
		} else {
			for _, claim := range claims {
				patches = append(patches, jsonPatchOp{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/resources/claims/-", idx),
					Value: claim,
				})
			}
		}
	}

	// 4. Add nodeAffinity to pin pod to the allocator-selected node.
	//    Merges with existing affinity if present.
	nodeSelectorTerm := corev1.NodeSelectorTerm{
		MatchFields: []corev1.NodeSelectorRequirement{{
			Key:      "metadata.name",
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{nodeName},
		}},
	}

	if pod.Spec.Affinity == nil {
		patches = append(patches, jsonPatchOp{
			Op:   "add",
			Path: "/spec/affinity",
			Value: corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{nodeSelectorTerm},
					},
				},
			},
		})
	} else if pod.Spec.Affinity.NodeAffinity == nil {
		patches = append(patches, jsonPatchOp{
			Op:   "add",
			Path: "/spec/affinity/nodeAffinity",
			Value: corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{nodeSelectorTerm},
				},
			},
		})
	} else if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		patches = append(patches, jsonPatchOp{
			Op:   "add",
			Path: "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution",
			Value: corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{nodeSelectorTerm},
			},
		})
	} else {
		// Replace existing terms with intersection (the allocator already
		// verified the selected node satisfies existing constraints)
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/-",
			Value: nodeSelectorTerm,
		})
	}

	// 5. Add mutated annotation
	if pod.Annotations == nil {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{AnnotationMutated: "true"},
		})
	} else {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations/" + escapeJSONPointer(AnnotationMutated),
			Value: "true",
		})
	}

	return json.Marshal(patches)
}

// escapeJSONPointer escapes a string for use in a JSON Pointer (RFC 6901).
// '~' becomes '~0', '/' becomes '~1'.
func escapeJSONPointer(s string) string {
	result := ""
	for _, c := range s {
		switch c {
		case '~':
			result += "~0"
		case '/':
			result += "~1"
		default:
			result += string(c)
		}
	}
	return result
}

func strPtr(s string) *string {
	return &s
}

func podName(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	if pod.GenerateName != "" {
		return pod.GenerateName + "<generated>"
	}
	return "<unknown>"
}

// intToStr converts int to string (used in JSON patch paths).
func intToStr(i int) string {
	return strconv.Itoa(i)
}
