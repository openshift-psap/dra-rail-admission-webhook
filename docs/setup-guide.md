# Setup Guide

## Prerequisites

- Kubernetes 1.34.2+ with DRA enabled
- GPU and NIC DRA drivers installed (e.g., `gpu.nvidia.com`, `dranet` v1.2.0+)
- `cert-manager` or manually generated TLS certificates
- Go 1.25+ (for building from source)

## Build

```bash
make build          # Build bin/webhook, bin/reconciler, bin/dryrun
make docker-build   # Build container images
make test           # Run unit tests
```

## Deployment

### Base deployment (Ethernet defaults)

Generate TLS certificates and deploy all components:

```bash
make deploy NAMESPACE=dra-webhook-system
```

This creates:
- TLS certificates (self-signed CA)
- `MutatingWebhookConfiguration` with both `/mutate` and `/mutate-ext` endpoints
- Webhook deployment (Recreate strategy)
- Reconciler deployment
- ConfigMap with default Ethernet configuration
- RBAC (ClusterRole, ClusterRoleBinding, ServiceAccount)

### Kustomize overlays

For cluster-specific configuration (images, transport, rails, replicas), use a kustomize overlay:

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

Create a new overlay by copying an existing one and updating `configmap-patch.yaml` with your cluster's PCIe topology and transport settings.

### Namespace labeling

For GPU-NIC pair support, label namespaces that should use the webhook:

```bash
kubectl label namespace my-namespace dra.llm-d.io/webhook-enabled=true
```

Extended resource interception via `/mutate-ext` works on all non-system namespaces without labeling.

### caBundle per cluster

The `MutatingWebhookConfiguration` contains a `caBundle` field that must match the TLS certificate used by the webhook. When deploying to a new cluster:

1. `make deploy` generates cluster-specific certs and sets the caBundle automatically
2. If applying base manifests manually, update `caBundle` in `deploy/base/webhook-config.yaml` to match your cluster's TLS secret:

```bash
CABUNDLE=$(kubectl get secret dra-gpu-nic-webhook-tls -n dra-webhook-system -o jsonpath='{.data.tls\.crt}')
kubectl patch mutatingwebhookconfiguration dra-gpu-nic-webhook --type=json -p "[
  {\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"$CABUNDLE\"},
  {\"op\":\"replace\",\"path\":\"/webhooks/1/clientConfig/caBundle\",\"value\":\"$CABUNDLE\"}
]"
```

Both webhook entries (`/mutate` and `/mutate-ext`) must use the same caBundle.

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

The webhook loads its ConfigMap at startup and does not watch for changes. After updating the ConfigMap, restart the webhook:

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
