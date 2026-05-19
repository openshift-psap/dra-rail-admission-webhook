package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
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
	// Driver is the ResourceSlice driver name (e.g., "dra.net", "gpu.nvidia.com").
	// Used to match ResourceSlice.Spec.Driver during availability scanning.
	// If empty, DeviceClassName is used as fallback (works when they match).
	Driver          string `yaml:"driver,omitempty"`
	AttributeDomain string `yaml:"attributeDomain"`
	AttributeName   string `yaml:"attributeName"`
}

// DriverName returns the driver name for ResourceSlice matching.
func (s DeviceSelectorConfig) DriverName() string {
	if s.Driver != "" {
		return s.Driver
	}
	return s.DeviceClassName
}

// ExplicitPairMapping defines one set of co-located devices.
// Devices keys must match the keys in PairingConfig.DeviceSelectors.
type ExplicitPairMapping struct {
	Devices map[string]string `yaml:"devices"`
	Rail    int               `yaml:"rail"`
	NUMA    int               `yaml:"numa"`
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

// IBRailEntry defines a GPU-NIC pair for InfiniBand rail configuration.
// List index is the rail index.
type IBRailEntry struct {
	GPU string `yaml:"gpu"` // GPU PCIe bus ID (e.g., "0001:00:00.0")
	NIC string `yaml:"nic"` // NIC PCI address (e.g., "0101:00:00.0")
}

// DeviceFilter defines criteria for matching NIC devices. A device matches
// the filter if it satisfies ANY of the configured criteria (OR logic).
// A nil filter matches everything. An empty filter (all fields zero) matches nothing.
type DeviceFilter struct {
	Encapsulations     []string `yaml:"encapsulations,omitempty"`
	PCIAddressPrefixes []string `yaml:"pciAddressPrefixes,omitempty"`
	IfNamePrefixes     []string `yaml:"ifNamePrefixes,omitempty"`
	PCIeRoots          []string `yaml:"pcieRoots,omitempty"`
	SRIOV              *bool    `yaml:"sriov,omitempty"`
}

// Matches returns true if the device matches any configured criterion.
// Nil filter returns true (matches everything). Empty filter returns false.
func (f *DeviceFilter) Matches(device resourcev1.Device) bool {
	if f == nil {
		return true
	}

	for _, enc := range f.Encapsulations {
		if attr, ok := device.Attributes[resourcev1.QualifiedName(EncapsulationAttribute)]; ok && attr.StringValue != nil && *attr.StringValue == enc {
			return true
		}
	}

	for _, prefix := range f.PCIAddressPrefixes {
		if attr, ok := device.Attributes[resourcev1.QualifiedName(NICPCIAddressAttribute)]; ok && attr.StringValue != nil && strings.HasPrefix(*attr.StringValue, prefix) {
			return true
		}
	}

	for _, prefix := range f.IfNamePrefixes {
		if attr, ok := device.Attributes[resourcev1.QualifiedName(NICIfNameAttribute)]; ok && attr.StringValue != nil && strings.HasPrefix(*attr.StringValue, prefix) {
			return true
		}
	}

	for _, root := range f.PCIeRoots {
		if attr, ok := device.Attributes[resourcev1.QualifiedName(PCIeRootAttribute)]; ok && attr.StringValue != nil && *attr.StringValue == root {
			return true
		}
	}

	if f.SRIOV != nil {
		if attr, ok := device.Attributes[resourcev1.QualifiedName(NICSRIOVAttribute)]; ok && attr.BoolValue != nil && *attr.BoolValue == *f.SRIOV {
			return true
		}
	}

	return false
}

// NICConfig holds network interface configuration.
type NICConfig struct {
	MTU             int          `yaml:"mtu"`
	RDMARequired    bool         `yaml:"rdmaRequired"`
	InterfacePrefix string       `yaml:"interfacePrefix"`
	Routes          []Route      `yaml:"routes,omitempty"`
	SourceSubnet    string       `yaml:"sourceSubnet,omitempty"`
	StartingTableID int          `yaml:"startingTableId,omitempty"`
	Rails           []RailConfig  `yaml:"rails,omitempty"`
	CrossRailCIDR   string        `yaml:"crossRailCIDR,omitempty"`
	IBRails         []IBRailEntry `yaml:"ibRails,omitempty"`
	IncludeDevices    *DeviceFilter     `yaml:"includeDevices,omitempty"`
	ExcludeDevices    *DeviceFilter     `yaml:"excludeDevices,omitempty"`
	GatewayResolution *GatewayResolution `yaml:"gatewayResolution,omitempty"`
}

// GatewayResolution configures how per-claim gateways are determined.
// "static" (default) uses RailConfig.Gateway. "lookup" resolves via
// a NodeName + RailIndex table, erroring if the entry is missing.
type GatewayResolution struct {
	Mode        string                       `yaml:"mode"`
	LookupTable map[string]map[string]string `yaml:"lookupTable,omitempty"`
}

// ResolveGateway returns the gateway IP for a given node, rail, and NIC.
// Static mode (nil or "static") returns railGateway unchanged.
// Lookup mode returns the table entry or an error if missing.
func (nc *NICConfig) ResolveGateway(nodeName string, railIndex int, railGateway string) (string, error) {
	if nc.GatewayResolution == nil || nc.GatewayResolution.Mode == "" || nc.GatewayResolution.Mode == "static" {
		return railGateway, nil
	}
	if nc.GatewayResolution.Mode == "lookup" {
		nodeMap, ok := nc.GatewayResolution.LookupTable[nodeName]
		if !ok {
			return "", fmt.Errorf("gateway lookup: no entry for node %q", nodeName)
		}
		key := fmt.Sprintf("%d", railIndex)
		gw, ok := nodeMap[key]
		if !ok {
			return "", fmt.Errorf("gateway lookup: no entry for node %q rail %d", nodeName, railIndex)
		}
		return gw, nil
	}
	return "", fmt.Errorf("unknown gateway resolution mode %q", nc.GatewayResolution.Mode)
}

// IsDeviceAllowed checks whether a device passes the include/exclude filters.
// Include layer (nil = all pass, configured = must match).
// Exclude layer (nil = nothing excluded, configured = matching devices dropped).
func (nc *NICConfig) IsDeviceAllowed(device resourcev1.Device) bool {
	if nc.IncludeDevices != nil && !nc.IncludeDevices.Matches(device) {
		return false
	}
	if nc.ExcludeDevices != nil && nc.ExcludeDevices.Matches(device) {
		return false
	}
	return true
}

// ExtendedResourceInterception defines an extended resource to intercept and
// convert to DRA ResourceClaims. When listed in Config.InterceptExtendedResources,
// the webhook strips the named resource from pod containers and creates DRA
// ResourceClaims using the specified DeviceClass instead.
type ExtendedResourceInterception struct {
	ResourceName    string `yaml:"resourceName"`
	DeviceClassName string `yaml:"deviceClassName"`
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

	// TransportMode selects the network transport: "auto" (default) detects
	// from ResourceSlice encapsulation attributes at startup, "ethernet" uses
	// IPv4-based rail selection, "infiniband" uses PCIe address-based pairing.
	TransportMode string `yaml:"transportMode,omitempty"`

	// InterceptExtendedResources lists Kubernetes extended resources that should
	// be intercepted and converted to DRA ResourceClaims. Each entry maps an
	// extended resource name (e.g., "nvidia.com/gpu") to a DRA DeviceClass name
	// (e.g., "gpu.nvidia.com"). When empty (default), no extended resources are
	// intercepted. This is opt-in.
	InterceptExtendedResources []ExtendedResourceInterception `yaml:"interceptExtendedResources,omitempty"`
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

// IsInfiniBand returns true when the resolved transport mode is InfiniBand.
func (c Config) IsInfiniBand() bool {
	return c.TransportMode == "infiniband"
}

// ResolveTransportMode detects the network transport from ResourceSlices.
// Scans dra.net devices for the encapsulation attribute. If any device
// reports "infiniband", returns "infiniband"; otherwise "ethernet".
func ResolveTransportMode(ctx context.Context, client kubernetes.Interface) string {
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to list ResourceSlices for transport detection, defaulting to ethernet")
		return "ethernet"
	}
	for _, s := range slices.Items {
		if s.Spec.Driver != "dra.net" {
			continue
		}
		for _, d := range s.Spec.Devices {
			attr, ok := d.Attributes[resourcev1.QualifiedName(EncapsulationAttribute)]
			if ok && attr.StringValue != nil && *attr.StringValue == "infiniband" {
				return "infiniband"
			}
		}
	}
	return "ethernet"
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
	if cfg.PairingMode != "" && cfg.PairingMode != PairingModeAuto && cfg.PairingMode != PairingModeExplicit {
		return fmt.Errorf("unknown pairingMode %q, must be %q or %q", cfg.PairingMode, PairingModeAuto, PairingModeExplicit)
	}
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

// InterceptedResourceMap returns a map from extended resource name to device
// class name for quick lookup. Returns nil if no interception is configured.
func (c Config) InterceptedResourceMap() map[string]string {
	if len(c.InterceptExtendedResources) == 0 {
		return nil
	}
	m := make(map[string]string, len(c.InterceptExtendedResources))
	for _, r := range c.InterceptExtendedResources {
		m[r.ResourceName] = r.DeviceClassName
	}
	return m
}

// ValidateInterceptConfig validates the extended resource interception config.
func ValidateInterceptConfig(cfg Config) error {
	seen := make(map[string]bool, len(cfg.InterceptExtendedResources))
	for i, r := range cfg.InterceptExtendedResources {
		if r.ResourceName == "" {
			return fmt.Errorf("interceptExtendedResources[%d]: resourceName must not be empty", i)
		}
		if r.DeviceClassName == "" {
			return fmt.Errorf("interceptExtendedResources[%d]: deviceClassName must not be empty", i)
		}
		if r.ResourceName == ResourceGPUNICPair {
			return fmt.Errorf("interceptExtendedResources[%d]: cannot intercept %q (handled natively)", i, ResourceGPUNICPair)
		}
		if seen[r.ResourceName] {
			return fmt.Errorf("interceptExtendedResources[%d]: duplicate resourceName %q", i, r.ResourceName)
		}
		seen[r.ResourceName] = true
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
	if err := ValidateInterceptConfig(cfg); err != nil {
		return Config{}, fmt.Errorf("invalid intercept config: %w", err)
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
