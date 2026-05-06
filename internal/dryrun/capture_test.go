package dryrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCaptureClusterState(t *testing.T) {
	nodeName := "gpu-node-1"
	rdma := true
	ipv4 := "10.0.100.1"
	numa := int64(0)

	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1-nics"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "dra.net",
			NodeName: &nodeName,
			Pool:     resourcev1.ResourcePool{Name: "nic-pool"},
			Devices: []resourcev1.Device{
				{
					Name: "nic-0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"dra.net/ipv4":     {StringValue: &ipv4},
						"dra.net/rdma":     {BoolValue: &rdma},
						"dra.net/numaNode": {IntValue: &numa},
					},
				},
			},
		},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{"gpu-type": "h100"},
		},
	}

	client := fake.NewSimpleClientset(slice, node)
	state, err := CaptureClusterState(context.Background(), client, "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if state.ClusterName != "test-cluster" {
		t.Errorf("cluster name = %q, want test-cluster", state.ClusterName)
	}
	if len(state.ResourceSlices) != 1 {
		t.Errorf("resource slices = %d, want 1", len(state.ResourceSlices))
	}
	if len(state.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(state.Nodes))
	}
	if state.CapturedAt.IsZero() {
		t.Error("capturedAt should not be zero")
	}
}

func TestSaveAndLoadClusterState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	nodeName := "node-1"
	original := &ClusterState{
		ClusterName: "roundtrip-test",
		ResourceSlices: []resourcev1.ResourceSlice{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-1"},
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   "dra.net",
					NodeName: &nodeName,
					Pool:     resourcev1.ResourcePool{Name: "pool"},
				},
			},
		},
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		},
	}

	if err := original.SaveToFile(path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("file is empty")
	}

	loaded, err := LoadClusterState(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if loaded.ClusterName != "roundtrip-test" {
		t.Errorf("cluster name = %q, want roundtrip-test", loaded.ClusterName)
	}
	if len(loaded.ResourceSlices) != 1 {
		t.Errorf("slices = %d, want 1", len(loaded.ResourceSlices))
	}
	if len(loaded.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(loaded.Nodes))
	}
}

func TestLoadClusterState_FileNotFound(t *testing.T) {
	_, err := LoadClusterState("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadClusterState_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(path, []byte("not json"), 0644)

	_, err := LoadClusterState(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
