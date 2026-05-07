# DRA GPU-NIC Admission Webhook

A Kubernetes mutating admission webhook that converts resource requests into full [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) objects, ensuring all GPU and NIC allocation is managed by a single system.

Supports both **Ethernet (RoCE)** and **InfiniBand** fabrics with automatic transport detection.

## Why

Writing DRA `ResourceClaim` and `ResourceClaimTemplate` objects by hand is complex and error-prone. Multiple users on the same cluster can step on each other's GPU and NIC allocations when resources are managed by different systems (device plugin vs DRA). This webhook provides a single point of control for all resource allocation.

## What It Does

The webhook handles two resource types:

**GPU-NIC Pairs** — request co-allocated GPU + RDMA NIC pairs with PCIe affinity:

```yaml
resources:
  requests:
    dra.llm-d.io/gpu-nic-pair: "2"
  limits:
    dra.llm-d.io/gpu-nic-pair: "2"
```

**Extended Resource Interception** (opt-in) — intercept standard Kubernetes extended resources like `nvidia.com/gpu` and convert them to DRA ResourceClaims:

```yaml
resources:
  requests:
    nvidia.com/gpu: "2"
  limits:
    nvidia.com/gpu: "2"
```

On pod creation, the webhook:

1. **Validates** the requested count against limits
2. **Allocates** devices on a specific node (NUMA-aware packing)
3. **Creates** `ResourceClaimTemplate` objects with device requests and topology constraints
4. **Injects** `resourceClaims` into the pod spec
5. **Strips** the original resource from `requests`/`limits`
6. **Pins** the pod to the selected node

## Webhook Endpoints

| Endpoint | Namespace Scope | Processes | Failure Policy |
|----------|----------------|-----------|---------------|
| `/mutate` | `dra.llm-d.io/webhook-enabled: "true"` | gpu-nic-pair + intercepted resources (mutually exclusive) | Fail |
| `/mutate-ext` | All except `kube-system`, `openshift-*`, `nvidia-*` | Intercepted resources only, ignores gpu-nic-pair | Ignore |

## Components

| Component | Description |
|-----------|-------------|
| **Webhook** (`cmd/webhook`) | Mutating admission webhook server |
| **Reconciler** (`cmd/reconciler`) | Detects and cleans up orphaned `ResourceClaimTemplate` objects |
| **Dryrun** (`cmd/dryrun`) | Offline cluster state capture and allocation simulation |

## Quick Start

```bash
make build                               # Build all binaries
make deploy NAMESPACE=dra-webhook-system  # Generate TLS certs + deploy
make test                                # Run unit tests
```

See [docs/setup-guide.md](docs/setup-guide.md) for full setup instructions, kustomize overlays, and E2E testing.

## Documentation

- [User Guide](docs/user-guide.md) — configuration reference, resource types, valid counts, transport modes, interception
- [Setup Guide](docs/setup-guide.md) — prerequisites, deployment, overlays, testing, NRI configuration

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

## CI

CI builds and pushes images to GHCR on every PR:
- `ghcr.io/openshift-psap/dra-rail-admission-webhook/webhook:pr-<number>`
- `ghcr.io/openshift-psap/dra-rail-admission-webhook/reconciler:pr-<number>`

Main branch pushes are tagged with the commit SHA. Tagged releases use semver.

## Acknowledgments

This project was written with [Claude Opus 4.6](https://www.anthropic.com/claude).

## License

Apache-2.0. See [LICENSE](LICENSE) for details.
