package reconciler

import (
	"context"
	"fmt"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// ManagedByLabel identifies resources created by the webhook.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "dra-gpu-nic-webhook"

	// OrphanedAnnotation is set on resources detected as orphaned.
	OrphanedAnnotation = "dra.llm-d.io/orphaned-at"
)

// Reconciler detects and optionally reaps orphaned DRA resources.
type Reconciler struct {
	KubeClient kubernetes.Interface
	State      *StateManager
	Config     Config
}

// Run starts the reconciliation loop. It blocks until the context is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	klog.InfoS("Starting reconciler",
		"interval", r.Config.Interval,
		"autoReap", r.Config.AutoReap,
		"gracePeriod", r.Config.GracePeriod)

	// Run immediately on startup
	r.reconcileOnce(ctx)

	ticker := time.NewTicker(r.Config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.InfoS("Reconciler shutting down")
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce performs a single reconciliation pass.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	klog.V(2).InfoS("Starting reconciliation pass")
	start := time.Now()

	orphanedTemplates, err := r.reconcileTemplates(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to reconcile ResourceClaimTemplates")
	}

	orphanedClaims, err := r.reconcileClaims(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to reconcile ResourceClaims")
	}

	// Prune old reaped records
	pruned := r.State.PruneReapedOlderThan(r.Config.PruneAfter)
	if pruned > 0 {
		klog.InfoS("Pruned old reaped records", "count", pruned)
	}

	r.State.UpdateReconciliationTime()
	if err := r.State.Save(); err != nil {
		klog.ErrorS(err, "Failed to save reconciler state")
	}

	stats := r.State.GetStats()
	klog.InfoS("Reconciliation pass complete",
		"duration", time.Since(start).Round(time.Millisecond),
		"orphanedTemplates", orphanedTemplates,
		"orphanedClaims", orphanedClaims,
		"totalReconciliations", stats.TotalReconciliations,
		"cumulativeOrphansDetected", stats.OrphansDetected,
		"cumulativeOrphansReaped", stats.OrphansReaped)
}

// reconcileTemplates finds orphaned ResourceClaimTemplates managed by the webhook.
// A template is orphaned if no pod references it via spec.resourceClaims[].resourceClaimTemplateName.
func (r *Reconciler) reconcileTemplates(ctx context.Context) (int, error) {
	// List all webhook-managed templates across all namespaces
	templates, err := r.KubeClient.ResourceV1().ResourceClaimTemplates("").List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list templates: %w", err)
	}

	if len(templates.Items) == 0 {
		return 0, nil
	}

	// Build a set of template names referenced by active pods, grouped by namespace
	referencedTemplates := make(map[string]map[string]bool) // namespace -> set of template names

	// Get unique namespaces from templates
	namespaces := make(map[string]bool)
	for _, t := range templates.Items {
		namespaces[t.Namespace] = true
	}

	for ns := range namespaces {
		pods, err := r.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to list pods", "namespace", ns)
			continue
		}

		if referencedTemplates[ns] == nil {
			referencedTemplates[ns] = make(map[string]bool)
		}

		for _, pod := range pods.Items {
			for _, rc := range pod.Spec.ResourceClaims {
				if rc.ResourceClaimTemplateName != nil {
					referencedTemplates[ns][*rc.ResourceClaimTemplateName] = true
				}
			}
		}
	}

	// Check each template for orphan status
	orphanCount := 0
	for _, template := range templates.Items {
		nsRefs := referencedTemplates[template.Namespace]
		referenced := nsRefs != nil && nsRefs[template.Name]

		if referenced {
			// Template is in use — clear any previous orphan tracking
			r.State.ClearResolved("ResourceClaimTemplate", template.Namespace, template.Name)
			continue
		}

		// Template is not referenced by any pod — it's an orphan
		orphanCount++
		r.handleOrphanedTemplate(ctx, &template)
	}

	return orphanCount, nil
}

// reconcileClaims finds orphaned ResourceClaims that were created from webhook-managed
// templates but whose owning pod no longer exists.
func (r *Reconciler) reconcileClaims(ctx context.Context) (int, error) {
	// List all ResourceClaims across all namespaces
	claims, err := r.KubeClient.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list claims: %w", err)
	}

	orphanCount := 0
	for _, claim := range claims.Items {
		// Only check claims that were created from a template (have owner reference to a pod)
		if !isFromWebhookTemplate(claim) {
			continue
		}

		// Check if the owning pod still exists
		ownerPod := getOwnerPod(claim)
		if ownerPod == "" {
			// No pod owner — likely orphaned
			orphanCount++
			r.handleOrphanedClaim(ctx, &claim, "no pod owner reference")
			continue
		}

		_, err := r.KubeClient.CoreV1().Pods(claim.Namespace).Get(ctx, ownerPod, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				orphanCount++
				r.handleOrphanedClaim(ctx, &claim, fmt.Sprintf("owner pod %q not found", ownerPod))
				continue
			}
			klog.ErrorS(err, "Failed to check owner pod", "claim", claim.Name, "pod", ownerPod)
		} else {
			// Pod exists, claim is valid
			r.State.ClearResolved("ResourceClaim", claim.Namespace, claim.Name)
		}
	}

	return orphanCount, nil
}

// handleOrphanedTemplate processes a detected orphaned template.
func (r *Reconciler) handleOrphanedTemplate(ctx context.Context, template *resourcev1.ResourceClaimTemplate) {
	r.State.RecordOrphan("ResourceClaimTemplate", template.Namespace, template.Name,
		"not referenced by any pod")

	// Add orphaned annotation if not already present
	if template.Annotations == nil || template.Annotations[OrphanedAnnotation] == "" {
		r.annotateOrphan(ctx, "ResourceClaimTemplate", template.Namespace, template.Name)
	}

	// Auto-reap if enabled and past grace period
	if r.Config.AutoReap {
		record, exists := r.State.GetOrphan("ResourceClaimTemplate", template.Namespace, template.Name)
		if exists && time.Since(record.DetectedAt) >= r.Config.GracePeriod {
			r.reapTemplate(ctx, template)
		}
	}

	klog.InfoS("Orphaned ResourceClaimTemplate detected",
		"namespace", template.Namespace, "name", template.Name,
		"autoReap", r.Config.AutoReap)
}

// handleOrphanedClaim processes a detected orphaned claim.
func (r *Reconciler) handleOrphanedClaim(ctx context.Context, claim *resourcev1.ResourceClaim, reason string) {
	r.State.RecordOrphan("ResourceClaim", claim.Namespace, claim.Name, reason)

	// Add orphaned annotation if not already present
	if claim.Annotations == nil || claim.Annotations[OrphanedAnnotation] == "" {
		r.annotateOrphan(ctx, "ResourceClaim", claim.Namespace, claim.Name)
	}

	// Auto-reap if enabled and past grace period
	if r.Config.AutoReap {
		record, exists := r.State.GetOrphan("ResourceClaim", claim.Namespace, claim.Name)
		if exists && time.Since(record.DetectedAt) >= r.Config.GracePeriod {
			r.reapClaim(ctx, claim)
		}
	}

	klog.InfoS("Orphaned ResourceClaim detected",
		"namespace", claim.Namespace, "name", claim.Name,
		"reason", reason, "autoReap", r.Config.AutoReap)
}

// annotateOrphan adds the orphaned-at annotation to a resource.
func (r *Reconciler) annotateOrphan(ctx context.Context, kind, namespace, name string) {
	now := time.Now().UTC().Format(time.RFC3339)

	switch kind {
	case "ResourceClaimTemplate":
		template, err := r.KubeClient.ResourceV1().ResourceClaimTemplates(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to get template for annotation", "namespace", namespace, "name", name)
			return
		}
		if template.Annotations == nil {
			template.Annotations = make(map[string]string)
		}
		template.Annotations[OrphanedAnnotation] = now
		_, err = r.KubeClient.ResourceV1().ResourceClaimTemplates(namespace).Update(ctx, template, metav1.UpdateOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to annotate orphaned template", "namespace", namespace, "name", name)
		}

	case "ResourceClaim":
		claim, err := r.KubeClient.ResourceV1().ResourceClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to get claim for annotation", "namespace", namespace, "name", name)
			return
		}
		if claim.Annotations == nil {
			claim.Annotations = make(map[string]string)
		}
		claim.Annotations[OrphanedAnnotation] = now
		_, err = r.KubeClient.ResourceV1().ResourceClaims(namespace).Update(ctx, claim, metav1.UpdateOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to annotate orphaned claim", "namespace", namespace, "name", name)
		}
	}
}

// reapTemplate deletes an orphaned ResourceClaimTemplate.
func (r *Reconciler) reapTemplate(ctx context.Context, template *resourcev1.ResourceClaimTemplate) {
	klog.InfoS("Reaping orphaned ResourceClaimTemplate",
		"namespace", template.Namespace, "name", template.Name)

	err := r.KubeClient.ResourceV1().ResourceClaimTemplates(template.Namespace).Delete(
		ctx, template.Name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to reap template", "namespace", template.Namespace, "name", template.Name)
		return
	}

	r.State.MarkReaped("ResourceClaimTemplate", template.Namespace, template.Name)
}

// reapClaim deletes an orphaned ResourceClaim.
func (r *Reconciler) reapClaim(ctx context.Context, claim *resourcev1.ResourceClaim) {
	klog.InfoS("Reaping orphaned ResourceClaim",
		"namespace", claim.Namespace, "name", claim.Name)

	err := r.KubeClient.ResourceV1().ResourceClaims(claim.Namespace).Delete(
		ctx, claim.Name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to reap claim", "namespace", claim.Namespace, "name", claim.Name)
		return
	}

	r.State.MarkReaped("ResourceClaim", claim.Namespace, claim.Name)
}

// isFromWebhookTemplate checks if a ResourceClaim was likely created from a
// webhook-managed template by checking the annotation that Kubernetes adds.
func isFromWebhookTemplate(claim resourcev1.ResourceClaim) bool {
	if claim.Annotations == nil {
		return false
	}
	// Kubernetes sets this annotation when creating a claim from a template
	_, hasClaimName := claim.Annotations["resource.kubernetes.io/pod-claim-name"]
	return hasClaimName
}

// getOwnerPod returns the name of the owning Pod from ownerReferences, or "".
func getOwnerPod(claim resourcev1.ResourceClaim) string {
	for _, ref := range claim.OwnerReferences {
		if ref.Kind == "Pod" && ref.APIVersion == "v1" {
			return ref.Name
		}
	}
	return ""
}
