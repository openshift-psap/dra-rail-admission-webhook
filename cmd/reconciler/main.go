package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/llm-d/dra-admission-webhook/internal/reconciler"
)

func main() {
	klog.InitFlags(nil)

	var (
		configNamespace string
		configName      string
		healthPort      int
	)

	flag.StringVar(&configNamespace, "config-namespace", "dra-webhook-system", "Namespace of the ConfigMap")
	flag.StringVar(&configName, "config-name", "dra-gpu-nic-webhook-config", "Name of the ConfigMap")
	flag.IntVar(&healthPort, "health-port", 8080, "Port for health check endpoints")
	flag.Parse()

	// Create in-cluster Kubernetes client
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to create in-cluster config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Load config
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := reconciler.LoadReconcilerConfig(ctx, kubeClient, configNamespace, configName)
	if err != nil {
		klog.Infof("Failed to load reconciler config, using defaults: %v", err)
		cfg = reconciler.DefaultReconcilerConfig()
	}

	klog.InfoS("Loaded reconciler configuration",
		"interval", cfg.Interval,
		"autoReap", cfg.AutoReap,
		"gracePeriod", cfg.GracePeriod,
		"statePath", cfg.StatePath)

	// Initialize state manager
	stateMgr, err := reconciler.NewStateManager(cfg.StatePath)
	if err != nil {
		klog.Fatalf("Failed to initialize state manager: %v", err)
	}

	// Create and start reconciler
	rec := &reconciler.Reconciler{
		KubeClient: kubeClient,
		State:      stateMgr,
		Config:     cfg,
	}

	// Health check server
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	go func() {
		klog.InfoS("Starting health check server", "port", healthPort)
		if err := http.ListenAndServe(fmt.Sprintf(":%d", healthPort), mux); err != nil {
			klog.Fatalf("Health check server failed: %v", err)
		}
	}()

	// Handle graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		klog.InfoS("Received shutdown signal")
		cancel()
	}()

	rec.Run(ctx)
}
