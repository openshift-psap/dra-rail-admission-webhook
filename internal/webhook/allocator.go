package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
)

// NICSlot represents a single available RDMA NIC on a specific node.
type NICSlot struct {
	NodeName  string
	RailIndex int
	NUMAZone  int
	IPv4      string
}

// AllocationResult is returned by the allocator with the selected node
// and rail indices for the requested GPU-NIC pairs.
type AllocationResult struct {
	NodeName    string
	RailIndices []int
}

// Allocator handles cluster-level GPU-NIC pair allocation. It scans
// ResourceSlices for per-node NIC availability, respects pod scheduling
// constraints (nodeSelector, nodeAffinity, podAntiAffinity, podAffinity),
// and tracks pending allocations to prevent double-booking.
type Allocator struct {
	ResourceClient resourceclient.ResourceV1Interface
	KubeClient     kubernetes.Interface
	Config         Config

	mu      sync.Mutex
	pending map[string]time.Time // "node:rail" → reservation timestamp

	pendingTTL time.Duration // how long pending entries remain valid
}

// NewAllocator creates an Allocator with an empty pending set.
// Pending entries expire after 2 minutes, allowing stale reservations
// (from pods that were rejected or deleted) to be reclaimed.
func NewAllocator(rc resourceclient.ResourceV1Interface, kc kubernetes.Interface, cfg Config) *Allocator {
	return &Allocator{
		ResourceClient: rc,
		KubeClient:     kc,
		Config:         cfg,
		pending:        make(map[string]time.Time),
		pendingTTL:     2 * time.Minute,
	}
}

// ClearPending resets the in-memory pending allocations.
func (a *Allocator) ClearPending() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = make(map[string]time.Time)
}

// expirePending removes pending entries older than pendingTTL.
// Must be called with a.mu held.
func (a *Allocator) expirePending() {
	cutoff := time.Now().Add(-a.pendingTTL)
	for key, ts := range a.pending {
		if ts.Before(cutoff) {
			delete(a.pending, key)
		}
	}
}

// Allocate finds a node with enough free GPU-NIC pair slots that satisfies
// the pod's scheduling constraints. It returns the chosen node and the
// rail indices to use for each pair.
func (a *Allocator) Allocate(ctx context.Context, pod *corev1.Pod, namespace string, count int, numaConstrained bool) (*AllocationResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Expire stale pending entries
	a.expirePending()

	// 1. Scan ResourceSlices for available NIC slots per node
	nodeSlots, err := a.scanAvailableSlots(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to scan cluster: %w", err)
	}

	// 2. Remove slots that are pending (reserved by earlier requests)
	for key := range a.pending {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		nodeName := parts[0]
		var railIdx int
		_, _ = fmt.Sscanf(parts[1], "%d", &railIdx)

		slots := nodeSlots[nodeName]
		filtered := make([]NICSlot, 0, len(slots))
		for _, s := range slots {
			if s.RailIndex != railIdx {
				filtered = append(filtered, s)
			}
		}
		nodeSlots[nodeName] = filtered
	}

	// 3. Filter candidate nodes by pod scheduling constraints
	candidateNodes, err := a.filterNodesByConstraints(ctx, pod, namespace, nodeSlots)
	if err != nil {
		return nil, err
	}
	if len(candidateNodes) == 0 {
		return nil, fmt.Errorf("no candidate nodes satisfy pod scheduling constraints")
	}

	// 4. Sort candidates by node packing strategy: always prefer the
	// most-utilized node (fewest free slots first). This packs workloads
	// together, leaving full nodes available. The priority queue ensures
	// larger requests (prefill) are processed first and claim full nodes;
	// smaller requests (decode) then pack onto the remaining nodes.
	// selectSlots() still gates on actual capacity, so a node that can't
	// fit the request is skipped automatically.
	sort.Slice(candidateNodes, func(i, j int) bool {
		return len(nodeSlots[candidateNodes[i]]) < len(nodeSlots[candidateNodes[j]])
	})

	// 5. Find a node with enough free slots
	for _, nodeName := range candidateNodes {
		slots := nodeSlots[nodeName]
		rails := selectSlots(slots, count, numaConstrained, a.Config.MaxPairsPerNUMA)
		if len(rails) >= count {
			selected := rails[:count]

			// Mark as pending so subsequent requests don't double-book
			now := time.Now()
			for _, rail := range selected {
				a.pending[fmt.Sprintf("%s:%d", nodeName, rail)] = now
			}

			klog.InfoS("Allocated GPU-NIC slots",
				"node", nodeName, "rails", selected, "count", count,
				"numaConstrained", numaConstrained)
			return &AllocationResult{
				NodeName:    nodeName,
				RailIndices: selected,
			}, nil
		}
	}

	return nil, fmt.Errorf("no node has %d available GPU-NIC pairs with given constraints (candidates: %v)",
		count, candidateNodes)
}

// scanAvailableSlots reads ResourceSlices and returns available NIC slots
// grouped by node. A NIC is available if it has the ipv4 attribute present
// (allocated NICs have ipv4 stripped by the dranet driver).
func (a *Allocator) scanAvailableSlots(ctx context.Context) (map[string][]NICSlot, error) {
	slices, err := a.ResourceClient.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list resource slices: %w", err)
	}

	// Build prefix → rail index map
	prefixToRail := make(map[string]int, len(a.Config.NICConfig.Rails))
	for i, rail := range a.Config.NICConfig.Rails {
		prefixToRail[rail.IPv4Prefix] = i
	}

	nodeSlots := make(map[string][]NICSlot)

	for _, slice := range slices.Items {
		if slice.Spec.Driver != "dra.net" {
			continue
		}
		nodeName := ""
		if slice.Spec.NodeName != nil {
			nodeName = *slice.Spec.NodeName
		}
		if nodeName == "" {
			continue
		}

		for _, device := range slice.Spec.Devices {
			ipv4 := getIPv4(device)
			if ipv4 == "" {
				continue // allocated
			}

			if a.Config.NICConfig.RDMARequired {
				rdmaAttr, ok := device.Attributes[resourcev1.QualifiedName("dra.net/rdma")]
				if !ok || rdmaAttr.BoolValue == nil || !*rdmaAttr.BoolValue {
					continue
				}
			}

			// Match to configured rail
			railIdx := -1
			for prefix, idx := range prefixToRail {
				if strings.HasPrefix(ipv4, prefix) {
					railIdx = idx
					break
				}
			}
			if railIdx < 0 {
				continue
			}

			numa := getNUMAZone(device)

			nodeSlots[nodeName] = append(nodeSlots[nodeName], NICSlot{
				NodeName:  nodeName,
				RailIndex: railIdx,
				NUMAZone:  numa,
				IPv4:      ipv4,
			})
		}
	}

	return nodeSlots, nil
}

// selectSlots picks 'count' rail indices from the available slots,
// respecting NUMA constraints if required.
//
// NUMA packing strategy: small requests (< maxPairsPerNUMA) prefer the NUMA
// zone with the fewest free slots (most utilized). This packs small requests
// together on one NUMA zone, keeping the other zone's full capacity available
// for larger requests that need all slots in a single zone.
func selectSlots(slots []NICSlot, count int, numaConstrained bool, maxPairsPerNUMA int) []int {
	if numaConstrained {
		// Group by NUMA zone
		numaSlots := make(map[int][]int)
		for _, s := range slots {
			numaSlots[s.NUMAZone] = append(numaSlots[s.NUMAZone], s.RailIndex)
		}

		// Collect eligible zones (those with enough slots)
		type zoneInfo struct {
			zone  int
			rails []int
		}
		var eligible []zoneInfo
		for zone, rails := range numaSlots {
			if len(rails) >= count {
				eligible = append(eligible, zoneInfo{zone: zone, rails: rails})
			}
		}
		if len(eligible) == 0 {
			return nil
		}

		// For small requests: prefer the zone with fewest free slots
		// (most utilized) to pack small requests together.
		// For large requests (>= maxPairsPerNUMA): prefer the zone with
		// most free slots to maximize success.
		sort.Slice(eligible, func(i, j int) bool {
			if count < maxPairsPerNUMA {
				return len(eligible[i].rails) < len(eligible[j].rails)
			}
			return len(eligible[i].rails) > len(eligible[j].rails)
		})

		return eligible[0].rails[:count]
	}

	// Cross-NUMA: any rails
	var rails []int
	for _, s := range slots {
		rails = append(rails, s.RailIndex)
	}
	if len(rails) >= count {
		return rails[:count]
	}
	return nil
}

// filterNodesByConstraints returns the subset of nodes (that have available
// slots) satisfying the pod's nodeSelector, nodeAffinity, podAntiAffinity,
// and podAffinity constraints.
func (a *Allocator) filterNodesByConstraints(ctx context.Context, pod *corev1.Pod, namespace string, nodeSlots map[string][]NICSlot) ([]string, error) {
	// Fetch node objects once
	allNodes, err := a.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	nodeMap := make(map[string]*corev1.Node, len(allNodes.Items))
	for i := range allNodes.Items {
		nodeMap[allNodes.Items[i].Name] = &allNodes.Items[i]
	}

	// Start with nodes that have available slots
	var candidates []string
	for nodeName := range nodeSlots {
		node, ok := nodeMap[nodeName]
		if !ok {
			continue
		}

		if !matchesNodeSelector(pod, node) {
			klog.V(3).InfoS("Node excluded by nodeSelector", "node", nodeName)
			continue
		}
		if !matchesNodeAffinity(pod, node) {
			klog.V(3).InfoS("Node excluded by nodeAffinity", "node", nodeName)
			continue
		}

		candidates = append(candidates, nodeName)
	}

	// Pod anti-affinity
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil {
		candidates, err = a.filterByPodAntiAffinity(ctx, pod, namespace, candidates, nodeMap)
		if err != nil {
			return nil, err
		}
	}

	// Pod affinity
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAffinity != nil {
		candidates, err = a.filterByPodAffinity(ctx, pod, namespace, candidates, nodeMap)
		if err != nil {
			return nil, err
		}
	}

	return candidates, nil
}

// --- Node-level constraint helpers ---

func matchesNodeSelector(pod *corev1.Pod, node *corev1.Node) bool {
	if pod.Spec.NodeSelector == nil {
		return true
	}
	for key, value := range pod.Spec.NodeSelector {
		if nodeVal, ok := node.Labels[key]; !ok || nodeVal != value {
			return false
		}
	}
	return true
}

func matchesNodeAffinity(pod *corev1.Pod, node *corev1.Node) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return true
	}
	req := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		return true
	}
	// Must match at least one NodeSelectorTerm
	for _, term := range req.NodeSelectorTerms {
		if matchesNodeSelectorTerm(node, term) {
			return true
		}
	}
	return false
}

func matchesNodeSelectorTerm(node *corev1.Node, term corev1.NodeSelectorTerm) bool {
	for _, expr := range term.MatchExpressions {
		if !matchesNodeSelectorRequirement(node.Labels, expr) {
			return false
		}
	}
	for _, field := range term.MatchFields {
		if !matchesFieldRequirement(node, field) {
			return false
		}
	}
	return true
}

func matchesNodeSelectorRequirement(labels map[string]string, req corev1.NodeSelectorRequirement) bool {
	val, exists := labels[req.Key]
	switch req.Operator {
	case corev1.NodeSelectorOpIn:
		if !exists {
			return false
		}
		for _, v := range req.Values {
			if v == val {
				return true
			}
		}
		return false
	case corev1.NodeSelectorOpNotIn:
		if !exists {
			return true
		}
		for _, v := range req.Values {
			if v == val {
				return false
			}
		}
		return true
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	}
	return true
}

func matchesFieldRequirement(node *corev1.Node, req corev1.NodeSelectorRequirement) bool {
	var val string
	switch req.Key {
	case "metadata.name":
		val = node.Name
	default:
		return true // unknown field — don't exclude
	}

	switch req.Operator {
	case corev1.NodeSelectorOpIn:
		for _, v := range req.Values {
			if v == val {
				return true
			}
		}
		return false
	case corev1.NodeSelectorOpNotIn:
		for _, v := range req.Values {
			if v == val {
				return false
			}
		}
		return true
	}
	return true
}

// --- Pod-level constraint helpers ---

// filterByPodAntiAffinity excludes candidate nodes where pods matching
// the anti-affinity label selector exist (running, pending, or scheduled).
// We intentionally do NOT filter by status.phase=Running because the webhook
// pins pods to nodes via nodeAffinity before they start running — those
// pending pods must also be considered for anti-affinity.
func (a *Allocator) filterByPodAntiAffinity(ctx context.Context, pod *corev1.Pod, namespace string, candidates []string, nodeMap map[string]*corev1.Node) ([]string, error) {
	for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == "" {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid anti-affinity label selector: %w", err)
		}

		// Determine namespaces to check
		namespaces := term.Namespaces
		if len(namespaces) == 0 {
			namespaces = []string{namespace}
		}

		// Find topology values where matching pods exist.
		// Check all pods (not just Running) because the webhook pins
		// pods via nodeAffinity before they start.
		excludedTopologyValues := make(map[string]bool)
		for _, ns := range namespaces {
			pods, err := a.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			if err != nil {
				klog.ErrorS(err, "Failed to list pods for anti-affinity check", "namespace", ns)
				continue
			}

			for _, ep := range pods.Items {
				// Skip completed/failed pods
				if ep.Status.Phase == corev1.PodSucceeded || ep.Status.Phase == corev1.PodFailed {
					continue
				}

				nodeName := podNodeName(&ep)
				if nodeName == "" {
					continue
				}
				node, ok := nodeMap[nodeName]
				if !ok {
					continue
				}
				if val, ok := node.Labels[term.TopologyKey]; ok {
					excludedTopologyValues[val] = true
				}
			}
		}

		// Filter out candidates whose topology value is excluded
		var filtered []string
		for _, name := range candidates {
			node, ok := nodeMap[name]
			if !ok {
				continue
			}
			nodeVal, ok := node.Labels[term.TopologyKey]
			if !ok || !excludedTopologyValues[nodeVal] {
				filtered = append(filtered, name)
			} else {
				klog.V(3).InfoS("Node excluded by podAntiAffinity",
					"node", name, "topologyKey", term.TopologyKey, "value", nodeVal)
			}
		}
		candidates = filtered
	}

	return candidates, nil
}

// filterByPodAffinity keeps only candidate nodes where pods matching
// the affinity label selector exist (required co-location).
func (a *Allocator) filterByPodAffinity(ctx context.Context, pod *corev1.Pod, namespace string, candidates []string, nodeMap map[string]*corev1.Node) ([]string, error) {
	for _, term := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == "" {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid affinity label selector: %w", err)
		}

		namespaces := term.Namespaces
		if len(namespaces) == 0 {
			namespaces = []string{namespace}
		}

		// Find topology values where matching pods exist
		requiredTopologyValues := make(map[string]bool)
		for _, ns := range namespaces {
			pods, err := a.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			if err != nil {
				klog.ErrorS(err, "Failed to list pods for affinity check", "namespace", ns)
				continue
			}

			for _, ep := range pods.Items {
				if ep.Status.Phase == corev1.PodSucceeded || ep.Status.Phase == corev1.PodFailed {
					continue
				}

				nodeName := podNodeName(&ep)
				if nodeName == "" {
					continue
				}
				node, ok := nodeMap[nodeName]
				if !ok {
					continue
				}
				if val, ok := node.Labels[term.TopologyKey]; ok {
					requiredTopologyValues[val] = true
				}
			}
		}

		// Keep only candidates whose topology value is in the required set
		var filtered []string
		for _, name := range candidates {
			node, ok := nodeMap[name]
			if !ok {
				continue
			}
			nodeVal, ok := node.Labels[term.TopologyKey]
			if ok && requiredTopologyValues[nodeVal] {
				filtered = append(filtered, name)
			} else {
				klog.V(3).InfoS("Node excluded by podAffinity (no matching co-located pod)",
					"node", name, "topologyKey", term.TopologyKey)
			}
		}
		candidates = filtered
	}

	return candidates, nil
}

// podNodeName returns the node a pod is assigned to. It checks spec.nodeName
// first, then falls back to extracting the node from a webhook-injected
// nodeAffinity (matchFields on metadata.name). This catches pods that the
// webhook has pinned but that haven't been scheduled yet.
func podNodeName(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	// Check for webhook-injected nodeAffinity
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		req := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if req != nil {
			for _, term := range req.NodeSelectorTerms {
				for _, field := range term.MatchFields {
					if field.Key == "metadata.name" && field.Operator == corev1.NodeSelectorOpIn && len(field.Values) == 1 {
						return field.Values[0]
					}
				}
			}
		}
	}
	return ""
}
