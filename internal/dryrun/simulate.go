package dryrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

// SimulateRequest holds input for a dry-run simulation.
type SimulateRequest struct {
	Config    webhook.Config
	State     *ClusterState
	PodSpec   *corev1.Pod // if nil, a default pod is synthesized from Count
	Count     int
	Namespace string
	CrossNUMA bool
}

// SimulateResult holds the output of a dry-run simulation.
type SimulateResult struct {
	NodeName      string
	PatchOps      []PatchOp
	Templates     []TemplateInfo
	RawPatch      json.RawMessage
	Error         string
	ClusterStats  ClusterStats
}

// PatchOp is a simplified JSON Patch operation for display.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value,omitempty"`
}

// TemplateInfo describes a ResourceClaimTemplate that would be created.
type TemplateInfo struct {
	Name       string
	Requests   []RequestInfo
	HasPCIeConstraint bool
}

// RequestInfo describes a single device request in a claim template.
type RequestInfo struct {
	Name            string
	DeviceClassName string
	Selectors       []string
}

// ClusterStats summarizes the captured cluster topology.
type ClusterStats struct {
	NodeCount          int
	ResourceSliceCount int
	NICDevices         int
	GPUDevices         int
}

// Simulate runs the full mutation pipeline against captured cluster state.
func Simulate(ctx context.Context, req SimulateRequest) *SimulateResult {
	result := &SimulateResult{}

	// Build cluster stats
	result.ClusterStats = computeStats(req.State, req.Config)

	// Build fake clientset from captured state
	objects := stateToRuntimeObjects(req.State)
	client := fake.NewSimpleClientset(objects...)

	// Build the pod
	pod := req.PodSpec
	if pod == nil {
		pod = defaultPod(req.Count, req.Namespace, req.CrossNUMA)
	}

	ns := req.Namespace
	if ns == "" {
		ns = "default"
	}

	// Create allocator and mutator
	allocator := webhook.NewAllocator(client.ResourceV1(), client, req.Config)
	mutator := &webhook.Mutator{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		Config:         req.Config,
		Allocator:      allocator,
	}

	// Run mutation
	patch, err := mutator.Mutate(ctx, pod, ns)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if patch == nil {
		result.Error = "no mutation needed (pod has no gpu-nic-pair request or already mutated)"
		return result
	}

	result.RawPatch = patch

	// Parse patch ops for display
	var ops []struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value,omitempty"`
	}
	if err := json.Unmarshal(patch, &ops); err == nil {
		for _, op := range ops {
			po := PatchOp{Op: op.Op, Path: op.Path}
			if op.Value != nil {
				b, _ := json.Marshal(op.Value)
				po.Value = string(b)
			}
			result.PatchOps = append(result.PatchOps, po)

			// Extract node name from affinity patch
			if op.Op == "add" && op.Path == "/spec/affinity" {
				b, _ := json.Marshal(op.Value)
				result.NodeName = extractNodeFromAffinity(string(b))
			}
		}
	}

	// List created templates
	templates, err := client.ResourceV1().ResourceClaimTemplates(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, tmpl := range templates.Items {
			ti := TemplateInfo{Name: tmpl.Name}
			for _, req := range tmpl.Spec.Spec.Devices.Requests {
				ri := RequestInfo{Name: req.Name}
				if req.Exactly != nil {
					ri.DeviceClassName = req.Exactly.DeviceClassName
					for _, sel := range req.Exactly.Selectors {
						if sel.CEL != nil {
							ri.Selectors = append(ri.Selectors, sel.CEL.Expression)
						}
					}
				}
				ti.Requests = append(ti.Requests, ri)
			}
			for _, c := range tmpl.Spec.Spec.Devices.Constraints {
				if c.MatchAttribute != nil {
					ti.HasPCIeConstraint = true
					break
				}
			}
			result.Templates = append(result.Templates, ti)
		}
	}

	return result
}

// PrintResult formats the simulation result to the writer.
//
//nolint:errcheck // report output to io.Writer; errors not actionable
func PrintResult(w io.Writer, result *SimulateResult, cfg webhook.Config) {
	fmt.Fprintln(w, "=== DRA Admission Webhook Dry Run ===")
	fmt.Fprintln(w)

	// Config summary
	mode := "auto (MatchAttribute)"
	if cfg.IsExplicitMode() {
		keys := cfg.DeviceSelectorKeys()
		mode = fmt.Sprintf("explicit (%d device selectors: %s)", len(keys), strings.Join(keys, ", "))
	}
	fmt.Fprintf(w, "Mode:           %s\n", mode)
	fmt.Fprintf(w, "Max pairs/NUMA: %d\n", cfg.MaxPairsPerNUMA)
	fmt.Fprintf(w, "Max pairs/node: %d\n", cfg.MaxPairsPerNode)
	fmt.Fprintln(w)

	// Cluster stats
	fmt.Fprintf(w, "Cluster:  %d nodes, %d resource slices\n",
		result.ClusterStats.NodeCount, result.ClusterStats.ResourceSliceCount)
	fmt.Fprintf(w, "Devices:  %d NICs, %d GPUs detected\n",
		result.ClusterStats.NICDevices, result.ClusterStats.GPUDevices)

	if cfg.IsExplicitMode() && cfg.PairingConfig != nil {
		fmt.Fprintf(w, "Pools:    ")
		for i, pool := range cfg.PairingConfig.NodePools {
			if i > 0 {
				fmt.Fprintf(w, ", ")
			}
			fmt.Fprintf(w, "%s (%d pairs)", pool.NodePoolLabel, len(pool.Pairs))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)

	if result.Error != "" {
		fmt.Fprintf(w, "FAILED: %s\n", result.Error)
		return
	}

	// Allocation
	fmt.Fprintf(w, "--- Allocation ---\n")
	fmt.Fprintf(w, "Node: %s\n", result.NodeName)
	fmt.Fprintln(w)

	// Templates
	for _, tmpl := range result.Templates {
		fmt.Fprintf(w, "--- Template: %s ---\n", tmpl.Name)
		if tmpl.HasPCIeConstraint {
			fmt.Fprintln(w, "Constraint: MatchAttribute (pcieRoot)")
		} else {
			fmt.Fprintln(w, "Constraint: none (CEL pinning)")
		}
		for _, req := range tmpl.Requests {
			fmt.Fprintf(w, "  Request %q:\n", req.Name)
			fmt.Fprintf(w, "    DeviceClass: %s\n", req.DeviceClassName)
			for _, sel := range req.Selectors {
				fmt.Fprintf(w, "    Selector:    %s\n", sel)
			}
		}
		fmt.Fprintln(w)
	}

	// Patch summary
	fmt.Fprintf(w, "--- Patch Operations (%d) ---\n", len(result.PatchOps))
	for _, op := range result.PatchOps {
		fmt.Fprintf(w, "  %s %s\n", strings.ToUpper(op.Op), op.Path)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "=== PASSED: %d pair(s) allocated on %s ===\n", len(result.Templates), result.NodeName)
}

func stateToRuntimeObjects(state *ClusterState) []runtime.Object {
	var objects []runtime.Object
	for i := range state.ResourceSlices {
		objects = append(objects, &state.ResourceSlices[i])
	}
	for i := range state.Nodes {
		objects = append(objects, &state.Nodes[i])
	}
	return objects
}

func defaultPod(count int, namespace string, crossNUMA bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dryrun-test-pod",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "workload",
					Image: "nvidia/cuda:12.3.0-base-ubuntu22.04",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceName("dra.llm-d.io/gpu-nic-pair"): resource.MustParse(fmt.Sprintf("%d", count)),
						},
					},
				},
			},
		},
	}
	if crossNUMA {
		pod.Annotations = map[string]string{
			"dra.llm-d.io/allow-cross-numa": "true",
		}
	}
	return pod
}

func computeStats(state *ClusterState, cfg webhook.Config) ClusterStats {
	stats := ClusterStats{
		NodeCount:          len(state.Nodes),
		ResourceSliceCount: len(state.ResourceSlices),
	}
	for _, slice := range state.ResourceSlices {
		for range slice.Spec.Devices {
			switch slice.Spec.Driver {
			case "dra.net":
				stats.NICDevices++
			case cfg.GPUDeviceClassName:
				stats.GPUDevices++
			}
		}
	}
	return stats
}

func extractNodeFromAffinity(jsonStr string) string {
	// Look for "values":["node-name"] pattern
	idx := strings.Index(jsonStr, `"values":[`)
	if idx < 0 {
		return "<unknown>"
	}
	start := idx + len(`"values":["`)
	end := strings.Index(jsonStr[start:], `"`)
	if end < 0 {
		return "<unknown>"
	}
	return jsonStr[start : start+end]
}
