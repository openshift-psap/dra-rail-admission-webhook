package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func strP(s string) *string { return &s }

func testReconcilerConfig(t *testing.T) Config {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	return Config{
		Interval:    1 * time.Second,
		AutoReap:    false,
		GracePeriod: 1 * time.Minute,
		PruneAfter:  1 * time.Hour,
		StatePath:   statePath,
	}
}

func webhookTemplate(namespace, name string) *resourcev1.ResourceClaimTemplate {
	return &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
			},
		},
	}
}

func podReferencingTemplate(namespace, podName, templateName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:                      "gpu-nic-devices",
					ResourceClaimTemplateName: strP(templateName),
				},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
			},
		},
	}
}

func orphanedClaim(namespace, claimName, ownerPodName string) *resourcev1.ResourceClaim {
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: namespace,
			Annotations: map[string]string{
				"resource.kubernetes.io/pod-claim-name": "gpu-nic-devices",
			},
		},
	}
	if ownerPodName != "" {
		claim.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       ownerPodName,
			},
		}
	}
	return claim
}

func TestReconcileTemplates_NoOrphans(t *testing.T) {
	template := webhookTemplate("default", "gpu-nic-2-numa-abc123")
	pod := podReferencingTemplate("default", "my-pod", "gpu-nic-2-numa-abc123")

	client := fake.NewSimpleClientset(template, pod)
	cfg := testReconcilerConfig(t)
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}
	count, err := rec.reconcileTemplates(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 orphans, got %d", count)
	}
}

func TestReconcileTemplates_DetectsOrphan(t *testing.T) {
	// Template with no pod referencing it
	template := webhookTemplate("default", "gpu-nic-4-numa-old123")

	client := fake.NewSimpleClientset(template)
	cfg := testReconcilerConfig(t)
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}
	count, err := rec.reconcileTemplates(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 orphan, got %d", count)
	}

	// Verify it was recorded in state
	orphans := state.GetActiveOrphans()
	if len(orphans) != 1 {
		t.Fatalf("expected 1 active orphan in state, got %d", len(orphans))
	}
	if orphans[0].Name != "gpu-nic-4-numa-old123" {
		t.Errorf("orphan name = %q, want gpu-nic-4-numa-old123", orphans[0].Name)
	}

	// Verify annotation was added
	updated, err := client.ResourceV1().ResourceClaimTemplates("default").Get(
		context.Background(), "gpu-nic-4-numa-old123", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get template: %v", err)
	}
	if updated.Annotations[OrphanedAnnotation] == "" {
		t.Error("expected orphaned-at annotation to be set")
	}
}

func TestReconcileTemplates_ClearsResolvedOrphan(t *testing.T) {
	template := webhookTemplate("default", "gpu-nic-2-numa-abc123")

	client := fake.NewSimpleClientset(template)
	cfg := testReconcilerConfig(t)
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}

	// First pass: detect as orphan
	count, _ := rec.reconcileTemplates(context.Background())
	if count != 1 {
		t.Fatalf("expected 1 orphan on first pass, got %d", count)
	}

	// Now add a pod referencing this template
	pod := podReferencingTemplate("default", "new-pod", "gpu-nic-2-numa-abc123")
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Second pass: should clear the orphan
	count, _ = rec.reconcileTemplates(context.Background())
	if count != 0 {
		t.Errorf("expected 0 orphans after pod created, got %d", count)
	}

	orphans := state.GetActiveOrphans()
	if len(orphans) != 0 {
		t.Errorf("expected 0 active orphans in state, got %d", len(orphans))
	}
}

func TestReconcileTemplates_AutoReap(t *testing.T) {
	template := webhookTemplate("default", "gpu-nic-old-stale")

	client := fake.NewSimpleClientset(template)
	cfg := testReconcilerConfig(t)
	cfg.AutoReap = true
	cfg.GracePeriod = 0 // Reap immediately for testing
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}

	count, err := rec.reconcileTemplates(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 orphan, got %d", count)
	}

	// Verify template was deleted
	_, err = client.ResourceV1().ResourceClaimTemplates("default").Get(
		context.Background(), "gpu-nic-old-stale", metav1.GetOptions{})
	if err == nil {
		t.Error("expected template to be deleted (reaped)")
	}

	// Verify state records the reap
	stats := state.GetStats()
	if stats.OrphansReaped != 1 {
		t.Errorf("expected 1 reap, got %d", stats.OrphansReaped)
	}
}

func TestReconcileTemplates_NoAutoReapBeforeGracePeriod(t *testing.T) {
	template := webhookTemplate("default", "gpu-nic-recent")

	client := fake.NewSimpleClientset(template)
	cfg := testReconcilerConfig(t)
	cfg.AutoReap = true
	cfg.GracePeriod = 1 * time.Hour // Won't expire during test
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}

	count, _ := rec.reconcileTemplates(context.Background())
	if count != 1 {
		t.Fatalf("expected 1 orphan, got %d", count)
	}

	// Template should still exist (grace period not elapsed)
	_, err := client.ResourceV1().ResourceClaimTemplates("default").Get(
		context.Background(), "gpu-nic-recent", metav1.GetOptions{})
	if err != nil {
		t.Error("template should NOT be deleted before grace period")
	}
}

func TestReconcileClaims_OrphanedPodGone(t *testing.T) {
	claim := orphanedClaim("default", "my-claim", "deleted-pod")

	client := fake.NewSimpleClientset(claim)
	cfg := testReconcilerConfig(t)
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}

	count, err := rec.reconcileClaims(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 orphaned claim, got %d", count)
	}

	orphans := state.GetActiveOrphans()
	if len(orphans) != 1 {
		t.Fatalf("expected 1 active orphan, got %d", len(orphans))
	}
	if orphans[0].Kind != "ResourceClaim" {
		t.Errorf("orphan kind = %q, want ResourceClaim", orphans[0].Kind)
	}
}

func TestReconcileClaims_PodExists(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	claim := orphanedClaim("default", "valid-claim", "existing-pod")

	client := fake.NewSimpleClientset(pod, claim)
	cfg := testReconcilerConfig(t)
	state, _ := NewStateManager(cfg.StatePath)

	rec := &Reconciler{KubeClient: client, State: state, Config: cfg}

	count, err := rec.reconcileClaims(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 orphans (pod exists), got %d", count)
	}
}

func TestStatePersistence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")

	// Create state, record orphan, save
	state1, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	state1.RecordOrphan("ResourceClaimTemplate", "ns", "test-tmpl", "no pods")
	state1.UpdateReconciliationTime()
	if err := state1.Save(); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	// Reload and verify
	state2, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to reload state: %v", err)
	}
	orphans := state2.GetActiveOrphans()
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan after reload, got %d", len(orphans))
	}
	if orphans[0].Name != "test-tmpl" {
		t.Errorf("orphan name = %q, want test-tmpl", orphans[0].Name)
	}

	stats := state2.GetStats()
	if stats.OrphansDetected != 1 {
		t.Errorf("stats.OrphansDetected = %d, want 1", stats.OrphansDetected)
	}
}

func TestStatePruning(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state, _ := NewStateManager(statePath)

	state.RecordOrphan("ResourceClaimTemplate", "ns", "old-tmpl", "stale")
	state.MarkReaped("ResourceClaimTemplate", "ns", "old-tmpl")

	// Prune with zero age — should remove the reaped record
	pruned := state.PruneReapedOlderThan(0)
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}

	orphans := state.GetActiveOrphans()
	if len(orphans) != 0 {
		t.Errorf("expected 0 active orphans after prune, got %d", len(orphans))
	}
}

func TestStateAtomicWrite(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state, _ := NewStateManager(statePath)

	state.RecordOrphan("ResourceClaim", "ns", "claim1", "test")
	if err := state.Save(); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	// Verify no temp file left behind
	tmpPath := statePath + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Error("temp file should not exist after save")
	}

	// Verify actual file exists and is valid JSON
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	if len(data) == 0 {
		t.Error("state file should not be empty")
	}
}
