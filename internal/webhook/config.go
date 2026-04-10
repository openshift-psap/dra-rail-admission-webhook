package webhook

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"gopkg.in/yaml.v3"
)

// Route represents a network route in the NIC configuration.
type Route struct {
	Destination string `yaml:"destination" json:"destination"`
	Gateway     string `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	Table       int    `yaml:"table,omitempty" json:"table,omitempty"`
}

// Rule represents a source-based routing rule.
type Rule struct {
	Source   string `yaml:"source" json:"source"`
	Table    int    `yaml:"table" json:"table"`
	Priority int    `yaml:"priority" json:"priority"`
}

// NICConfig holds network interface configuration.
type NICConfig struct {
	MTU             int     `yaml:"mtu"`
	RDMARequired    bool    `yaml:"rdmaRequired"`
	InterfacePrefix string  `yaml:"interfacePrefix"`
	Routes          []Route `yaml:"routes,omitempty"`
	SourceSubnet    string  `yaml:"sourceSubnet,omitempty"`
	StartingTableID int     `yaml:"startingTableId,omitempty"`
}

// Config holds the webhook configuration loaded from a ConfigMap.
type Config struct {
	MaxPairsPerNUMA    int       `yaml:"maxPairsPerNUMA"`
	MaxPairsPerNode    int       `yaml:"maxPairsPerNode"`
	GPUDeviceClassName string    `yaml:"gpuDeviceClassName"`
	NICDeviceClassName string    `yaml:"nicDeviceClassName"`
	NICConfig          NICConfig `yaml:"nicConfig"`

	// PreflightCheck enables an experimental pre-flight availability check.
	// When enabled, the webhook reads ResourceSlices to verify that at least
	// one node has enough available GPU-NIC pairs before admitting the pod.
	// This avoids pods stuck in Pending but adds latency to admission.
	PreflightCheck bool `yaml:"preflightCheck"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
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
		},
	}
}

// LoadConfigFromConfigMap reads and parses the webhook config from a Kubernetes ConfigMap.
func LoadConfigFromConfigMap(ctx context.Context, client kubernetes.Interface, namespace, name string) (Config, error) {
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Config{}, fmt.Errorf("failed to get configmap %s/%s: %w", namespace, name, err)
	}

	data, ok := cm.Data["config.yaml"]
	if !ok {
		return Config{}, fmt.Errorf("configmap %s/%s missing 'config.yaml' key", namespace, name)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config.yaml: %w", err)
	}

	return cfg, nil
}
