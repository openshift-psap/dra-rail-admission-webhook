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

On **Ethernet** clusters, the resulting `ResourceClaim` uses `matchAttribute` constraints on `pcieRoot` and `numaNode` for hardware affinity. On **InfiniBand** clusters, GPU and NIC are pinned by exact PCIe address from the configured `ibRails` mapping.

---

## InfiniBand Support

The webhook auto-detects InfiniBand transport from `dra.net/encapsulation` attributes at startup. On IB clusters, configure `ibRails` instead of `rails` in the ConfigMap:

```yaml
transportMode: auto          # auto-detects from ResourceSlice attributes
nicConfig:
  mtu: 2044                  # IPoIB MTU
  rdmaRequired: true
  ibRails:                   # GPU+NIC PCIe address pairs (list index = rail index)
    - gpu: "0001:00:00.0"    # rail 0, NUMA 0
      nic: "0101:00:00.0"
    - gpu: "0002:00:00.0"    # rail 1
      nic: "0102:00:00.0"
    # ... one entry per GPU-NIC pair on the node
```

### How IB mode differs from Ethernet

| Aspect | Ethernet | InfiniBand |
|--------|----------|------------|
| Rail selection | IPv4 prefix match (`ipv4.startsWith`) | PCIe address exact match |
| GPU-NIC pairing | `matchAttribute: pcieRoot` | CEL selector on `pciBusID` + `pciAddress` |
| Routing config | Policy routes + cross-rail gateway | None (IB fabric handles forwarding) |
| Availability detection | `dra.net/ipv4` attribute presence | ResourceClaim inspection |

### Finding PCIe address pairs

The `ibRails` mapping comes from your VM's PCIe topology. For Azure ND-series, the topology file is at:
- Host path: `/opt/microsoft/ndv5-topo.xml`
- GitHub: [Azure/azhpc-images/topology/ndv5-topo.xml](https://github.com/Azure/azhpc-images/blob/master/topology/ndv5-topo.xml)

Each `<pci>` bridge in the topology contains one GPU (class `0x030200`) and one NIC (class `0x020700`) — these form a pair.

You can also extract pairs from DRA ResourceSlices:

```bash
# GPU PCIe bus IDs
kubectl get resourceslice -o json | jq -r '.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[] | .attributes["resource.kubernetes.io/pciBusID"].string'

# NIC PCI addresses
kubectl get resourceslice -o json | jq -r '.items[] | select(.spec.driver=="dra.net") | .spec.devices[] | select(.attributes["dra.net/rdma"].bool==true) | .attributes["dra.net/pciAddress"].string'
```

---

## Kustomize Overlays

The `deploy/base/` directory contains canonical manifests with Ethernet defaults. For cluster-specific configuration (images, transport, rails), use a kustomize overlay:

```
deploy/overlays/aks-ndv5/
  kustomization.yaml        # References ../../base
  webhook-patch.yaml        # Image tag, replicas
  reconciler-patch.yaml     # Image tag
  configmap-patch.yaml      # IB transport, ibRails, MTU
```

Deploy:

```bash
kubectl apply -k deploy/overlays/aks-ndv5/
```

Create a new overlay by copying an existing one and updating `configmap-patch.yaml` with your cluster's topology.

---

## E2E Testing

Run e2e tests against a cluster with DRA drivers installed:

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

---

## Advanced Configuration

### Disable NUMA Packing

By default, the allocator packs small requests onto the most-utilized NUMA zone, keeping the other zone's full capacity available for larger requests. To disable this heuristic:

```yaml
disableNUMAPacking: true
```

When disabled, the allocator does not prefer specific NUMA zones for small requests.

### Explicit Pairing Mode (experimental)

For clusters where automatic rail discovery doesn't work, explicit pairing mode lets admins define exact device-to-device mappings per node pool:

```yaml
pairingMode: explicit
pairingConfig:
  nodePoolLabelKey: "node.kubernetes.io/instance-type"
  deviceSelectors:
    gpu:
      deviceClassName: gpu.nvidia.com
      driver: gpu.nvidia.com
      attributeDomain: "resource.kubernetes.io"
      attributeName: "pciBusID"
    nic:
      deviceClassName: dranet
      driver: dra.net
      attributeDomain: "dra.net"
      attributeName: "ifName"
  nodePools:
    - nodePoolLabel: "gpu-h100"
      pairs:
        - devices: { gpu: "GPU-UUID-1", nic: "net0" }
          rail: 0
          numa: 0
        - devices: { gpu: "GPU-UUID-2", nic: "net1" }
          rail: 1
          numa: 0
        # ... one entry per GPU-NIC pair
```

> **Note:** Explicit pairing mode has known issues being tracked in [#9](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/9), [#10](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/10), [#11](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/11), and [#12](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/12). Use auto mode (with `rails` or `ibRails`) for production deployments.

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
