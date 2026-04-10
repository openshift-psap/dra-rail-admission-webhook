//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	resourceclient "k8s.io/client-go/kubernetes/typed/resource/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultKubeconfig  = "/home/thibrahi/kubeconfigs/kubeconfig_files/ibmcluster"
	defaultWebhookNS   = "dra-webhook-system"
	defaultConfigMap   = "dra-gpu-nic-webhook-config"
	webhookDeployment  = "dra-gpu-nic-webhook"
	reconcilerDeployment = "dra-gpu-nic-reconciler"
)

// Framework provides test-scoped Kubernetes clients and namespace lifecycle.
type Framework struct {
	KubeClient     kubernetes.Interface
	ResourceClient resourceclient.ResourceV1Interface
	RestConfig     *rest.Config
	Namespace      string
	WebhookNS      string
	ConfigMapName  string
}

// NewFramework creates a Framework with a fresh namespace labeled for the webhook.
// The namespace is deleted via t.Cleanup when the test finishes.
func NewFramework(t *testing.T, prefix string) *Framework {
	t.Helper()

	cfg := buildRestConfig(t)
	client := kubernetes.NewForConfigOrDie(cfg)

	ns := fmt.Sprintf("e2e-%s-%s", prefix, randomSuffix())

	// Create namespace with webhook-enabled label
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"dra.llm-d.io/webhook-enabled": "true",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace %s: %v", ns, err)
	}

	// Register namespace cleanup (runs last due to LIFO)
	t.Cleanup(func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})

	t.Logf("Created test namespace: %s", ns)

	webhookNS := envOrDefault("E2E_WEBHOOK_NS", defaultWebhookNS)
	configMapName := envOrDefault("E2E_CONFIGMAP", defaultConfigMap)

	return &Framework{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		RestConfig:     cfg,
		Namespace:      ns,
		WebhookNS:      webhookNS,
		ConfigMapName:  configMapName,
	}
}

// NewFrameworkWithoutNamespace creates a Framework without creating a namespace.
// Used for edge case tests that manage namespaces manually.
func NewFrameworkWithoutNamespace(t *testing.T) *Framework {
	t.Helper()

	cfg := buildRestConfig(t)
	client := kubernetes.NewForConfigOrDie(cfg)

	webhookNS := envOrDefault("E2E_WEBHOOK_NS", defaultWebhookNS)
	configMapName := envOrDefault("E2E_CONFIGMAP", defaultConfigMap)

	return &Framework{
		KubeClient:     client,
		ResourceClient: client.ResourceV1(),
		RestConfig:     cfg,
		WebhookNS:      webhookNS,
		ConfigMapName:  configMapName,
	}
}

func buildRestConfig(t *testing.T) *rest.Config {
	t.Helper()
	kubeconfig := envOrDefault("E2E_KUBECONFIG", defaultKubeconfig)
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("failed to build kubeconfig from %s: %v", kubeconfig, err)
	}
	return cfg
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func randomSuffix() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}
