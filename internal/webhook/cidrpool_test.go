package webhook

import (
	"testing"
)

func TestCIDRPoolCache_GetAllocation(t *testing.T) {
	cache := &CIDRPoolCache{
		data: map[int]map[string]CIDRPoolAllocation{
			0: {
				"node1": {
					Prefix:      "172.16.1.0/24",
					Gateway:     "172.16.1.254",
					ReservedIPs: []string{"172.16.1.1", "172.16.1.254"},
				},
			},
			1: {
				"node2": {
					Prefix:      "172.16.2.0/24",
					Gateway:     "172.16.2.254",
					ReservedIPs: []string{"172.16.2.1", "172.16.2.254"},
				},
			},
		},
	}

	alloc, ok := cache.GetAllocation(0, "node1")
	if !ok {
		t.Fatal("expected allocation for rail 0, node1")
	}
	if alloc.Prefix != "172.16.1.0/24" {
		t.Errorf("expected prefix 172.16.1.0/24, got %s", alloc.Prefix)
	}
	if alloc.Gateway != "172.16.1.254" {
		t.Errorf("expected gateway 172.16.1.254, got %s", alloc.Gateway)
	}
	if len(alloc.ReservedIPs) != 2 {
		t.Errorf("expected 2 reserved IPs, got %d", len(alloc.ReservedIPs))
	}

	alloc, ok = cache.GetAllocation(1, "node2")
	if !ok {
		t.Fatal("expected allocation for rail 1, node2")
	}
	if alloc.Prefix != "172.16.2.0/24" {
		t.Errorf("expected prefix 172.16.2.0/24, got %s", alloc.Prefix)
	}
}

func TestCIDRPoolCache_GetAllocation_Miss(t *testing.T) {
	cache := &CIDRPoolCache{
		data: map[int]map[string]CIDRPoolAllocation{
			0: {
				"node1": {
					Prefix:  "172.16.1.0/24",
					Gateway: "172.16.1.254",
				},
			},
		},
	}

	_, ok := cache.GetAllocation(99, "node1")
	if ok {
		t.Error("expected miss for non-existent rail")
	}

	_, ok = cache.GetAllocation(0, "node99")
	if ok {
		t.Error("expected miss for non-existent node")
	}
}

func TestCIDRPoolCache_GetGateway(t *testing.T) {
	cache := &CIDRPoolCache{
		data: map[int]map[string]CIDRPoolAllocation{
			0: {
				"node1": {
					Prefix:  "172.16.1.0/24",
					Gateway: "172.16.1.254",
				},
			},
		},
	}

	gateway, err := cache.GetGateway(0, "node1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if gateway != "172.16.1.254" {
		t.Errorf("expected gateway 172.16.1.254, got %s", gateway)
	}

	_, err = cache.GetGateway(99, "node1")
	if err == nil {
		t.Error("expected error for non-existent rail")
	}

	_, err = cache.GetGateway(0, "node99")
	if err == nil {
		t.Error("expected error for non-existent node")
	}
}

func TestComputeReservedIPs(t *testing.T) {
	tests := []struct {
		name              string
		prefix            string
		perNodeExclusions []map[string]interface{}
		gatewayIndex      int
		expected          []string
	}{
		{
			name:   "single exclusion and gateway",
			prefix: "172.16.1.0/24",
			perNodeExclusions: []map[string]interface{}{
				{"startIndex": int64(1), "endIndex": int64(1)},
			},
			gatewayIndex: 254,
			expected:     []string{"172.16.1.1", "172.16.1.254"},
		},
		{
			name:   "range exclusion",
			prefix: "172.16.1.0/24",
			perNodeExclusions: []map[string]interface{}{
				{"startIndex": int64(1), "endIndex": int64(3)},
			},
			gatewayIndex: 254,
			expected:     []string{"172.16.1.1", "172.16.1.2", "172.16.1.3", "172.16.1.254"},
		},
		{
			name:              "no exclusions",
			prefix:            "172.16.1.0/24",
			perNodeExclusions: []map[string]interface{}{},
			gatewayIndex:      254,
			expected:          []string{"172.16.1.254"},
		},
		{
			name:   "multiple exclusions",
			prefix: "172.16.1.0/24",
			perNodeExclusions: []map[string]interface{}{
				{"startIndex": int64(1), "endIndex": int64(1)},
				{"startIndex": int64(5), "endIndex": int64(6)},
			},
			gatewayIndex: 254,
			expected:     []string{"172.16.1.1", "172.16.1.5", "172.16.1.6", "172.16.1.254"},
		},
		{
			name:              "no gateway index",
			prefix:            "172.16.1.0/24",
			perNodeExclusions: []map[string]interface{}{},
			gatewayIndex:      0,
			expected:          []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reserved, err := computeReservedIPs(tt.prefix, tt.perNodeExclusions, tt.gatewayIndex)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(reserved) != len(tt.expected) {
				t.Fatalf("expected %d reserved IPs, got %d", len(tt.expected), len(reserved))
			}
			for i, ip := range reserved {
				if ip != tt.expected[i] {
					t.Errorf("expected IP %s at index %d, got %s", tt.expected[i], i, ip)
				}
			}
		})
	}
}

func TestComputeReservedIPs_InvalidCIDR(t *testing.T) {
	_, err := computeReservedIPs("invalid", []map[string]interface{}{}, 254)
	if err == nil {
		t.Error("expected error for invalid CIDR")
	}
}
