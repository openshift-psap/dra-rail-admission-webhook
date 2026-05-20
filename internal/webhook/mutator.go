package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
)

// ContainerResourceCount tracks how many of a resource a specific container requested.
type ContainerResourceCount struct {
	ContainerIndex int
	Count          int
}

// InterceptedResourceRequest represents an extended resource found in a pod's
// containers that needs to be converted to DRA ResourceClaims.
type InterceptedResourceRequest struct {
	ResourceName    string
	DeviceClassName string
	PerContainer    []ContainerResourceCount
}

// TotalCount returns the sum of all per-container counts.
func (r InterceptedResourceRequest) TotalCount() int {
	total := 0
	for _, c := range r.PerContainer {
		total += c.Count
	}
	return total
}

// Mutator handles pod mutation for GPU-NIC pair requests.
type Mutator struct {
	KubeClient     kubernetes.Interface
	ResourceClient resourceclient.ResourceV1Interface
	Config         Config
	Allocator      *Allocator      // cluster-level GPU-NIC pair allocator
	CIDRPoolCache  *CIDRPoolCache  // nil when not in cidrpool mode
	IPAllocator    *IPAllocator    // nil when not in cidrpool mode
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

	// Extract both resource types
	pairCount, pairContainerIndices, err := extractGPUNICPairCount(pod)
	if err != nil {
		return nil, err
	}

	interceptMap := m.Config.InterceptedResourceMap()
	interceptedReqs, err := extractInterceptedResources(pod, interceptMap)
	if err != nil {
		return nil, err
	}

	if pairCount == 0 && len(interceptedReqs) == 0 {
		return nil, nil
	}

	if pairCount > 0 && len(interceptedReqs) > 0 {
		interceptedNames := make([]string, len(interceptedReqs))
		for i, r := range interceptedReqs {
			interceptedNames[i] = r.ResourceName
		}
		return nil, fmt.Errorf(
			"pod requests both %s and intercepted resource(s) %v; "+
				"these are mutually exclusive because both allocate from the same GPU pool",
			ResourceGPUNICPair, interceptedNames)
	}

	if m.Allocator == nil {
		return nil, fmt.Errorf("allocator not configured")
	}

	var allPatches []jsonPatchOp
	var selectedNode string
	firstPodClaim := true
	containerClaimRefAdded := make(map[int]bool)

	// Phase 1: GPU-NIC pair handling (existing logic)
	if pairCount > 0 {
		allowCrossNUMA := pod.Annotations != nil && pod.Annotations[AnnotationAllowCrossNUMA] == "true"
		if err := ValidateRequest(pairCount, allowCrossNUMA, m.Config); err != nil {
			return nil, err
		}

		numaConstrained := !allowCrossNUMA && pairCount < m.Config.MaxPairsPerNode
		templateNames := make([]string, pairCount)

		if m.Config.IsExplicitMode() {
			result, err := m.Allocator.AllocateExplicit(ctx, pod, namespace, pairCount, numaConstrained)
			if err != nil {
				return nil, fmt.Errorf("explicit allocation failed: %w", err)
			}
			selectedNode = result.NodeName

			for i, pair := range result.Pairs {
				var gateway string
				var addresses []string

				if m.Config.IsCIDRPoolMode() {
					alloc, ok := m.CIDRPoolCache.GetAllocation(pair.RailIndex, result.NodeName)
					if !ok {
						return nil, fmt.Errorf("no cidrpool allocation for node %q rail %d", result.NodeName, pair.RailIndex)
					}
					gateway = alloc.Gateway
					claimRef := fmt.Sprintf("%s/%s", namespace, podName(pod))
					ipCIDR, err := m.IPAllocator.AllocateIP(result.NodeName, pair.RailIndex, alloc.Prefix, claimRef, alloc.ReservedIPs)
					if err != nil {
						return nil, fmt.Errorf("failed to allocate IP for pair %d: %w", i, err)
					}
					addresses = []string{ipCIDR}
				} else {
					railGW := ""
					if pair.RailIndex >= 0 && pair.RailIndex < len(m.Config.NICConfig.Rails) {
						railGW = m.Config.NICConfig.Rails[pair.RailIndex].Gateway
					}
					var err error
					gateway, err = m.Config.NICConfig.ResolveGateway(result.NodeName, pair.RailIndex, railGW)
					if err != nil {
						return nil, fmt.Errorf("failed to resolve gateway for pair %d: %w", i, err)
					}
				}

				mapping := ExplicitPairMapping{Devices: pair.Devices, Rail: pair.RailIndex}
				spec, err := BuildExplicitPairClaimSpec(pair.NICIndex, pair.RailIndex, mapping, m.Config, gateway, addresses...)
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
				"gpu-nic-pairs", pairCount, "numaConstrained", numaConstrained,
				"node", selectedNode, "templates", templateNames)
		} else {
			result, err := m.Allocator.Allocate(ctx, pod, namespace, pairCount, numaConstrained)
			if err != nil {
				return nil, fmt.Errorf("allocation failed: %w", err)
			}
			selectedNode = result.NodeName

			for i := 0; i < pairCount; i++ {
				railIdx := result.RailIndices[i]
				var gateway string
				var addresses []string

				if m.Config.IsCIDRPoolMode() {
					alloc, ok := m.CIDRPoolCache.GetAllocation(railIdx, result.NodeName)
					if !ok {
						return nil, fmt.Errorf("no cidrpool allocation for node %q rail %d", result.NodeName, railIdx)
					}
					gateway = alloc.Gateway
					claimRef := fmt.Sprintf("%s/%s", namespace, podName(pod))
					ipCIDR, err := m.IPAllocator.AllocateIP(result.NodeName, railIdx, alloc.Prefix, claimRef, alloc.ReservedIPs)
					if err != nil {
						return nil, fmt.Errorf("failed to allocate IP for pair %d: %w", i, err)
					}
					addresses = []string{ipCIDR}
				} else {
					railGW := ""
					if railIdx >= 0 && railIdx < len(m.Config.NICConfig.Rails) {
						railGW = m.Config.NICConfig.Rails[railIdx].Gateway
					}
					gateway, err = m.Config.NICConfig.ResolveGateway(result.NodeName, railIdx, railGW)
					if err != nil {
						return nil, fmt.Errorf("failed to resolve gateway for pair %d: %w", i, err)
					}
				}

				spec, err := BuildSinglePairClaimSpec(i, railIdx, m.Config, gateway, addresses...)
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
				"gpu-nic-pairs", pairCount, "numaConstrained", numaConstrained,
				"node", selectedNode, "rails", result.RailIndices,
				"templates", templateNames)
		}

		// Build pair patches
		allPatches = append(allPatches, buildResourceRemovalPatches(pod, pairContainerIndices, ResourceGPUNICPair)...)

		podClaims := make([]corev1.PodResourceClaim, pairCount)
		for i := 0; i < pairCount; i++ {
			podClaims[i] = corev1.PodResourceClaim{
				Name:                      fmt.Sprintf("gpu-nic-pair-%d", i),
				ResourceClaimTemplateName: strPtr(templateNames[i]),
			}
		}
		allPatches = append(allPatches, buildPodClaimPatches(pod, podClaims, firstPodClaim)...)
		firstPodClaim = false

		requestNames := []string{"gpu", "nic"}
		if m.Config.IsExplicitMode() {
			requestNames = m.Config.DeviceSelectorKeys()
		}
		for _, idx := range pairContainerIndices {
			refs := make([]corev1.ResourceClaim, 0, pairCount*len(requestNames))
			for i := 0; i < pairCount; i++ {
				claimName := fmt.Sprintf("gpu-nic-pair-%d", i)
				for _, reqName := range requestNames {
					refs = append(refs, corev1.ResourceClaim{Name: claimName, Request: reqName})
				}
			}
			allPatches = append(allPatches, buildContainerClaimRefPatches(pod, idx, refs, !containerClaimRefAdded[idx])...)
			containerClaimRefAdded[idx] = true
		}
	}

	// Phase 2: Intercepted extended resources
	for _, req := range interceptedReqs {
		totalCount := req.TotalCount()
		if totalCount <= 0 {
			continue
		}

		nodeName, err := m.Allocator.AllocateExtendedResource(ctx, pod, namespace, totalCount, req.DeviceClassName, selectedNode)
		if err != nil {
			return nil, fmt.Errorf("extended resource allocation failed for %s: %w", req.ResourceName, err)
		}
		if selectedNode == "" {
			selectedNode = nodeName
		}

		// Build templates for each device
		templateNames := make([]string, totalCount)
		for i := 0; i < totalCount; i++ {
			spec := BuildExtendedResourceClaimSpec(req.DeviceClassName)
			name := ExtendedResourceTemplateName(i, req.DeviceClassName, m.Config)
			if err := m.ensureClaimTemplate(ctx, namespace, name, spec); err != nil {
				return nil, fmt.Errorf("failed to ensure extended resource template %d: %w", i, err)
			}
			templateNames[i] = name
		}

		klog.InfoS("Mutating pod (extended resource interception)", "namespace", namespace,
			"pod", podName(pod), "resource", req.ResourceName,
			"deviceClass", req.DeviceClassName, "count", totalCount,
			"node", nodeName, "templates", templateNames)

		// Remove intercepted resource from containers
		containerIndices := make([]int, len(req.PerContainer))
		for i, pc := range req.PerContainer {
			containerIndices[i] = pc.ContainerIndex
		}
		allPatches = append(allPatches, buildResourceRemovalPatches(pod, containerIndices, req.ResourceName)...)

		// Add pod-level claims
		// Scan existing pod claim names for collision avoidance
		existingNames := make(map[string]bool)
		for _, rc := range pod.Spec.ResourceClaims {
			existingNames[rc.Name] = true
		}
		podClaims := make([]corev1.PodResourceClaim, totalCount)
		for i := 0; i < totalCount; i++ {
			claimName := fmt.Sprintf("ext-%s-%d", escapeClaimName(req.ResourceName), i)
			for existingNames[claimName] {
				claimName = claimName + "-x"
			}
			existingNames[claimName] = true
			podClaims[i] = corev1.PodResourceClaim{
				Name:                      claimName,
				ResourceClaimTemplateName: strPtr(templateNames[i]),
			}
		}
		allPatches = append(allPatches, buildPodClaimPatches(pod, podClaims, firstPodClaim)...)
		firstPodClaim = false

		// Per-container claim refs: each container gets only its claims
		claimOffset := 0
		for _, pc := range req.PerContainer {
			refs := make([]corev1.ResourceClaim, pc.Count)
			for j := 0; j < pc.Count; j++ {
				refs[j] = corev1.ResourceClaim{
					Name:    podClaims[claimOffset+j].Name,
					Request: "device",
				}
			}
			allPatches = append(allPatches, buildContainerClaimRefPatches(pod, pc.ContainerIndex, refs, !containerClaimRefAdded[pc.ContainerIndex])...)
			containerClaimRefAdded[pc.ContainerIndex] = true
			claimOffset += pc.Count
		}
	}

	// Phase 3: Shared patches — node affinity + annotation
	allPatches = append(allPatches, buildNodeAffinityPatch(pod, selectedNode)...)
	allPatches = append(allPatches, buildMutatedAnnotationPatch(pod)...)

	return json.Marshal(allPatches)
}

// MutateExtOnly processes a pod for intercepted extended resources only.
// It ignores gpu-nic-pair requests. Used by the /mutate-ext endpoint which
// serves all non-system namespaces.
func (m *Mutator) MutateExtOnly(ctx context.Context, pod *corev1.Pod, namespace string) ([]byte, error) {
	if pod.Annotations != nil && pod.Annotations[AnnotationMutated] == "true" {
		return nil, nil
	}

	interceptMap := m.Config.InterceptedResourceMap()
	interceptedReqs, err := extractInterceptedResources(pod, interceptMap)
	if err != nil {
		return nil, err
	}

	if len(interceptedReqs) == 0 {
		return nil, nil
	}

	if m.Allocator == nil {
		return nil, fmt.Errorf("allocator not configured")
	}

	var allPatches []jsonPatchOp
	var selectedNode string
	firstPodClaim := true
	containerClaimRefAdded := make(map[int]bool)

	for _, req := range interceptedReqs {
		totalCount := req.TotalCount()
		if totalCount <= 0 {
			continue
		}

		nodeName, err := m.Allocator.AllocateExtendedResource(ctx, pod, namespace, totalCount, req.DeviceClassName, selectedNode)
		if err != nil {
			return nil, fmt.Errorf("extended resource allocation failed for %s: %w", req.ResourceName, err)
		}
		if selectedNode == "" {
			selectedNode = nodeName
		}

		templateNames := make([]string, totalCount)
		for i := 0; i < totalCount; i++ {
			spec := BuildExtendedResourceClaimSpec(req.DeviceClassName)
			name := ExtendedResourceTemplateName(i, req.DeviceClassName, m.Config)
			if err := m.ensureClaimTemplate(ctx, namespace, name, spec); err != nil {
				return nil, fmt.Errorf("failed to ensure extended resource template %d: %w", i, err)
			}
			templateNames[i] = name
		}

		klog.InfoS("Mutating pod (extended resource interception)", "namespace", namespace,
			"pod", podName(pod), "resource", req.ResourceName,
			"deviceClass", req.DeviceClassName, "count", totalCount,
			"node", nodeName, "templates", templateNames)

		containerIndices := make([]int, len(req.PerContainer))
		for i, pc := range req.PerContainer {
			containerIndices[i] = pc.ContainerIndex
		}
		allPatches = append(allPatches, buildResourceRemovalPatches(pod, containerIndices, req.ResourceName)...)

		existingNames := make(map[string]bool)
		for _, rc := range pod.Spec.ResourceClaims {
			existingNames[rc.Name] = true
		}
		podClaims := make([]corev1.PodResourceClaim, totalCount)
		for i := 0; i < totalCount; i++ {
			claimName := fmt.Sprintf("ext-%s-%d", escapeClaimName(req.ResourceName), i)
			for existingNames[claimName] {
				claimName = claimName + "-x"
			}
			existingNames[claimName] = true
			podClaims[i] = corev1.PodResourceClaim{
				Name:                      claimName,
				ResourceClaimTemplateName: strPtr(templateNames[i]),
			}
		}
		allPatches = append(allPatches, buildPodClaimPatches(pod, podClaims, firstPodClaim)...)
		firstPodClaim = false

		claimOffset := 0
		for _, pc := range req.PerContainer {
			refs := make([]corev1.ResourceClaim, pc.Count)
			for j := 0; j < pc.Count; j++ {
				refs[j] = corev1.ResourceClaim{
					Name:    podClaims[claimOffset+j].Name,
					Request: "device",
				}
			}
			allPatches = append(allPatches, buildContainerClaimRefPatches(pod, pc.ContainerIndex, refs, !containerClaimRefAdded[pc.ContainerIndex])...)
			containerClaimRefAdded[pc.ContainerIndex] = true
			claimOffset += pc.Count
		}
	}

	allPatches = append(allPatches, buildNodeAffinityPatch(pod, selectedNode)...)
	allPatches = append(allPatches, buildMutatedAnnotationPatch(pod)...)

	return json.Marshal(allPatches)
}

// escapeClaimName makes a resource name safe for use in claim names.
// Replaces '/' and '.' with '-'.
func escapeClaimName(name string) string {
	result := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == '.' {
			result[i] = '-'
		} else {
			result[i] = name[i]
		}
	}
	return string(result)
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

// extractInterceptedResources finds extended resources in the pod's containers
// that match the configured interception list. Returns one entry per matched
// resource with per-container counts.
func extractInterceptedResources(pod *corev1.Pod, interceptMap map[string]string) ([]InterceptedResourceRequest, error) {
	if len(interceptMap) == 0 {
		return nil, nil
	}

	type accumulator struct {
		deviceClassName string
		perContainer    []ContainerResourceCount
	}
	byResource := make(map[string]*accumulator)

	for i, c := range pod.Spec.Containers {
		if c.Resources.Requests == nil {
			continue
		}
		for resName, deviceClass := range interceptMap {
			q, ok := c.Resources.Requests[corev1.ResourceName(resName)]
			if !ok {
				continue
			}
			val, ok := q.AsInt64()
			if !ok {
				return nil, fmt.Errorf("container %q: %s must be an integer, got %s", c.Name, resName, q.String())
			}
			if val <= 0 {
				continue
			}
			acc, exists := byResource[resName]
			if !exists {
				acc = &accumulator{deviceClassName: deviceClass}
				byResource[resName] = acc
			}
			acc.perContainer = append(acc.perContainer, ContainerResourceCount{
				ContainerIndex: i,
				Count:          int(val),
			})
		}
	}

	if len(byResource) == 0 {
		return nil, nil
	}

	result := make([]InterceptedResourceRequest, 0, len(byResource))
	for resName, acc := range byResource {
		result = append(result, InterceptedResourceRequest{
			ResourceName:    resName,
			DeviceClassName: acc.deviceClassName,
			PerContainer:    acc.perContainer,
		})
	}
	return result, nil
}

// countInterceptedGPUs quickly sums intercepted resource counts for queue priority.
func countInterceptedGPUs(pod *corev1.Pod, interceptMap map[string]string) int {
	if len(interceptMap) == 0 {
		return 0
	}
	total := 0
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Resources.Requests == nil {
			continue
		}
		for resName := range interceptMap {
			if q, ok := c.Resources.Requests[corev1.ResourceName(resName)]; ok {
				if val, ok := q.AsInt64(); ok && val > 0 {
					total += int(val)
				}
			}
		}
	}
	return total
}

// ensureClaimTemplate creates a ResourceClaimTemplate if it doesn't exist.
// On AlreadyExists, fetches the existing template and verifies the spec matches.
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
			existing, getErr := m.ResourceClient.ResourceClaimTemplates(namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("template %s exists but failed to verify: %w", name, getErr)
			}
			if !equality.Semantic.DeepEqual(existing.Spec.Spec, spec) {
				return fmt.Errorf("template %s exists with divergent spec", name)
			}
			klog.V(2).InfoS("ResourceClaimTemplate already exists", "namespace", namespace, "name", name)
			return nil
		}
		return err
	}

	klog.InfoS("Created ResourceClaimTemplate", "namespace", namespace, "name", name)
	return nil
}

// buildResourceRemovalPatches generates JSON patch ops to remove a resource
// from the specified containers' requests and limits.
func buildResourceRemovalPatches(pod *corev1.Pod, containerIndices []int, resourceName string) []jsonPatchOp {
	var patches []jsonPatchOp
	for _, idx := range containerIndices {
		patches = append(patches, jsonPatchOp{
			Op:   "remove",
			Path: fmt.Sprintf("/spec/containers/%d/resources/requests/%s", idx, escapeJSONPointer(resourceName)),
		})
		c := pod.Spec.Containers[idx]
		if c.Resources.Limits != nil {
			if _, ok := c.Resources.Limits[corev1.ResourceName(resourceName)]; ok {
				patches = append(patches, jsonPatchOp{
					Op:   "remove",
					Path: fmt.Sprintf("/spec/containers/%d/resources/limits/%s", idx, escapeJSONPointer(resourceName)),
				})
			}
		}
	}
	return patches
}

// buildPodClaimPatches generates JSON patch ops to add ResourceClaims at the
// pod level. firstBatch is true when no prior claim patches have been added
// (determines whether to create or append to the spec.resourceClaims array).
func buildPodClaimPatches(pod *corev1.Pod, claims []corev1.PodResourceClaim, firstBatch bool) []jsonPatchOp {
	var patches []jsonPatchOp
	if pod.Spec.ResourceClaims == nil && firstBatch {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/resourceClaims",
			Value: claims,
		})
	} else {
		for _, prc := range claims {
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/resourceClaims/-",
				Value: prc,
			})
		}
	}
	return patches
}

// buildContainerClaimRefPatches generates JSON patch ops to add claim references
// to a specific container. firstBatch is true when no prior claim ref patches
// have been added to this container.
func buildContainerClaimRefPatches(pod *corev1.Pod, containerIndex int, refs []corev1.ResourceClaim, firstBatch bool) []jsonPatchOp {
	var patches []jsonPatchOp
	c := pod.Spec.Containers[containerIndex]
	if c.Resources.Claims == nil && firstBatch {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  fmt.Sprintf("/spec/containers/%d/resources/claims", containerIndex),
			Value: refs,
		})
	} else {
		for _, ref := range refs {
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/containers/%d/resources/claims/-", containerIndex),
				Value: ref,
			})
		}
	}
	return patches
}

// buildNodeAffinityPatch generates JSON patch ops to pin a pod to a node.
// When the pod already has required node affinity terms, the node pin MatchField
// is added to each existing term (intersection semantics: terms are OR'd, so
// adding a MatchField to each narrows every branch).
func buildNodeAffinityPatch(pod *corev1.Pod, nodeName string) []jsonPatchOp {
	nodeField := corev1.NodeSelectorRequirement{
		Key:      "metadata.name",
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{nodeName},
	}

	nodeSelectorTerm := corev1.NodeSelectorTerm{
		MatchFields: []corev1.NodeSelectorRequirement{nodeField},
	}

	var patches []jsonPatchOp

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
		// Existing terms are OR'd — add the node MatchField to each term
		// so every branch is narrowed to this node (intersection).
		terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		for i := range terms {
			patches = append(patches, jsonPatchOp{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/%d/matchFields/-", i),
				Value: nodeField,
			})
		}
	}

	return patches
}

// buildMutatedAnnotationPatch generates the JSON patch op for the mutated annotation.
func buildMutatedAnnotationPatch(pod *corev1.Pod) []jsonPatchOp {
	if pod.Annotations == nil {
		return []jsonPatchOp{{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{AnnotationMutated: "true"},
		}}
	}
	return []jsonPatchOp{{
		Op:    "add",
		Path:  "/metadata/annotations/" + escapeJSONPointer(AnnotationMutated),
		Value: "true",
	}}
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
