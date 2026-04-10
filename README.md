# DRA GPU-NIC Admission Webhook

A Kubernetes mutating admission webhook that converts a simple resource request (`dra.llm-d.io/gpu-nic-pair: "N"`) into full [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) objects, co-allocating GPU + RDMA NIC pairs with PCIe affinity.

## Overview

Writing DRA `ResourceClaim` and `ResourceClaimTemplate` objects by hand is complex and error-prone. This webhook lets users request GPU-NIC pairs with a single line in their pod spec:

```yaml
resources:
  requests:
    dra.llm-d.io/gpu-nic-pair: "2"
  limits:
    dra.llm-d.io/gpu-nic-pair: "2"
```

The webhook intercepts pod creation and:

1. **Validates** the requested count against NUMA/node limits
2. **Creates** a `ResourceClaimTemplate` with GPU + NIC device requests and `matchAttribute` constraints (PCIe root, NUMA node)
3. **Injects** `resourceClaims` into the pod spec referencing the template
4. **Strips** the synthetic resource from `requests`/`limits`

## Components

| Component | Description |
|-----------|-------------|
| **Webhook** (`cmd/webhook`) | Mutating admission webhook server |
| **Reconciler** (`cmd/reconciler`) | Detects and cleans up orphaned `ResourceClaimTemplate` objects |

## Prerequisites

- Kubernetes 1.32+ with DRA enabled
- GPU and NIC DRA drivers installed (e.g., `gpu.nvidia.com`, `dranet` v1.1.0 tested)
- `cert-manager` or manually generated TLS certificates

## Quick Start

```bash
# Build
make build

# Generate TLS certs and deploy
make deploy NAMESPACE=dra-webhook-system

# Run unit tests
make test

# Run e2e tests (requires a cluster with DRA-capable GPU nodes)
make e2e E2E_KUBECONFIG=~/.kube/config
```

## Configuration

The webhook is configured via a `ConfigMap` (`deploy/configmap.yaml`):

```yaml
maxPairsPerNUMA: 4          # Max GPU-NIC pairs per NUMA zone
maxPairsPerNode: 8          # Max GPU-NIC pairs per node
gpuDeviceClassName: gpu.nvidia.com
nicDeviceClassName: dranet
nicConfig:
  mtu: 9000
  rdmaRequired: true
  interfacePrefix: "net"
  startingTableId: 100
preflightCheck: false        # Check ResourceSlice availability before admitting
```

## Namespace Opt-In

Only namespaces with the label `dra.llm-d.io/webhook-enabled: "true"` are processed. Pods in unlabeled namespaces pass through unchanged.

## Project Layout

```
cmd/
  webhook/          Webhook server entrypoint
  reconciler/       Reconciler entrypoint
internal/
  webhook/          Core logic: config, validation, mutation, preflight, claim building
  reconciler/       Orphan detection and cleanup
deploy/             Kustomize manifests (RBAC, deployments, service, configmap, PDB)
test/e2e/           End-to-end test suite
docs/               User-facing documentation
```

## Documentation

See [docs/user-guide.md](docs/user-guide.md) for detailed usage, valid counts, and cross-NUMA allocation.

## Acknowledgments

This project was written with [Claude Opus 4.6](https://www.anthropic.com/claude).

## License

TBD
