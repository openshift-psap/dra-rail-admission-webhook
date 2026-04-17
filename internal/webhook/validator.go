package webhook

import "fmt"

// ValidateRequest checks whether the requested GPU-NIC pair count is valid
// given the NUMA and node constraints from the config.
func ValidateRequest(count int, allowCrossNUMA bool, cfg Config) error {
	if count <= 0 {
		return fmt.Errorf("gpu-nic-pair count must be at least 1, got %d", count)
	}

	if count > cfg.MaxPairsPerNode {
		return fmt.Errorf("gpu-nic-pair count %d exceeds maximum per node (%d)", count, cfg.MaxPairsPerNode)
	}

	if len(cfg.NICConfig.Rails) > 0 && count > len(cfg.NICConfig.Rails) {
		return fmt.Errorf("gpu-nic-pair count %d exceeds number of configured rails (%d)", count, len(cfg.NICConfig.Rails))
	}

	// Requesting all pairs on a node implicitly requires cross-NUMA;
	// allow it automatically instead of forcing the user to annotate.
	if count == cfg.MaxPairsPerNode {
		return nil
	}

	if count > cfg.MaxPairsPerNUMA && !allowCrossNUMA {
		return fmt.Errorf(
			"gpu-nic-pair count %d exceeds single NUMA zone capacity (%d); "+
				"set annotation %s=true to allow cross-NUMA allocation",
			count, cfg.MaxPairsPerNUMA, AnnotationAllowCrossNUMA,
		)
	}

	return nil
}

// ValidateExplicitRequest checks that the requested count is satisfiable
// by the given node pool mapping.
func ValidateExplicitRequest(count int, poolMapping *NodePoolMapping, cfg Config) error {
	if count <= 0 {
		return fmt.Errorf("gpu-nic-pair count must be at least 1, got %d", count)
	}
	if poolMapping == nil {
		return fmt.Errorf("no node pool mapping resolved for this node")
	}
	if count > len(poolMapping.Pairs) {
		return fmt.Errorf("gpu-nic-pair count %d exceeds node pool %q pair count (%d)",
			count, poolMapping.NodePoolLabel, len(poolMapping.Pairs))
	}
	if count > cfg.MaxPairsPerNode {
		return fmt.Errorf("gpu-nic-pair count %d exceeds maximum per node (%d)", count, cfg.MaxPairsPerNode)
	}
	return nil
}
