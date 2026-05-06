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

func TestLoadConfigFromConfigMap_ExplicitMode(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"config.yaml": `
maxPairsPerNUMA: 4
maxPairsPerNode: 8
gpuDeviceClassName: gpu.nvidia.com
nicDeviceClassName: dranet
nicConfig:
  mtu: 9000
  rdmaRequired: true
  interfacePrefix: net
pairingMode: explicit
pairingConfig:
  deviceSelectors:
    gpu:
      deviceClassName: gpu.nvidia.com
      attributeDomain: resource.kubernetes.io
      attributeName: pciBusID
    nic:
      deviceClassName: dranet
      attributeDomain: dra.net
      attributeName: rdmaDevice
  nodePoolLabelKey: agentpool
  nodePools:
    - nodePoolLabel: gpu-h100
      pairs:
        - devices:
            gpu: "0008:06:00.0"
            nic: mlx5_0
          rail: 0
        - devices:
            gpu: "0008:07:00.0"
            nic: mlx5_1
          rail: 1
`,
		},
	}

	client := fake.NewSimpleClientset(cm)
	cfg, err := LoadConfigFromConfigMap(context.Background(), client, "test-ns", "test-config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.IsExplicitMode() {
		t.Error("expected explicit mode")
	}
	if cfg.PairingConfig == nil {
		t.Fatal("expected pairingConfig to be set")
	}
	if len(cfg.PairingConfig.DeviceSelectors) != 2 {
		t.Errorf("expected 2 device selectors, got %d", len(cfg.PairingConfig.DeviceSelectors))
	}
	if cfg.PairingConfig.NodePoolLabelKey != "agentpool" {
		t.Errorf("nodePoolLabelKey = %q, want agentpool", cfg.PairingConfig.NodePoolLabelKey)
	}
	if len(cfg.PairingConfig.NodePools) != 1 {
		t.Fatalf("expected 1 node pool, got %d", len(cfg.PairingConfig.NodePools))
	}
	pool := cfg.PairingConfig.NodePools[0]
	if len(pool.Pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pool.Pairs))
	}
	if pool.Pairs[0].Devices["gpu"] != "0008:06:00.0" {
		t.Errorf("pair 0 gpu = %q, want 0008:06:00.0", pool.Pairs[0].Devices["gpu"])
	}
	if pool.Pairs[0].Devices["nic"] != "mlx5_0" {
		t.Errorf("pair 0 nic = %q, want mlx5_0", pool.Pairs[0].Devices["nic"])
	}
}

func TestLoadConfigFromConfigMap_DefaultsToAutoMode(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"config.yaml": `
maxPairsPerNUMA: 4
maxPairsPerNode: 8
gpuDeviceClassName: gpu.nvidia.com
nicDeviceClassName: dranet
nicConfig:
  mtu: 9000
`,
		},
	}

	client := fake.NewSimpleClientset(cm)
	cfg, err := LoadConfigFromConfigMap(context.Background(), client, "test-ns", "test-config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.IsExplicitMode() {
		t.Error("expected auto mode by default")
	}
}

func TestValidatePairingConfig_MissingConfig(t *testing.T) {
	cfg := Config{PairingMode: PairingModeExplicit}
	if err := ValidatePairingConfig(cfg); err == nil {
		t.Error("expected error for explicit mode without pairingConfig")
	}
}

func TestValidatePairingConfig_MismatchedDeviceKeys(t *testing.T) {
	cfg := Config{
		MaxPairsPerNode: 8,
		PairingMode:     PairingModeExplicit,
		PairingConfig: &PairingConfig{
			DeviceSelectors: map[string]DeviceSelectorConfig{
				"gpu": {DeviceClassName: "gpu.nvidia.com", AttributeDomain: "resource.kubernetes.io", AttributeName: "pciBusID"},
				"nic": {DeviceClassName: "dranet", AttributeDomain: "dra.net", AttributeName: "rdmaDevice"},
			},
			NodePoolLabelKey: "agentpool",
			NodePools: []NodePoolMapping{
				{
					NodePoolLabel: "pool-a",
					Pairs: []ExplicitPairMapping{
						{Devices: map[string]string{"gpu": "0000:01:00.0"}}, // missing "nic"
					},
				},
			},
		},
	}
	if err := ValidatePairingConfig(cfg); err == nil {
		t.Error("expected error for mismatched device keys")
	}
}

func TestValidatePairingConfig_EmptyDeviceSelectors(t *testing.T) {
	cfg := Config{
		PairingMode: PairingModeExplicit,
		PairingConfig: &PairingConfig{
			DeviceSelectors:  map[string]DeviceSelectorConfig{},
			NodePoolLabelKey: "agentpool",
			NodePools:        []NodePoolMapping{{NodePoolLabel: "a", Pairs: []ExplicitPairMapping{{}}}},
		},
	}
	if err := ValidatePairingConfig(cfg); err == nil {
		t.Error("expected error for empty device selectors")
	}
}

func TestGetNodePoolMapping(t *testing.T) {
	cfg := Config{
		PairingConfig: &PairingConfig{
			NodePoolLabelKey: "agentpool",
			NodePools: []NodePoolMapping{
				{NodePoolLabel: "pool-a"},
				{NodePoolLabel: "pool-b"},
			},
		},
	}

	m, err := cfg.GetNodePoolMapping(map[string]string{"agentpool": "pool-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.NodePoolLabel != "pool-b" {
		t.Errorf("got pool %q, want pool-b", m.NodePoolLabel)
	}

	_, err = cfg.GetNodePoolMapping(map[string]string{"agentpool": "nonexistent"})
	if err == nil {
		t.Error("expected error for unknown pool label")
	}

	_, err = cfg.GetNodePoolMapping(map[string]string{"other": "value"})
	if err == nil {
		t.Error("expected error for missing label key")
	}
}

func TestDeviceSelectorKeys(t *testing.T) {
	cfg := Config{
		PairingConfig: &PairingConfig{
			DeviceSelectors: map[string]DeviceSelectorConfig{
				"nic":  {},
				"gpu":  {},
				"fpga": {},
			},
		},
	}
	keys := cfg.DeviceSelectorKeys()
	if len(keys) != 3 || keys[0] != "fpga" || keys[1] != "gpu" || keys[2] != "nic" {
		t.Errorf("expected sorted keys [fpga gpu nic], got %v", keys)
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
