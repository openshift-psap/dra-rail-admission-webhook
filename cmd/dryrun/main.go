package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/llm-d/dra-admission-webhook/internal/dryrun"
	"github.com/llm-d/dra-admission-webhook/internal/webhook"
)

func main() {
	klog.InitFlags(nil)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "capture":
		captureCmd(os.Args[2:])
	case "simulate":
		simulateCmd(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `dryrun — offline validation tool for the DRA GPU-NIC admission webhook

Usage:
  dryrun capture  --kubeconfig <path> --output <file.json>
  dryrun simulate --state <file.json> --config <config.yaml> --count <N>

Subcommands:
  capture    Dump ResourceSlices + Nodes from a live cluster to a JSON file
  simulate   Run the webhook mutation pipeline against captured cluster state

Workflow:
  1. Capture cluster state:
       dryrun capture --kubeconfig ~/.kube/config -o cluster-state.json

  2. Validate a config against the captured state:
       dryrun simulate --state cluster-state.json --config config.yaml --count 2`)
}

func captureCmd(args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	var (
		kubeconfig  string
		output      string
		clusterName string
	)
	fs.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (required)")
	fs.StringVar(&output, "output", "cluster-state.json", "Output file path")
	fs.StringVar(&output, "o", "cluster-state.json", "Output file path (shorthand)")
	fs.StringVar(&clusterName, "cluster-name", "", "Optional cluster name for the capture")
	_ = fs.Parse(args)

	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}
	if kubeconfig == "" {
		fmt.Fprintln(os.Stderr, "Error: --kubeconfig is required (or set KUBECONFIG)")
		os.Exit(1)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building kubeconfig: %v\n", err)
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	state, err := dryrun.CaptureClusterState(ctx, client, clusterName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error capturing cluster state: %v\n", err)
		os.Exit(1)
	}

	if err := state.SaveToFile(output); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving state: %v\n", err)
		os.Exit(1)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Captured %d resource slices and %d nodes to %s\n",
		len(state.ResourceSlices), len(state.Nodes), output)
	if state.ClusterName != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Cluster: %s\n", state.ClusterName)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Captured at: %s\n", state.CapturedAt.Format("2006-01-02 15:04:05 UTC"))
}

func simulateCmd(args []string) {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	var (
		statePath  string
		configPath string
		count      int
		namespace  string
		crossNUMA  bool
	)
	fs.StringVar(&statePath, "state", "", "Path to captured cluster state JSON (required)")
	fs.StringVar(&configPath, "config", "", "Path to webhook config YAML (required)")
	fs.IntVar(&count, "count", 1, "Number of GPU-NIC pairs to request")
	fs.StringVar(&namespace, "namespace", "default", "Namespace for the simulated pod")
	fs.BoolVar(&crossNUMA, "cross-numa", false, "Allow cross-NUMA allocation")
	_ = fs.Parse(args)

	if statePath == "" || configPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --state and --config are required")
		fs.Usage()
		os.Exit(1)
	}
	if count < 1 {
		fmt.Fprintf(os.Stderr, "Error: --count must be at least 1, got %d\n", count)
		os.Exit(1)
	}

	// Load cluster state
	state, err := dryrun.LoadClusterState(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading cluster state: %v\n", err)
		os.Exit(1)
	}

	// Load config
	cfg, err := loadConfigFromFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	req := dryrun.SimulateRequest{
		Config:    cfg,
		State:     state,
		Count:     count,
		Namespace: namespace,
		CrossNUMA: crossNUMA,
	}

	result := dryrun.Simulate(ctx, req)
	dryrun.PrintResult(os.Stdout, result, cfg)

	if result.Error != "" {
		os.Exit(1)
	}
}

func loadConfigFromFile(path string) (webhook.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return webhook.Config{}, fmt.Errorf("failed to read config file: %w", err)
	}
	return webhook.ParseConfig(data)
}
