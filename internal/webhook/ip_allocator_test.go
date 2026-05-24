package webhook

import (
	"path/filepath"
	"testing"
)

func TestIPAllocator_AllocateSequential(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Allocate 3 IPs
	ip1, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	ip2, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	ip3, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim3", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 3: %v", err)
	}

	// Verify sequential and in CIDR format
	expected := []string{"172.16.1.1/24", "172.16.1.2/24", "172.16.1.3/24"}
	actual := []string{ip1, ip2, ip3}

	for i, exp := range expected {
		if actual[i] != exp {
			t.Errorf("IP %d: expected %s, got %s", i+1, exp, actual[i])
		}
	}
}

func TestIPAllocator_SkipsReserved(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Reserve .1 and .254
	reserved := []string{"172.16.1.1", "172.16.1.254"}

	// Allocate 2 IPs
	ip1, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim1", reserved)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	ip2, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim2", reserved)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	// Should skip .1 and start at .2
	expected := []string{"172.16.1.2/24", "172.16.1.3/24"}
	actual := []string{ip1, ip2}

	for i, exp := range expected {
		if actual[i] != exp {
			t.Errorf("IP %d: expected %s, got %s", i+1, exp, actual[i])
		}
	}
}

func TestIPAllocator_Release(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Allocate 2 IPs
	ip1, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	ip2, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	// Release first IP
	if err := allocator.ReleaseIP(nodeName, railIndex, ip1); err != nil {
		t.Fatalf("Failed to release IP: %v", err)
	}

	// Allocate again - should reuse released IP
	ip3, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim3", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 3: %v", err)
	}

	if ip3 != ip1 {
		t.Errorf("Expected to reuse released IP %s, got %s", ip1, ip3)
	}

	// ip2 should still be allocated
	if ip2 != "172.16.1.2/24" {
		t.Errorf("IP 2 should be 172.16.1.2/24, got %s", ip2)
	}
}

func TestIPAllocator_ReleaseByClaimRef(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Allocate 3 IPs with same claim ref on different rails (idempotent per rail+node)
	claimRef := "same-claim"
	for i := 0; i < 3; i++ {
		_, err := allocator.AllocateIP(nodeName, i, prefix, claimRef, nil)
		if err != nil {
			t.Fatalf("Failed to allocate IP %d: %v", i+1, err)
		}
	}

	// Allocate 1 IP with different claim ref
	_, err = allocator.AllocateIP(nodeName, railIndex, prefix, "other-claim", nil)
	if err != nil {
		t.Fatalf("Failed to allocate other IP: %v", err)
	}

	// Release by claim ref — should free 3 (one per rail)
	freed := allocator.ReleaseByClaimRef(claimRef)
	if freed != 3 {
		t.Errorf("Expected to free 3 IPs, freed %d", freed)
	}

	// Verify state — only "other-claim" remains on rail 0
	allocator.mu.Lock()
	railKey := "0"
	nodeState := allocator.state.Rails[railKey][nodeName]
	if len(nodeState.Assignments) != 1 {
		t.Errorf("Expected 1 remaining assignment, got %d", len(nodeState.Assignments))
	}
	allocator.mu.Unlock()
}

func TestIPAllocator_PersistenceRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// First allocator
	allocator1, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator1: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Allocate 2 IPs
	ip1, err := allocator1.AllocateIP(nodeName, railIndex, prefix, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	ip2, err := allocator1.AllocateIP(nodeName, railIndex, prefix, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	// Create second allocator from same file
	allocator2, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator2: %v", err)
	}

	// Verify state loaded correctly
	allocator2.mu.Lock()
	railKey := "0"
	nodeState := allocator2.state.Rails[railKey][nodeName]
	if nodeState == nil {
		t.Fatal("Node state is nil")
	}
	if nodeState.Prefix != prefix {
		t.Errorf("Expected prefix %s, got %s", prefix, nodeState.Prefix)
	}
	if len(nodeState.Assignments) != 2 {
		t.Errorf("Expected 2 assignments, got %d", len(nodeState.Assignments))
	}
	allocator2.mu.Unlock()

	// Allocate next IP from second allocator
	ip3, err := allocator2.AllocateIP(nodeName, railIndex, prefix, "claim3", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 3: %v", err)
	}

	// Verify sequential
	expected := []string{"172.16.1.1/24", "172.16.1.2/24", "172.16.1.3/24"}
	actual := []string{ip1, ip2, ip3}

	for i, exp := range expected {
		if actual[i] != exp {
			t.Errorf("IP %d: expected %s, got %s", i+1, exp, actual[i])
		}
	}
}

func TestIPAllocator_PrefixExhaustion(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	// /30 gives us 4 addresses: .0 (network), .1, .2, .3 (broadcast)
	// So only .1 and .2 are usable
	prefix := "172.16.1.0/30"
	nodeName := "node1"
	railIndex := 0

	// Allocate 2 IPs - should succeed
	_, err = allocator.AllocateIP(nodeName, railIndex, prefix, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	_, err = allocator.AllocateIP(nodeName, railIndex, prefix, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	// Third allocation should fail
	_, err = allocator.AllocateIP(nodeName, railIndex, prefix, "claim3", nil)
	if err == nil {
		t.Fatal("Expected allocation to fail due to exhaustion, but it succeeded")
	}
}

func TestIPAllocator_ReconcileFromClaims(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	prefix := "172.16.1.0/24"
	nodeName := "node1"
	railIndex := 0

	// Allocate 3 IPs
	ip1, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 1: %v", err)
	}

	ip2, err := allocator.AllocateIP(nodeName, railIndex, prefix, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 2: %v", err)
	}

	_, err = allocator.AllocateIP(nodeName, railIndex, prefix, "claim3", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP 3: %v", err)
	}

	// Reconcile with only claim1 and claim2 active
	activeClaimIPs := map[string]string{
		"claim1": ip1,
		"claim2": ip2,
	}

	changes := allocator.ReconcileFromClaims(activeClaimIPs)
	if changes != 1 {
		t.Errorf("Expected 1 change (removal of claim3), got %d", changes)
	}

	// Verify state
	allocator.mu.Lock()
	railKey := "0"
	nodeState := allocator.state.Rails[railKey][nodeName]
	if len(nodeState.Assignments) != 2 {
		t.Errorf("Expected 2 remaining assignments, got %d", len(nodeState.Assignments))
	}
	allocator.mu.Unlock()
}

func TestIPAllocator_PrefixChange(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	allocator, err := NewIPAllocator(stateFile)
	if err != nil {
		t.Fatalf("Failed to create allocator: %v", err)
	}

	nodeName := "node1"
	railIndex := 0

	// Allocate with first prefix
	prefix1 := "172.16.1.0/24"
	ip1, err := allocator.AllocateIP(nodeName, railIndex, prefix1, "claim1", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP with prefix1: %v", err)
	}

	if ip1 != "172.16.1.1/24" {
		t.Errorf("Expected 172.16.1.1/24, got %s", ip1)
	}

	// Allocate with different prefix for same rail+node
	prefix2 := "192.168.1.0/24"
	ip2, err := allocator.AllocateIP(nodeName, railIndex, prefix2, "claim2", nil)
	if err != nil {
		t.Fatalf("Failed to allocate IP with prefix2: %v", err)
	}

	// Should start fresh from new prefix
	if ip2 != "192.168.1.1/24" {
		t.Errorf("Expected 192.168.1.1/24, got %s", ip2)
	}

	// Verify old assignments cleared
	allocator.mu.Lock()
	railKey := "0"
	nodeState := allocator.state.Rails[railKey][nodeName]
	if nodeState.Prefix != prefix2 {
		t.Errorf("Expected prefix %s, got %s", prefix2, nodeState.Prefix)
	}
	if len(nodeState.Assignments) != 1 {
		t.Errorf("Expected 1 assignment (old cleared), got %d", len(nodeState.Assignments))
	}
	allocator.mu.Unlock()
}
