package webhook

import (
	"strings"
	"testing"
)

func TestValidateRequest(t *testing.T) {
	cfg := Config{
		MaxPairsPerNUMA: 4,
		MaxPairsPerNode: 8,
	}

	tests := []struct {
		name           string
		count          int
		allowCrossNUMA bool
		wantErr        bool
		errContains    string
	}{
		{
			name:    "valid: 1 pair",
			count:   1,
			wantErr: false,
		},
		{
			name:    "valid: 4 pairs (max per NUMA)",
			count:   4,
			wantErr: false,
		},
		{
			name:    "valid: 4 pairs with cross-NUMA",
			count:   4,
			allowCrossNUMA: true,
			wantErr: false,
		},
		{
			name:           "valid: 5 pairs with cross-NUMA override",
			count:          5,
			allowCrossNUMA: true,
			wantErr:        false,
		},
		{
			name:           "valid: 8 pairs with cross-NUMA",
			count:          8,
			allowCrossNUMA: true,
			wantErr:        false,
		},
		{
			name:        "invalid: 0 pairs",
			count:       0,
			wantErr:     true,
			errContains: "at least 1",
		},
		{
			name:        "invalid: negative",
			count:       -1,
			wantErr:     true,
			errContains: "at least 1",
		},
		{
			name:        "invalid: exceeds node max",
			count:       9,
			wantErr:     true,
			errContains: "exceeds maximum per node",
		},
		{
			name:           "invalid: exceeds node max even with cross-NUMA",
			count:          9,
			allowCrossNUMA: true,
			wantErr:        true,
			errContains:    "exceeds maximum per node",
		},
		{
			name:        "invalid: 5 pairs without cross-NUMA",
			count:       5,
			wantErr:     true,
			errContains: "exceeds single NUMA zone capacity",
		},
		{
			name:    "valid: 8 pairs auto-allows cross-NUMA (full node)",
			count:   8,
			wantErr: false,
		},
		{
			name:        "invalid: 7 pairs without cross-NUMA",
			count:       7,
			wantErr:     true,
			errContains: "exceeds single NUMA zone capacity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequest(tt.count, tt.allowCrossNUMA, cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRequest(%d, %v) error = %v, wantErr %v", tt.count, tt.allowCrossNUMA, err, tt.wantErr)
			}
			if err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestValidateRequest_PCIeRootOnlyMode(t *testing.T) {
	cfg := Config{
		MaxPairsPerNUMA: 4,
		MaxPairsPerNode: 8,
		PairingMode:     PairingModePCIeRootOnly,
	}

	// 6 pairs without cross-NUMA annotation should succeed in pcieRootOnly mode
	if err := ValidateRequest(6, false, cfg); err != nil {
		t.Errorf("pcieRootOnly mode should allow 6 pairs without cross-NUMA: %v", err)
	}

	// 7 pairs should also succeed
	if err := ValidateRequest(7, false, cfg); err != nil {
		t.Errorf("pcieRootOnly mode should allow 7 pairs without cross-NUMA: %v", err)
	}

	// 8 pairs (full node) should succeed
	if err := ValidateRequest(8, false, cfg); err != nil {
		t.Errorf("pcieRootOnly mode should allow 8 pairs: %v", err)
	}

	// 9 pairs should still fail (exceeds MaxPairsPerNode)
	if err := ValidateRequest(9, false, cfg); err == nil {
		t.Error("pcieRootOnly mode should still enforce MaxPairsPerNode")
	}
}

func TestValidateRequest_RailsCount(t *testing.T) {
	cfg := Config{
		MaxPairsPerNUMA: 4,
		MaxPairsPerNode: 8,
		NICConfig: NICConfig{
			Rails: []RailConfig{
				{Subnet: "10.0.0.0/16", Gateway: "10.0.0.1", IPv4Prefix: "10.0."},
				{Subnet: "10.1.0.0/16", Gateway: "10.1.0.1", IPv4Prefix: "10.1."},
				{Subnet: "10.2.0.0/16", Gateway: "10.2.0.1", IPv4Prefix: "10.2."},
				{Subnet: "10.3.0.0/16", Gateway: "10.3.0.1", IPv4Prefix: "10.3."},
			},
		},
	}

	// 4 pairs with 4 rails: OK
	if err := ValidateRequest(4, true, cfg); err != nil {
		t.Errorf("4 pairs with 4 rails should be valid: %v", err)
	}

	// 5 pairs with 4 rails: exceeds rails
	err := ValidateRequest(5, true, cfg)
	if err == nil {
		t.Fatal("5 pairs with 4 rails should fail")
	}
	if !strings.Contains(err.Error(), "configured rails") {
		t.Errorf("error %q should mention configured rails", err.Error())
	}
}

func TestValidateExplicitRequest(t *testing.T) {
	pool := &NodePoolMapping{
		NodePoolLabel: "gpu-h100",
		Pairs: []ExplicitPairMapping{
			{Devices: map[string]string{"gpu": "a", "nic": "b"}},
			{Devices: map[string]string{"gpu": "c", "nic": "d"}},
		},
	}
	cfg := Config{MaxPairsPerNode: 8}

	if err := ValidateExplicitRequest(1, pool, cfg); err != nil {
		t.Errorf("1 pair should be valid: %v", err)
	}
	if err := ValidateExplicitRequest(2, pool, cfg); err != nil {
		t.Errorf("2 pairs should be valid: %v", err)
	}
	if err := ValidateExplicitRequest(3, pool, cfg); err == nil {
		t.Error("3 pairs should exceed pool pair count of 2")
	}
	if err := ValidateExplicitRequest(0, pool, cfg); err == nil {
		t.Error("0 pairs should fail")
	}
}
