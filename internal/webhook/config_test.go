package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLoadConfigFromConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"config.yaml": `
maxPairsPerNUMA: 6
maxPairsPerNode: 12
gpuDeviceClassName: custom-gpu
nicDeviceClassName: custom-nic
nicConfig:
  mtu: 1500
  rdmaRequired: false
  interfacePrefix: eth
  startingTableId: 200
  routes:
  - destination: "10.0.0.0/8"
    gateway: "10.0.0.1"
`,
		},
	}

	client := fake.NewSimpleClientset(cm)
	cfg, err := LoadConfigFromConfigMap(context.Background(), client, "test-ns", "test-config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxPairsPerNUMA != 6 {
		t.Errorf("MaxPairsPerNUMA = %d, want 6", cfg.MaxPairsPerNUMA)
	}
	if cfg.MaxPairsPerNode != 12 {
		t.Errorf("MaxPairsPerNode = %d, want 12", cfg.MaxPairsPerNode)
	}
	if cfg.GPUDeviceClassName != "custom-gpu" {
		t.Errorf("GPUDeviceClassName = %q, want custom-gpu", cfg.GPUDeviceClassName)
	}
	if cfg.NICDeviceClassName != "custom-nic" {
		t.Errorf("NICDeviceClassName = %q, want custom-nic", cfg.NICDeviceClassName)
	}
	if cfg.NICConfig.MTU != 1500 {
		t.Errorf("NICConfig.MTU = %d, want 1500", cfg.NICConfig.MTU)
	}
	if cfg.NICConfig.RDMARequired {
		t.Error("NICConfig.RDMARequired should be false")
	}
	if cfg.NICConfig.InterfacePrefix != "eth" {
		t.Errorf("NICConfig.InterfacePrefix = %q, want eth", cfg.NICConfig.InterfacePrefix)
	}
	if cfg.NICConfig.StartingTableID != 200 {
		t.Errorf("NICConfig.StartingTableID = %d, want 200", cfg.NICConfig.StartingTableID)
	}
	if len(cfg.NICConfig.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(cfg.NICConfig.Routes))
	}
}

func TestLoadConfigFromConfigMap_MissingKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"other-key": "value",
		},
	}

	client := fake.NewSimpleClientset(cm)
	_, err := LoadConfigFromConfigMap(context.Background(), client, "test-ns", "test-config")
	if err == nil {
		t.Error("expected error for missing config.yaml key")
	}
}

func TestLoadConfigFromConfigMap_NotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	_, err := LoadConfigFromConfigMap(context.Background(), client, "test-ns", "nonexistent")
	if err == nil {
		t.Error("expected error for missing configmap")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxPairsPerNUMA != 4 {
		t.Errorf("default MaxPairsPerNUMA = %d, want 4", cfg.MaxPairsPerNUMA)
	}
	if cfg.MaxPairsPerNode != 8 {
		t.Errorf("default MaxPairsPerNode = %d, want 8", cfg.MaxPairsPerNode)
	}
	if cfg.GPUDeviceClassName != "gpu.nvidia.com" {
		t.Errorf("default GPUDeviceClassName = %q, want gpu.nvidia.com", cfg.GPUDeviceClassName)
	}
	if cfg.NICDeviceClassName != "dranet" {
		t.Errorf("default NICDeviceClassName = %q, want dranet", cfg.NICDeviceClassName)
	}
	if cfg.NICConfig.MTU != 9000 {
		t.Errorf("default NICConfig.MTU = %d, want 9000", cfg.NICConfig.MTU)
	}
}
