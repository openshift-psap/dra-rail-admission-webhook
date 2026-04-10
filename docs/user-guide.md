# DRA GPU-NIC Admission Webhook — User Guide

This webhook automatically converts a simple resource request into the full DRA (Dynamic Resource Allocation) machinery needed to co-allocate GPU + RDMA NIC pairs with PCIe affinity.

## Prerequisites

Your namespace must have the webhook-enabled label:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: my-namespace
  labels:
    dra.llm-d.io/webhook-enabled: "true"
```

Pods in namespaces without this label are ignored by the webhook.

---

## Requesting GPU-NIC Pairs

Add the synthetic resource `dra.llm-d.io/gpu-nic-pair` to any container in your pod spec. The webhook replaces it with the correct `ResourceClaim`, `ResourceClaimTemplate`, and scheduling constraints.

### Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: inference-worker
spec:
  containers:
  - name: model
    image: my-model:latest
    resources:
      requests:
        dra.llm-d.io/gpu-nic-pair: "2"
      limits:
        dra.llm-d.io/gpu-nic-pair: "2"
```

### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inference
spec:
  replicas: 3
  selector:
    matchLabels:
      app: inference
  template:
    metadata:
      labels:
        app: inference
    spec:
      containers:
      - name: model
        image: my-model:latest
        resources:
          requests:
            dra.llm-d.io/gpu-nic-pair: "4"
          limits:
            dra.llm-d.io/gpu-nic-pair: "4"
```

The webhook mutates each pod at admission time. The synthetic resource is stripped from `requests`/`limits` and replaced with proper DRA references. You never need to write `ResourceClaim` or `ResourceClaimTemplate` objects yourself.

---

## Valid Counts

| Count | NUMA Behavior | Notes |
|-------|--------------|-------|
| 1-4   | Single NUMA zone | Pairs are co-located on one NUMA zone (PCIe + NUMA affinity) |
| 5-7   | **Rejected** unless `allow-cross-numa` is set | Exceeds single-NUMA capacity (4 per zone) |
| 8     | Automatic cross-NUMA | Full node allocation, both NUMA zones used |
| >8    | **Rejected** | Exceeds maximum per node |

Defaults: `maxPairsPerNUMA=4`, `maxPairsPerNode=8` (configurable via the webhook ConfigMap).

---

## Cross-NUMA Annotation

For counts between `maxPairsPerNUMA+1` and `maxPairsPerNode-1` (default: 5-7), you must explicitly opt in to cross-NUMA allocation:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: large-worker
  annotations:
    dra.llm-d.io/allow-cross-numa: "true"
spec:
  containers:
  - name: model
    image: my-model:latest
    resources:
      requests:
        dra.llm-d.io/gpu-nic-pair: "6"
      limits:
        dra.llm-d.io/gpu-nic-pair: "6"
```

This tells the webhook (and the DRA scheduler) that pairs may span both NUMA zones on a node. PCIe affinity between each GPU-NIC pair is still enforced.

---

## What the Webhook Does

When a pod with `dra.llm-d.io/gpu-nic-pair` is created, the webhook:

1. **Validates** the count against `maxPairsPerNUMA` / `maxPairsPerNode` limits
2. **Runs preflight** (if enabled) to check ResourceSlice availability across nodes
3. **Creates a `ResourceClaimTemplate`** (if one doesn't already exist for this count + mode)
4. **Injects `resourceClaims`** into the pod spec referencing that template
5. **Strips** the synthetic resource from container `requests` and `limits`
6. **Annotates** the pod with `dra.llm-d.io/mutated: "true"`

The resulting `ResourceClaim` requests N GPUs and N NICs with `matchAttribute` constraints on `pcieRoot` (and `numaNode` when NUMA-constrained), ensuring hardware affinity.

---

## Quick Reference

| Item | Value |
|------|-------|
| Resource name | `dra.llm-d.io/gpu-nic-pair` |
| Cross-NUMA annotation | `dra.llm-d.io/allow-cross-numa: "true"` |
| Namespace label | `dra.llm-d.io/webhook-enabled: "true"` |
| Mutated marker | `dra.llm-d.io/mutated: "true"` (set by webhook) |
| Default max per NUMA | 4 |
| Default max per node | 8 |
