package webhook

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"k8s.io/klog/v2"
)

type RailNodeState struct {
	Prefix      string            `json:"prefix"`
	Assignments map[string]string `json:"assignments"` // IP -> claimRef
}

type IPState struct {
	Rails map[string]map[string]*RailNodeState `json:"rails"` // railIdx(string) -> nodeName -> state
}

type IPAllocator struct {
	mu       sync.Mutex
	state    IPState
	filePath string
}

// NewIPAllocator creates a new IP allocator, loading state from file if it exists.
func NewIPAllocator(filePath string) (*IPAllocator, error) {
	a := &IPAllocator{
		filePath: filePath,
		state: IPState{
			Rails: make(map[string]map[string]*RailNodeState),
		},
	}

	if _, err := os.Stat(filePath); err == nil {
		if err := a.load(); err != nil {
			klog.Warningf("Failed to load IP allocator state from %s: %v, starting fresh", filePath, err)
		}
	}

	return a, nil
}

// AllocateIP allocates the next available IP from the prefix for the given node and rail.
// Returns IP in CIDR format (e.g., "172.16.1.2/24").
func (a *IPAllocator) AllocateIP(nodeName string, railIndex int, prefix string, claimRef string, reserved []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	railKey := fmt.Sprintf("%d", railIndex)

	if a.state.Rails[railKey] == nil {
		a.state.Rails[railKey] = make(map[string]*RailNodeState)
	}

	nodeState := a.state.Rails[railKey][nodeName]

	// If prefix changed or node doesn't exist, reset state for this rail+node
	if nodeState == nil || nodeState.Prefix != prefix {
		nodeState = &RailNodeState{
			Prefix:      prefix,
			Assignments: make(map[string]string),
		}
		a.state.Rails[railKey][nodeName] = nodeState
	}

	ip, err := nextAvailableIP(prefix, nodeState.Assignments, reserved)
	if err != nil {
		return "", fmt.Errorf("no available IP in prefix %s: %w", prefix, err)
	}

	// Return IP in CIDR format
	_, ipNet, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", fmt.Errorf("invalid prefix %s: %w", prefix, err)
	}
	maskSize, _ := ipNet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", ip, maskSize)

	nodeState.Assignments[ip] = claimRef

	if err := a.save(); err != nil {
		return "", fmt.Errorf("failed to save state: %w", err)
	}

	return ipCIDR, nil
}

// ReleaseIP releases an allocated IP. The ip param can be in CIDR format or plain IP.
func (a *IPAllocator) ReleaseIP(nodeName string, railIndex int, ip string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Strip CIDR suffix if present
	plainIP := ip
	if parsedIP, _, err := net.ParseCIDR(ip); err == nil {
		plainIP = parsedIP.String()
	}

	railKey := fmt.Sprintf("%d", railIndex)

	if a.state.Rails[railKey] == nil {
		return nil
	}

	nodeState := a.state.Rails[railKey][nodeName]
	if nodeState == nil {
		return nil
	}

	if _, exists := nodeState.Assignments[plainIP]; !exists {
		return nil
	}

	delete(nodeState.Assignments, plainIP)

	return a.save()
}

// ReleaseByClaimRef releases all IPs across all rails/nodes matching the claim ref.
// Returns count of IPs freed.
func (a *IPAllocator) ReleaseByClaimRef(claimRef string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	freed := 0

	for _, nodes := range a.state.Rails {
		for _, nodeState := range nodes {
			for ip, ref := range nodeState.Assignments {
				if ref == claimRef {
					delete(nodeState.Assignments, ip)
					freed++
				}
			}
		}
	}

	if freed > 0 {
		if err := a.save(); err != nil {
			klog.Errorf("Failed to save state after releasing %d IPs for claim %s: %v", freed, claimRef, err)
		}
	}

	return freed
}

// ReconcileFromClaims reconciles state against active claims.
// Takes map of claimRef→IP for all currently active claims.
// Removes stale entries (in state but not in active claims).
// Adds missing entries (in active claims but not in state).
// Returns count of changes.
func (a *IPAllocator) ReconcileFromClaims(activeClaimIPs map[string]string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	changes := 0

	// Build reverse map: IP -> claimRef from active claims
	activeIPs := make(map[string]string)
	for claimRef, ip := range activeClaimIPs {
		// Strip CIDR suffix if present
		plainIP := ip
		if parsedIP, _, err := net.ParseCIDR(ip); err == nil {
			plainIP = parsedIP.String()
		}
		activeIPs[plainIP] = claimRef
	}

	// Remove stale entries
	for _, nodes := range a.state.Rails {
		for _, nodeState := range nodes {
			for ip, ref := range nodeState.Assignments {
				if activeRef, exists := activeIPs[ip]; !exists || activeRef != ref {
					delete(nodeState.Assignments, ip)
					changes++
				}
			}
		}
	}

	// Note: We don't add missing entries because we don't know which rail+node
	// they belong to from the activeClaimIPs map alone. The reconciler should
	// only remove stale entries; new allocations happen through AllocateIP.

	if changes > 0 {
		if err := a.save(); err != nil {
			klog.Errorf("Failed to save state after reconciling %d changes: %v", changes, err)
		}
	}

	return changes
}

// save atomically writes the state to disk.
func (a *IPAllocator) save() error {
	dir := filepath.Dir(a.filePath)
	tmpFile, err := os.CreateTemp(dir, "ip-state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath) // Clean up if rename fails
	}()

	encoder := json.NewEncoder(tmpFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(&a.state); err != nil {
		return fmt.Errorf("failed to encode state: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, a.filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// load reads the state from disk.
func (a *IPAllocator) load() error {
	data, err := os.ReadFile(a.filePath)
	if err != nil {
		return fmt.Errorf("failed to read state file: %w", err)
	}

	if err := json.Unmarshal(data, &a.state); err != nil {
		return fmt.Errorf("failed to unmarshal state: %w", err)
	}

	// Ensure maps are initialized
	if a.state.Rails == nil {
		a.state.Rails = make(map[string]map[string]*RailNodeState)
	}

	for railKey, nodes := range a.state.Rails {
		if nodes == nil {
			a.state.Rails[railKey] = make(map[string]*RailNodeState)
		}
		for _, nodeState := range nodes {
			if nodeState.Assignments == nil {
				nodeState.Assignments = make(map[string]string)
			}
		}
	}

	return nil
}

// nextAvailableIP finds the next available IP in the prefix.
// Returns the bare IP (e.g., "172.16.1.2"), not CIDR format.
func nextAvailableIP(prefix string, assigned map[string]string, reserved []string) (string, error) {
	_, ipNet, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR: %w", err)
	}

	// Build set of unavailable IPs
	unavailable := make(map[string]bool)
	for ip := range assigned {
		unavailable[ip] = true
	}
	for _, ip := range reserved {
		// Handle both plain IP and CIDR format
		plainIP := ip
		if parsedIP, _, err := net.ParseCIDR(ip); err == nil {
			plainIP = parsedIP.String()
		}
		unavailable[plainIP] = true
	}

	// Get network address and broadcast address
	networkIP := ipNet.IP.Mask(ipNet.Mask)
	broadcastIP := make(net.IP, len(networkIP))
	copy(broadcastIP, networkIP)
	for i := range broadcastIP {
		broadcastIP[i] |= ^ipNet.Mask[i]
	}

	// Iterate through host addresses
	for ip := incrementIP(networkIP); ipNet.Contains(ip); ip = incrementIP(ip) {
		ipStr := ip.String()

		// Skip network address and broadcast address
		if ip.Equal(networkIP) || ip.Equal(broadcastIP) {
			continue
		}

		if !unavailable[ipStr] {
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("prefix exhausted")
}

// incrementIP increments an IP address by one.
func incrementIP(ip net.IP) net.IP {
	// Work with a copy
	newIP := make(net.IP, len(ip))
	copy(newIP, ip)

	// Increment from the end
	for i := len(newIP) - 1; i >= 0; i-- {
		newIP[i]++
		if newIP[i] != 0 {
			break
		}
	}

	return newIP
}
