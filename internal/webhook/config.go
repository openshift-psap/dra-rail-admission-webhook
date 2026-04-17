package webhook

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"gopkg.in/yaml.v3"
)

// PairingMode controls how GPU-NIC pairing constraints are expressed.
type PairingMode string

const (
	PairingModeAuto     PairingMode = "auto"
	PairingModeExplicit PairingMode = "explicit"
)

// DeviceSelectorConfig defines how to identify a device type in CEL selectors.
type DeviceSelectorConfig struct {
	DeviceClassName string `yaml:"deviceClassName"`
	AttributeDomain string `yaml:"attributeDomain"`
	AttributeName   string `yaml:"attributeName"`
}

// ExplicitPairMapping defines one set of co-located devices.
// Devices keys must match the keys in PairingConfig.DeviceSelectors.
type ExplicitPairMapping struct {
	Devices map[string]string `yaml:"devices"`
	Rail    int               `yaml:"rail"`
}

// NodePoolMapping defines the device topology for a group of similar nodes.
type NodePoolMapping struct {
	NodePoolLabel string                `yaml:"nodePoolLabel"`
	Pairs         []ExplicitPairMapping `yaml:"pairs"`
}

// PairingConfig holds explicit device-to-device pairing configuration.
type PairingConfig struct {
	DeviceSelectors  map[string]DeviceSelectorConfig `yaml:"deviceSelectors"`
	NodePoolLabelKey string                          `yaml:"nodePoolLabelKey"`
	NodePools        []NodePoolMapping               `yaml:"nodePools"`
}

// Route represents a network route in the NIC configuration.
type Route struct {
	Destination string `yaml:"destination" json:"destination"`
	Gateway     string `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	Table       int    `yaml:"table,omitempty" json:"table,omitempty"`
	Scope       int    `yaml:"scope,omitempty" json:"scope,omitempty"`
}

// RailConfig defines the network topology for a single rail.
// Each rail maps to a specific subnet and gateway.
type RailConfig struct {
	Subnet     string `yaml:"subnet" json:"subnet"`         // e.g., "10.0.0.0/16"
	Gateway    string `yaml:"gateway" json:"gateway"`        // e.g., "10.0.0.1"
	IPv4Prefix string `yaml:"ipv4Prefix" json:"ipv4Prefix"` // e.g., "10.0." for CEL selector
}

// Rule represents a source-based routing rule.
type Rule struct {
	Source   string `yaml:"source" json:"source"`
	Table    int    `yaml:"table" json:"table"`
	Priority int    `yaml:"priority" json:"priority"`
}

// NICConfig holds network interface configuration.
type NICConfig struct {
	MTU             int          `yaml:"mtu"`
	RDMARequired    bool         `yaml:"rdmaRequired"`
	InterfacePrefix string       `yaml:"interfacePrefix"`
	Routes          []Route      `yaml:"routes,omitempty"`
	SourceSubnet    string       `yaml:"sourceSubnet,omitempty"`
	StartingTableID int          `yaml:"startingTableId,omitempty"`
	Rails           []RailConfig `yaml:"rails,omitempty"`
}

// Config holds the webhook configuration loaded from a ConfigMap.
type Config struct {
	MaxPairsPerNUMA    int       `yaml:"maxPairsPerNUMA"`
	MaxPairsPerNode    int       `yaml:"maxPairsPerNode"`
	GPUDeviceClassName string    `yaml:"gpuDeviceClassName"`
	NICDeviceClassName string    `yaml:"nicDeviceClassName"`
	NICConfig          NICConfig `yaml:"nicConfig"`

	PreflightCheck bool `yaml:"preflightCheck"`

	// PairingMode selects how GPU-NIC pairing is determined:
	// "auto" (default) uses MatchAttribute on pcieRoot.
	// "explicit" uses admin-defined device-to-device mappings with CEL selectors.
	PairingMode   PairingMode    `yaml:"pairingMode,omitempty"`
	PairingConfig *PairingConfig `yaml:"pairingConfig,omitempty"`

	// DisableNUMAPacking disables the NUMA-aware packing strategy in the
	// allocator. When true, the allocator does not prefer specific NUMA zones.
	DisableNUMAPacking bool `yaml:"disableNUMAPacking,omitempty"`
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

// IsExplicitMode returns true when explicit device-to-device pairing is configured.
func (c Config) IsExplicitMode() bool {
	return c.PairingMode == PairingModeExplicit
}

// GetNodePoolMapping finds the NodePoolMapping for a node based on its labels.
func (c Config) GetNodePoolMapping(nodeLabels map[string]string) (*NodePoolMapping, error) {
	if c.PairingConfig == nil {
		return nil, fmt.Errorf("pairingConfig is nil")
	}
	labelValue, ok := nodeLabels[c.PairingConfig.NodePoolLabelKey]
	if !ok {
		return nil, fmt.Errorf("node missing label %q", c.PairingConfig.NodePoolLabelKey)
	}
	for i := range c.PairingConfig.NodePools {
		if c.PairingConfig.NodePools[i].NodePoolLabel == labelValue {
			return &c.PairingConfig.NodePools[i], nil
		}
	}
	return nil, fmt.Errorf("no node pool mapping for label %s=%s", c.PairingConfig.NodePoolLabelKey, labelValue)
}

// DeviceSelectorKeys returns sorted device selector keys for deterministic iteration.
func (c Config) DeviceSelectorKeys() []string {
	if c.PairingConfig == nil {
		return nil
	}
	keys := make([]string, 0, len(c.PairingConfig.DeviceSelectors))
	for k := range c.PairingConfig.DeviceSelectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidatePairingConfig validates the explicit pairing configuration.
func ValidatePairingConfig(cfg Config) error {
	if !cfg.IsExplicitMode() {
		return nil
	}
	pc := cfg.PairingConfig
	if pc == nil {
		return fmt.Errorf("pairingMode is 'explicit' but pairingConfig is not set")
	}
	if len(pc.DeviceSelectors) == 0 {
		return fmt.Errorf("pairingConfig.deviceSelectors must not be empty")
	}
	for role, sel := range pc.DeviceSelectors {
		if sel.DeviceClassName == "" {
			return fmt.Errorf("deviceSelector %q: deviceClassName is required", role)
		}
		if sel.AttributeDomain == "" || sel.AttributeName == "" {
			return fmt.Errorf("deviceSelector %q: attributeDomain and attributeName are required", role)
		}
	}
	if pc.NodePoolLabelKey == "" {
		return fmt.Errorf("pairingConfig.nodePoolLabelKey must not be empty")
	}
	if len(pc.NodePools) == 0 {
		return fmt.Errorf("pairingConfig.nodePools must have at least one entry")
	}
	selectorKeys := make([]string, 0, len(pc.DeviceSelectors))
	for k := range pc.DeviceSelectors {
		selectorKeys = append(selectorKeys, k)
	}
	sort.Strings(selectorKeys)

	for _, pool := range pc.NodePools {
		if len(pool.Pairs) == 0 {
			return fmt.Errorf("nodePool %q: must have at least one pair", pool.NodePoolLabel)
		}
		if len(pool.Pairs) > cfg.MaxPairsPerNode {
			return fmt.Errorf("nodePool %q: %d pairs exceeds maxPairsPerNode (%d)",
				pool.NodePoolLabel, len(pool.Pairs), cfg.MaxPairsPerNode)
		}
		for i, pair := range pool.Pairs {
			pairKeys := make([]string, 0, len(pair.Devices))
			for k := range pair.Devices {
				pairKeys = append(pairKeys, k)
			}
			sort.Strings(pairKeys)
			if len(pairKeys) != len(selectorKeys) {
				return fmt.Errorf("nodePool %q pair %d: device keys %v don't match selector keys %v",
					pool.NodePoolLabel, i, pairKeys, selectorKeys)
			}
			for j, k := range pairKeys {
				if k != selectorKeys[j] {
					return fmt.Errorf("nodePool %q pair %d: device keys %v don't match selector keys %v",
						pool.NodePoolLabel, i, pairKeys, selectorKeys)
				}
			}
			for role, val := range pair.Devices {
				if val == "" {
					return fmt.Errorf("nodePool %q pair %d: device %q value must not be empty",
						pool.NodePoolLabel, i, role)
				}
			}
		}
	}
	return nil
}

// ParseConfig parses raw YAML bytes into a validated Config.
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config YAML: %w", err)
	}
	if err := ValidatePairingConfig(cfg); err != nil {
		return Config{}, fmt.Errorf("invalid pairing config: %w", err)
	}
	return cfg, nil
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

	return ParseConfig([]byte(data))
}
