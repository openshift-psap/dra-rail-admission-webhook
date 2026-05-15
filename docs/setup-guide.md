# Setup Guide

## Prerequisites

- Kubernetes 1.34.2+ with DRA enabled
- GPU and NIC DRA drivers installed (e.g., `gpu.nvidia.com`, `dranet` v1.2.0+)
- Helm 3.x (for deployment)
- Go 1.25+ (for building from source)

## Build

```bash
make build          # Build bin/webhook, bin/reconciler, bin/dryrun
make docker-build   # Build container images
make test           # Run unit tests
```

## Deployment

### Helm (preferred)

Install with default Ethernet configuration:

```bash
helm install dra charts/dra-admission-webhook/ -n dra-webhook-system --create-namespace
```

This creates all components: webhook, reconciler, ConfigMap, RBAC, PVC, PDB, MutatingWebhookConfiguration, and self-signed TLS certificates. TLS and caBundle are handled automatically.

For cluster-specific configuration, use a values file:

```bash
# AKS NDv5 (InfiniBand transport, reduced MTU)
helm install dra charts/dra-admission-webhook/ \
  -f charts/dra-admission-webhook/values-aks-ndv5.yaml \
  -n dra-webhook-system --create-namespace

# PSAP RDU4 B200 (explicit pairing with PCI device mappings)
helm install dra charts/dra-admission-webhook/ \
  -f charts/dra-admission-webhook/values-psap-rdu4-b200.yaml \
  -n dra-webhook-system --create-namespace
```

Create a new values file by copying an existing one and updating `webhookConfig` with your cluster's PCIe topology and transport settings. See `charts/dra-admission-webhook/values.yaml` for all configurable fields.

#### TLS modes

The Helm chart supports three TLS modes (set via `tls.mode`):

| Mode | Description |
|------|-------------|
| `helm-generated` (default) | Self-signed CA + cert, auto-injected caBundle, persists across upgrades |
| `cert-manager` | Creates a Certificate CR, cainjector handles caBundle |
| `manual` | User creates TLS secret externally, provides `tls.caBundle` in values |

#### Upgrading

```bash
helm upgrade dra charts/dra-admission-webhook/ -n dra-webhook-system
```

ConfigMap changes trigger automatic pod restarts via config hash annotations.

### Namespace labeling

For GPU-NIC pair support, label namespaces that should use the webhook:

```bash
kubectl label namespace my-namespace dra.llm-d.io/webhook-enabled=true
```

### Extended resource interception

To intercept legacy extended resources (e.g., `nvidia.com/gpu`) and convert them to DRA ResourceClaims, enable interception at install or upgrade:

```bash
# At install time
helm install dra charts/dra-admission-webhook/ -n dra-webhook-system --create-namespace \
  --set 'webhookConfig.interceptExtendedResources[0].resourceName=nvidia.com/gpu' \
  --set 'webhookConfig.interceptExtendedResources[0].deviceClassName=gpu.nvidia.com'

# Or via upgrade on an existing release
helm upgrade dra charts/dra-admission-webhook/ -n dra-webhook-system \
  --set 'webhookConfig.interceptExtendedResources[0].resourceName=nvidia.com/gpu' \
  --set 'webhookConfig.interceptExtendedResources[0].deviceClassName=gpu.nvidia.com'
```

Or in a values file:

```yaml
webhookConfig:
  interceptExtendedResources:
    - resourceName: "nvidia.com/gpu"
      deviceClassName: "gpu.nvidia.com"
```

When enabled, the `/mutate-ext` endpoint intercepts pod creates across all non-system namespaces (no labeling required). The webhook strips the extended resource from the container spec and replaces it with a DRA ResourceClaim. Not needed on Kubernetes >= 1.35 with the `DRAExtendedResource` feature gate.

### Kustomize (DEPRECATED)

> **Kustomize deployment is deprecated and will be removed in the next release.** Migrate to the Helm chart above.

<details>
<summary>Legacy kustomize instructions</summary>

```bash
make deploy NAMESPACE=dra-webhook-system
kubectl apply -k deploy/overlays/aks-ndv5/
```

When using kustomize, TLS certificates must be generated manually via `make generate-certs` and the caBundle must be pasted into `deploy/base/webhook-config.yaml`.

</details>

---

## NRI Plugin Timeout

When using DRAnet with multiple RDMA NICs per pod, increase the CRI-O NRI plugin timeout from the default 2 seconds. Without this, CRI-O disconnects the DRAnet NRI plugin during multi-VF operations, causing crashes and NICs left in a down state.

Create on every GPU worker node:

```bash
# /etc/crio/crio.conf.d/10-nri-timeout.conf
[crio.nri]
enable_nri = true
nri_plugin_request_timeout = "60s"
nri_plugin_registration_timeout = "10s"
```

Restart CRI-O (or reboot the node) to apply:

```bash
systemctl restart crio
```

---

## Configuration Changes

The webhook loads its ConfigMap at startup and does not watch for changes.

**Helm (preferred):** Update `webhookConfig` in your values file and run `helm upgrade`. The config hash annotation triggers an automatic pod restart.

```bash
helm upgrade dra charts/dra-admission-webhook/ -n dra-webhook-system -f my-values.yaml
```

**Manual restart (kustomize or direct ConfigMap edits):**

```bash
kubectl rollout restart deployment/dra-gpu-nic-webhook -n dra-webhook-system
```

The deployment uses `strategy: Recreate` to ensure the old pod is terminated before the new one starts, guaranteeing the new config is loaded.

---

## E2E Testing

Run E2E tests against a cluster with DRA drivers installed:

```bash
# Test against an already-deployed webhook
E2E_KUBECONFIG=~/.kube/config \
  go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/

# Deploy from overlay before testing
E2E_KUBECONFIG=~/.kube/config \
E2E_DEPLOY_OVERLAY=deploy/overlays/aks-ndv5 \
  go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/
```

When `E2E_DEPLOY_OVERLAY` is set, `TestMain` runs `kubectl apply -k` on the overlay and waits for rollout before starting tests.

The test suite covers:
1. Webhook mutation (10 tests) — validation, idempotency, NUMA modes
2. Allocation verification (5 tests) — PCIe pairing, NUMA locality, NIC config
3. Preflight (5 tests) — availability checks, resource exhaustion
4. Reconciler (8 tests) — orphan detection, auto-reap, state persistence
5. Edge cases (5 tests) — webhook down, config missing, concurrent requests
6. Interception `/mutate` (6 tests) — enable/disable, mutual exclusivity
7. Interception `/mutate-ext` (5 tests) — unlabeled namespace, gpu-nic-pair ignored

Set `E2E_SKIP_GPU=true` to skip tests that require pods to reach Running state (useful when GPU drivers are not fully configured).
