//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	globalClient    kubernetes.Interface
	globalWebhookNS string
	globalCMName    string
	savedCMData     map[string]string
)

func TestMain(m *testing.M) {
	kubeconfig := envOrDefault("E2E_KUBECONFIG", defaultKubeconfig)
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot build kubeconfig from %s: %v\n", kubeconfig, err)
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot create Kubernetes client: %v\n", err)
		os.Exit(1)
	}
	globalClient = client
	globalWebhookNS = envOrDefault("E2E_WEBHOOK_NS", defaultWebhookNS)
	globalCMName = envOrDefault("E2E_CONFIGMAP", defaultConfigMap)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Cluster connectivity
	_, err = client.Discovery().ServerVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cluster not reachable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK: cluster reachable")

	// 2. Webhook deployment ready
	dep, err := client.AppsV1().Deployments(globalWebhookNS).Get(ctx, webhookDeployment, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: webhook deployment not found: %v\n", err)
		os.Exit(1)
	}
	if dep.Status.ReadyReplicas < 1 {
		fmt.Fprintf(os.Stderr, "FATAL: webhook deployment has 0 ready replicas\n")
		os.Exit(1)
	}
	fmt.Printf("OK: webhook deployment ready (%d/%d replicas)\n", dep.Status.ReadyReplicas, *dep.Spec.Replicas)

	// 3. Reconciler deployment ready
	rdep, err := client.AppsV1().Deployments(globalWebhookNS).Get(ctx, reconcilerDeployment, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: reconciler deployment not found: %v\n", err)
		os.Exit(1)
	}
	if rdep.Status.ReadyReplicas < 1 {
		fmt.Fprintf(os.Stderr, "FATAL: reconciler deployment has 0 ready replicas\n")
		os.Exit(1)
	}
	fmt.Printf("OK: reconciler deployment ready (%d/%d replicas)\n", rdep.Status.ReadyReplicas, *rdep.Spec.Replicas)

	// 4. MutatingWebhookConfiguration exists
	_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, webhookDeployment, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: MutatingWebhookConfiguration %q not found: %v\n", webhookDeployment, err)
		os.Exit(1)
	}
	fmt.Println("OK: MutatingWebhookConfiguration exists")

	// 5. ResourceSlices exist (DRA drivers are running)
	slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot list ResourceSlices: %v\n", err)
		os.Exit(1)
	}
	if len(slices.Items) == 0 {
		fmt.Fprintf(os.Stderr, "FATAL: no ResourceSlices found — DRA drivers may not be running\n")
		os.Exit(1)
	}
	fmt.Printf("OK: %d ResourceSlices found\n", len(slices.Items))

	// 6. Safety-net: save ConfigMap snapshot
	cm, err := client.CoreV1().ConfigMaps(globalWebhookNS).Get(ctx, globalCMName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read ConfigMap %s/%s: %v\n", globalWebhookNS, globalCMName, err)
		os.Exit(1)
	}
	savedCMData = make(map[string]string)
	for k, v := range cm.Data {
		savedCMData[k] = v
	}
	fmt.Println("OK: ConfigMap snapshot saved")

	code := m.Run()

	// Restore ConfigMap on exit (safety net)
	restoreCM, err := client.CoreV1().ConfigMaps(globalWebhookNS).Get(context.Background(), globalCMName, metav1.GetOptions{})
	if err == nil {
		restoreCM.Data = savedCMData
		_, _ = client.CoreV1().ConfigMaps(globalWebhookNS).Update(context.Background(), restoreCM, metav1.UpdateOptions{})
		fmt.Println("ConfigMap restored to original state")
	} else {
		// ConfigMap may have been deleted by a test — re-create it
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      globalCMName,
				Namespace: globalWebhookNS,
			},
			Data: savedCMData,
		}
		_, err = client.CoreV1().ConfigMaps(globalWebhookNS).Create(context.Background(), newCM, metav1.CreateOptions{})
		if err == nil {
			fmt.Println("ConfigMap re-created from snapshot")
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: failed to restore ConfigMap: %v\n", err)
		}
	}

	os.Exit(code)
}

// TestE2E is the single orchestrator that runs all test categories in order.
func TestE2E(t *testing.T) {
	t.Run("01_WebhookMutation", testWebhookMutation)
	t.Run("02_AllocationVerification", testAllocationVerification)
	t.Run("03_Preflight", testPreflight)
	t.Run("04_Reconciler", testReconciler)
	t.Run("05_EdgeCases", testEdgeCases)
}
