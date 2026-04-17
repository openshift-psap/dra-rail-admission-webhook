package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

func main() {
	klog.InitFlags(nil)

	var (
		port            int
		certDir         string
		certFile        string
		keyFile         string
		configNamespace string
		configName      string
	)

	flag.IntVar(&port, "port", 8443, "Webhook server port")
	flag.StringVar(&certDir, "cert-dir", "/certs", "Directory containing TLS certificates")
	flag.StringVar(&certFile, "tls-cert-file", "tls.crt", "TLS certificate file name (relative to cert-dir)")
	flag.StringVar(&keyFile, "tls-key-file", "tls.key", "TLS key file name (relative to cert-dir)")
	flag.StringVar(&configNamespace, "config-namespace", "dra-webhook-system", "Namespace of the webhook ConfigMap")
	flag.StringVar(&configName, "config-name", "dra-gpu-nic-webhook-config", "Name of the webhook ConfigMap")
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

	// Load config from ConfigMap
	ctx := context.Background()
	cfg, err := webhook.LoadConfigFromConfigMap(ctx, kubeClient, configNamespace, configName)
	if err != nil {
		klog.Warningf("Failed to load config from ConfigMap, using defaults: %v", err)
		cfg = webhook.DefaultConfig()
	}

	klog.InfoS("Loaded configuration",
		"maxPairsPerNUMA", cfg.MaxPairsPerNUMA,
		"maxPairsPerNode", cfg.MaxPairsPerNode,
		"gpuDeviceClass", cfg.GPUDeviceClassName,
		"nicDeviceClass", cfg.NICDeviceClassName,
		"nicMTU", cfg.NICConfig.MTU,
	)

	// Create cluster-level allocator
	allocator := webhook.NewAllocator(kubeClient.ResourceV1(), kubeClient, cfg)

	// Create mutator
	mutator := &webhook.Mutator{
		KubeClient:     kubeClient,
		ResourceClient: kubeClient.ResourceV1(),
		Config:         cfg,
		Allocator:      allocator,
	}

	// Create priority queue for rail-aware mutation ordering.
	// The 3s debounce collects pod creations from deployment rollouts
	// so that larger requests (more GPU-NIC pairs) get first pick of
	// rails and anti-affinity is correctly evaluated across the batch.
	queue := webhook.NewMutationQueue(mutator, 3*time.Second)

	// Create handler
	handler := &webhook.Handler{
		Mutator: mutator,
		Queue:   queue,
	}

	// Set up HTTP server
	mux := http.NewServeMux()
	mux.Handle(webhook.MutatePath, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	certPath := fmt.Sprintf("%s/%s", certDir, certFile)
	keyPath := fmt.Sprintf("%s/%s", certDir, keyFile)

	tlsCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		klog.Fatalf("Failed to load TLS certificate: %v", err)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Handle graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		klog.InfoS("Starting webhook server", "port", port)
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-stop
	klog.InfoS("Shutting down webhook server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		klog.ErrorS(err, "Server shutdown error")
	}
}
