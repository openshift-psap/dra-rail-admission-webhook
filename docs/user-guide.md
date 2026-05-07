# User Guide

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

The webhook mutates each pod at admission time. The synthetic resource is stripped from `requests`/`limits` and replaced with proper DRA references.

---

## Valid Counts

| Count | NUMA Behavior | Notes |
|-------|--------------|-------|
| 1-4   | Single NUMA zone | Pairs are co-located on one NUMA zone (PCIe + NUMA affinity) |
| 5-7   | **Rejected** unless `allow-cross-numa` is set | Exceeds single-NUMA capacity (4 per zone) |
| 8     | Automatic cross-NUMA | Full node allocation, both NUMA zones used |
| >8    | **Rejected** | Exceeds maximum per node |

Defaults: `maxPairsPerNUMA=4`, `maxPairsPerNode=8` (configurable).

---

## Cross-NUMA Annotation

For counts between `maxPairsPerNUMA+1` and `maxPairsPerNode-1` (default: 5-7), explicitly opt in to cross-NUMA allocation:

```yaml
metadata:
  annotations:
    dra.llm-d.io/allow-cross-numa: "true"
```

PCIe affinity between each GPU-NIC pair is still enforced.

---

## What the Webhook Does

When a pod with `dra.llm-d.io/gpu-nic-pair` is created, the webhook:

1. **Validates** the count against `maxPairsPerNUMA` / `maxPairsPerNode` limits
2. **Runs preflight** (if enabled) to check ResourceSlice availability across nodes
3. **Creates a `ResourceClaimTemplate`** (if one doesn't already exist for this count + mode)
4. **Injects `resourceClaims`** into the pod spec referencing that template
5. **Strips** the synthetic resource from container `requests` and `limits`
6. **Pins** the pod to the allocator-selected node via node affinity
7. **Annotates** the pod with `dra.llm-d.io/mutated: "true"`

---

## Configuration Reference

All configuration is loaded from a ConfigMap (`deploy/base/configmap.yaml`). The webhook loads config at startup and requires a restart to pick up changes.

### Ethernet (RoCE)

```yaml
transportMode: auto          # "auto", "ethernet", or "infiniband"
maxPairsPerNUMA: 4
maxPairsPerNode: 8
gpuDeviceClassName: gpu.nvidia.com
nicDeviceClassName: dranet
preflightCheck: false         # opt-in availability check
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
    - subnet: "10.1.0.0/16"
      gateway: "10.1.0.1"
      ipv4Prefix: "10.1."
    # ... one per rail
```

### InfiniBand

The webhook auto-detects InfiniBand transport from `dra.net/encapsulation` attributes at startup. Configure `ibRails` instead of `rails`:

```yaml
transportMode: auto
nicConfig:
  mtu: 2044                  # IPoIB MTU
  rdmaRequired: true
  ibRails:                   # GPU+NIC PCIe address pairs (list index = rail index)
    - gpu: "0001:00:00.0"    # rail 0
      nic: "0101:00:00.0"
    - gpu: "0002:00:00.0"    # rail 1
      nic: "0102:00:00.0"
    # ... one entry per GPU-NIC pair on the node
```

#### How IB mode differs from Ethernet

| Aspect | Ethernet | InfiniBand |
|--------|----------|------------|
| Rail selection | IPv4 prefix match (`ipv4.startsWith`) | PCIe address exact match |
| GPU-NIC pairing | `matchAttribute: pcieRoot` | CEL selector on `pciBusID` + `pciAddress` |
| Routing config | Policy routes + cross-rail gateway | None (IB fabric handles forwarding) |
| Availability detection | `dra.net/ipv4` attribute presence | ResourceClaim inspection |

#### Finding PCIe address pairs

The `ibRails` mapping comes from your VM's PCIe topology. For Azure ND-series, the topology file is at:
- Host path: `/opt/microsoft/ndv5-topo.xml`
- GitHub: [Azure/azhpc-images/topology/ndv5-topo.xml](https://github.com/Azure/azhpc-images/blob/master/topology/ndv5-topo.xml)

Extract pairs from DRA ResourceSlices:

```bash
# GPU PCIe bus IDs
kubectl get resourceslice -o json | jq -r '.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[] | .attributes["resource.kubernetes.io/pciBusID"].string'

# NIC PCI addresses
kubectl get resourceslice -o json | jq -r '.items[] | select(.spec.driver=="dra.net") | .spec.devices[] | select(.attributes["dra.net/rdma"].bool==true) | .attributes["dra.net/pciAddress"].string'
```

### Extended Resource Interception

Intercept standard Kubernetes extended resources (e.g., `nvidia.com/gpu`) and convert them to DRA ResourceClaims. Disabled by default (empty list). Ensures all GPU allocation goes through the webhook's allocator and reconciler.

```yaml
interceptExtendedResources:
  - resourceName: "nvidia.com/gpu"
    deviceClassName: "gpu.nvidia.com"
```

With interception enabled, a pod requesting `nvidia.com/gpu: 2` is mutated the same way as `gpu-nic-pair`: the resource is stripped, ResourceClaims are created, and the pod is pinned to a node.

**Per-container binding**: each container gets only the claim references for the GPUs it requested (container A requesting 3 GPUs gets 3 refs, container B requesting 1 gets 1).

**Mutual exclusivity**: a pod cannot request both `dra.llm-d.io/gpu-nic-pair` and an intercepted resource. Both allocate from the same GPU pool.

**Namespace scope**: interception works via two webhook endpoints:

| Endpoint | Namespace | Use case |
|----------|-----------|----------|
| `/mutate` | Labeled with `dra.llm-d.io/webhook-enabled: "true"` | Full feature set |
| `/mutate-ext` | All except `kube-system`, `openshift-*`, `nvidia-*` | Intercepted resources only |

**When to use**:
- Before Kubernetes 1.35: enable interception to route GPU allocation through DRA
- Kubernetes >= 1.35 with `DRAExtendedResource` feature gate: not needed, remove the config

Multiple resources can be intercepted:

```yaml
interceptExtendedResources:
  - resourceName: "nvidia.com/gpu"
    deviceClassName: "gpu.nvidia.com"
  - resourceName: "amd.com/gpu"
    deviceClassName: "gpu.amd.com"
```

### Advanced Configuration

#### Disable NUMA Packing

By default, the allocator packs small requests onto the most-utilized NUMA zone, keeping the other zone's full capacity available for larger requests:

```yaml
disableNUMAPacking: true
```

#### Explicit Pairing Mode (experimental)

For clusters where automatic rail discovery doesn't work, explicit pairing mode defines exact device-to-device mappings per node pool:

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
```

> **Note:** Explicit pairing mode has known issues being tracked in [#9](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/9), [#10](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/10), [#11](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/11), and [#12](https://github.com/openshift-psap/dra-rail-admission-webhook/issues/12). Use auto mode (with `rails` or `ibRails`) for production.

---

## Quick Reference

| Item | Value |
|------|-------|
| GPU-NIC pair resource | `dra.llm-d.io/gpu-nic-pair` |
| Cross-NUMA annotation | `dra.llm-d.io/allow-cross-numa: "true"` |
| Namespace label (gpu-nic-pair) | `dra.llm-d.io/webhook-enabled: "true"` |
| Mutated marker | `dra.llm-d.io/mutated: "true"` (set by webhook) |
| Default max per NUMA | 4 |
| Default max per node | 8 |
| Interception config | `interceptExtendedResources` (list, empty = disabled) |
| Full endpoint | `/mutate` (labeled namespaces) |
| Extended-only endpoint | `/mutate-ext` (all non-system namespaces) |
