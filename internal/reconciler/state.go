package reconciler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// OrphanRecord tracks when a resource was first detected as orphaned
// and when (if) it was reaped.
type OrphanRecord struct {
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"` // "ResourceClaimTemplate" or "ResourceClaim"
	DetectedAt time.Time  `json:"detectedAt"`
	ReapedAt   *time.Time `json:"reapedAt,omitempty"`
	Reason     string     `json:"reason"`
}

// ReconcilerState is the persistent state written to disk.
type ReconcilerState struct {
	LastReconciliation time.Time               `json:"lastReconciliation"`
	Orphans            map[string]OrphanRecord  `json:"orphans"` // key: "kind/namespace/name"
	Stats              ReconcilerStats          `json:"stats"`
}

// ReconcilerStats tracks cumulative statistics.
type ReconcilerStats struct {
	TotalReconciliations int `json:"totalReconciliations"`
	OrphansDetected      int `json:"orphansDetected"`
	OrphansReaped        int `json:"orphansReaped"`
}

// StateManager handles reading/writing reconciler state to disk.
type StateManager struct {
	mu       sync.Mutex
	filePath string
	state    ReconcilerState
}

// NewStateManager creates a StateManager that persists to the given file path.
func NewStateManager(filePath string) (*StateManager, error) {
	sm := &StateManager{
		filePath: filePath,
		state: ReconcilerState{
			Orphans: make(map[string]OrphanRecord),
		},
	}

	// Ensure parent directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory %s: %w", dir, err)
	}

	// Load existing state if present
	if err := sm.load(); err != nil {
		klog.InfoS("No existing state found, starting fresh", "path", filePath, "err", err)
	} else {
		klog.InfoS("Loaded existing state",
			"path", filePath,
			"lastReconciliation", sm.state.LastReconciliation,
			"trackedOrphans", len(sm.state.Orphans))
	}

	return sm, nil
}

// load reads state from disk.
func (sm *StateManager) load() error {
	data, err := os.ReadFile(sm.filePath)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &sm.state)
}

// Save writes the current state to disk atomically.
func (sm *StateManager) Save() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write atomically: write to temp file, then rename
	tmpPath := sm.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0640); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	if err := os.Rename(tmpPath, sm.filePath); err != nil {
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

// RecordOrphan records a newly detected orphan. If already tracked, it is not overwritten.
func (sm *StateManager) RecordOrphan(kind, namespace, name, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := orphanKey(kind, namespace, name)
	if _, exists := sm.state.Orphans[key]; !exists {
		sm.state.Orphans[key] = OrphanRecord{
			Namespace:  namespace,
			Name:       name,
			Kind:       kind,
			DetectedAt: time.Now(),
			Reason:     reason,
		}
		sm.state.Stats.OrphansDetected++
	}
}

// MarkReaped marks an orphan as reaped (deleted).
func (sm *StateManager) MarkReaped(kind, namespace, name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := orphanKey(kind, namespace, name)
	if record, exists := sm.state.Orphans[key]; exists {
		now := time.Now()
		record.ReapedAt = &now
		sm.state.Orphans[key] = record
		sm.state.Stats.OrphansReaped++
	}
}

// ClearResolved removes orphan records that are no longer orphaned
// (e.g., a pod started referencing the template again).
func (sm *StateManager) ClearResolved(kind, namespace, name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := orphanKey(kind, namespace, name)
	record, exists := sm.state.Orphans[key]
	if exists && record.ReapedAt == nil {
		delete(sm.state.Orphans, key)
	}
}

// GetOrphan returns the orphan record if it exists and hasn't been reaped.
func (sm *StateManager) GetOrphan(kind, namespace, name string) (OrphanRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := orphanKey(kind, namespace, name)
	record, exists := sm.state.Orphans[key]
	if exists && record.ReapedAt == nil {
		return record, true
	}
	return OrphanRecord{}, false
}

// GetActiveOrphans returns all orphans that haven't been reaped.
func (sm *StateManager) GetActiveOrphans() []OrphanRecord {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var active []OrphanRecord
	for _, record := range sm.state.Orphans {
		if record.ReapedAt == nil {
			active = append(active, record)
		}
	}
	return active
}

// UpdateReconciliationTime updates the last reconciliation timestamp.
func (sm *StateManager) UpdateReconciliationTime() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.state.LastReconciliation = time.Now()
	sm.state.Stats.TotalReconciliations++
}

// GetStats returns a copy of the current stats.
func (sm *StateManager) GetStats() ReconcilerStats {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.Stats
}

// PruneReapedOlderThan removes reaped orphan records older than the given duration
// to keep the state file from growing unbounded.
func (sm *StateManager) PruneReapedOlderThan(age time.Duration) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cutoff := time.Now().Add(-age)
	pruned := 0
	for key, record := range sm.state.Orphans {
		if record.ReapedAt != nil && record.ReapedAt.Before(cutoff) {
			delete(sm.state.Orphans, key)
			pruned++
		}
	}
	return pruned
}

func orphanKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}
