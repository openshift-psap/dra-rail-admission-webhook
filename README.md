# DRA GPU-NIC Admission Webhook

A Kubernetes mutating admission webhook that converts a simple resource request (`dra.llm-d.io/gpu-nic-pair: "N"`) into full [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) objects, co-allocating GPU + RDMA NIC pairs with PCIe affinity.

Supports both **Ethernet (RoCE)** and **InfiniBand** fabrics with automatic transport detection.

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
2. **Creates** `ResourceClaimTemplate` objects with GPU + NIC device requests and topology constraints
3. **Injects** `resourceClaims` into the pod spec referencing the templates
4. **Strips** the synthetic resource from `requests`/`limits`

## Components

| Component | Description |
|-----------|-------------|
| **Webhook** (`cmd/webhook`) | Mutating admission webhook server |
| **Reconciler** (`cmd/reconciler`) | Detects and cleans up orphaned `ResourceClaimTemplate` objects |
| **Dryrun** (`cmd/dryrun`) | Offline cluster state capture and allocation simulation |

## Prerequisites

- Kubernetes 1.34.2+ with DRA enabled
- GPU and NIC DRA drivers installed (e.g., `gpu.nvidia.com`, `dranet` v1.2.0+)
- `cert-manager` or manually generated TLS certificates

## Quick Start

```bash
# Build
make build

# Generate TLS certs and deploy (base Ethernet config)
make deploy NAMESPACE=dra-webhook-system

# Deploy with a cluster-specific overlay (e.g., AKS InfiniBand)
kubectl apply -k deploy/overlays/aks-ndv5/

# Run unit tests
make test

# Run e2e tests (requires a cluster with DRA-capable GPU nodes)
E2E_KUBECONFIG=~/.kube/config go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/

# Run e2e with automatic deploy from overlay
E2E_KUBECONFIG=~/.kube/config E2E_DEPLOY_OVERLAY=deploy/overlays/aks-ndv5 \
  go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/
```

## Configuration

The webhook is configured via a `ConfigMap` (`deploy/configmap.yaml`). See [docs/user-guide.md](docs/user-guide.md) for detailed options.

### Ethernet (default)

```yaml
transportMode: auto          # "auto", "ethernet", or "infiniband"
maxPairsPerNUMA: 4
maxPairsPerNode: 8
gpuDeviceClassName: gpu.nvidia.com
nicDeviceClassName: dranet
nicConfig:
  mtu: 9000
  rdmaRequired: true
  interfacePrefix: "net"
  startingTableId: 100
  crossRailCIDR: "10.0.0.0/13"
  rails:
    - subnet: "10.0.0.0/16"
      gateway: "10.0.0.1"
      ipv4Prefix: "10.0."
    # ... one per rail
```

### InfiniBand

On IB fabrics, the webhook auto-detects the transport from `dra.net/encapsulation` attributes at startup. Rails are defined as GPU+NIC PCIe address pairs:

```yaml
transportMode: auto
nicConfig:
  mtu: 2044
  rdmaRequired: true
  ibRails:
    - gpu: "0001:00:00.0"
      nic: "0101:00:00.0"
    - gpu: "0002:00:00.0"
      nic: "0102:00:00.0"
    # ... one per rail, list index = rail index
```

IB mode differences:
- GPU pinned by `pciBusID`, NIC pinned by `pciAddress` (no `matchAttribute: pcieRoot` needed)
- No L3 policy routing rules generated (IB fabric handles forwarding)
- Device availability tracked via ResourceClaim inspection (DRAnet doesn't strip attributes on IB allocation)

## Deployment

### Base manifests

`deploy/` and `deploy/base/` contain canonical manifests with Ethernet defaults:

```bash
make deploy NAMESPACE=dra-webhook-system
```

### Cluster-specific overlays

Use kustomize overlays for cluster-specific config (images, transport, rails, replicas):

```text
deploy/
  base/                    # Canonical manifests
  overlays/
    aks-ndv5/              # AKS ND96isr_H100_v5 (InfiniBand)
      kustomization.yaml
      webhook-patch.yaml   # Image + replicas
      reconciler-patch.yaml
      configmap-patch.yaml # IB transport + ibRails
```

Deploy an overlay:

```bash
kubectl apply -k deploy/overlays/aks-ndv5/
```

Create new overlays by copying `aks-ndv5/` and updating the `configmap-patch.yaml` with your cluster's PCIe topology and transport settings.

## Namespace Opt-In

Only namespaces with the label `dra.llm-d.io/webhook-enabled: "true"` are processed. Pods in unlabeled namespaces pass through unchanged.

## CI

CI builds and pushes images to GHCR on every PR:
- `ghcr.io/openshift-psap/dra-rail-admission-webhook/webhook:pr-<number>`
- `ghcr.io/openshift-psap/dra-rail-admission-webhook/reconciler:pr-<number>`

Main branch pushes are tagged with the commit SHA. Tagged releases use semver.

## Project Layout

```text
cmd/
  webhook/          Webhook server entrypoint
  reconciler/       Reconciler entrypoint
  dryrun/           Offline simulation tool
internal/
  webhook/          Core logic: config, validation, mutation, preflight, claim building
  reconciler/       Orphan detection and cleanup
  dryrun/           Cluster state capture and allocation simulation
deploy/
  base/             Canonical Kustomize manifests
  overlays/         Cluster-specific overlays (AKS, etc.)
test/e2e/           End-to-end test suite
docs/               User-facing documentation
```

## Documentation

See [docs/user-guide.md](docs/user-guide.md) for detailed usage, valid counts, cross-NUMA allocation, and InfiniBand configuration.

## Acknowledgments

This project was written with [Claude Opus 4.6](https://www.anthropic.com/claude).

## License

Apache-2.0. See [LICENSE](LICENSE) for details.
