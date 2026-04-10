package reconciler

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"gopkg.in/yaml.v3"
)

// Config holds the reconciler configuration.
type Config struct {
	// Interval is the reconciliation loop interval.
	Interval time.Duration `yaml:"-"`
	// IntervalStr is the string representation parsed from config (e.g. "5m", "30s").
	IntervalStr string `yaml:"interval"`

	// AutoReap enables automatic deletion of orphaned resources after the grace period.
	AutoReap bool `yaml:"autoReap"`

	// GracePeriod is how long an orphan must be detected before it can be auto-reaped.
	GracePeriod    time.Duration `yaml:"-"`
	GracePeriodStr string        `yaml:"gracePeriod"`

	// PruneAfter is how long to keep reaped records in state before pruning.
	PruneAfter    time.Duration `yaml:"-"`
	PruneAfterStr string        `yaml:"pruneAfter"`

	// StatePath is the file path for persistent state.
	StatePath string `yaml:"statePath"`
}

// DefaultReconcilerConfig returns a Config with sensible defaults.
func DefaultReconcilerConfig() Config {
	return Config{
		Interval:       5 * time.Minute,
		IntervalStr:    "5m",
		AutoReap:       false,
		GracePeriod:    10 * time.Minute,
		GracePeriodStr: "10m",
		PruneAfter:     7 * 24 * time.Hour,
		PruneAfterStr:  "168h",
		StatePath:      "/data/reconciler-state.json",
	}
}

// LoadReconcilerConfig reads the reconciler config from a ConfigMap.
func LoadReconcilerConfig(ctx context.Context, client kubernetes.Interface, namespace, name string) (Config, error) {
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Config{}, fmt.Errorf("failed to get configmap %s/%s: %w", namespace, name, err)
	}

	data, ok := cm.Data["reconciler.yaml"]
	if !ok {
		return DefaultReconcilerConfig(), nil
	}

	cfg := DefaultReconcilerConfig()
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse reconciler.yaml: %w", err)
	}

	// Parse duration strings
	if cfg.IntervalStr != "" {
		d, err := time.ParseDuration(cfg.IntervalStr)
		if err != nil {
			return Config{}, fmt.Errorf("invalid interval %q: %w", cfg.IntervalStr, err)
		}
		cfg.Interval = d
	}
	if cfg.GracePeriodStr != "" {
		d, err := time.ParseDuration(cfg.GracePeriodStr)
		if err != nil {
			return Config{}, fmt.Errorf("invalid gracePeriod %q: %w", cfg.GracePeriodStr, err)
		}
		cfg.GracePeriod = d
	}
	if cfg.PruneAfterStr != "" {
		d, err := time.ParseDuration(cfg.PruneAfterStr)
		if err != nil {
			return Config{}, fmt.Errorf("invalid pruneAfter %q: %w", cfg.PruneAfterStr, err)
		}
		cfg.PruneAfter = d
	}

	return cfg, nil
}
