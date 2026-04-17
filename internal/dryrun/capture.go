package dryrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterState holds captured cluster topology for offline simulation.
type ClusterState struct {
	CapturedAt     time.Time                  `json:"capturedAt"`
	ClusterName    string                     `json:"clusterName,omitempty"`
	ResourceSlices []resourcev1.ResourceSlice `json:"resourceSlices"`
	Nodes          []corev1.Node              `json:"nodes"`
}

// CaptureClusterState reads ResourceSlices and Nodes from a live cluster.
func CaptureClusterState(ctx context.Context, client kubernetes.Interface, clusterName string) (*ClusterState, error) {
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list resource slices: %w", err)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Strip managed fields and status to keep the capture compact
	for i := range slices.Items {
		slices.Items[i].ManagedFields = nil
	}
	for i := range nodes.Items {
		nodes.Items[i].ManagedFields = nil
		nodes.Items[i].Status = corev1.NodeStatus{}
	}

	return &ClusterState{
		CapturedAt:     time.Now().UTC(),
		ClusterName:    clusterName,
		ResourceSlices: slices.Items,
		Nodes:          nodes.Items,
	}, nil
}

// SaveToFile writes the cluster state to a JSON file.
func (cs *ClusterState) SaveToFile(path string) error {
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cluster state: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

// LoadClusterState reads a ClusterState from a JSON file.
func LoadClusterState(path string) (*ClusterState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var cs ClusterState
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("failed to parse cluster state: %w", err)
	}
	return &cs, nil
}
