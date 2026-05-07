# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make build                          # Build bin/webhook, bin/reconciler, bin/dryrun
make test                           # Run all unit tests
go test -v ./internal/webhook/...   # Run tests for a specific package
go test -v -run TestValidateRequest ./internal/webhook/  # Run a single test
make docker-build                   # Build separate webhook and reconciler images
make deploy NAMESPACE=dra-webhook-system  # Generate TLS certs + deploy to cluster
kubectl apply -k deploy/overlays/aks-ndv5/  # Deploy with cluster-specific overlay

# E2e tests
E2E_KUBECONFIG=~/.kube/config go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/

# E2e with automatic deploy from overlay
E2E_KUBECONFIG=~/.kube/config E2E_DEPLOY_OVERLAY=deploy/overlays/aks-ndv5 \
  go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/
```

## Architecture

This is a Kubernetes mutating admission webhook that converts resource requests into full Dynamic Resource Allocation (DRA) objects. Handles two resource types: `dra.llm-d.io/gpu-nic-pair` (GPU+NIC co-allocation) and opt-in interception of extended resources like `nvidia.com/gpu`. Two components:

**Webhook** (`cmd/webhook` → `internal/webhook`): HTTPS server on :8443 with two endpoints:
- `/mutate` — Full feature set for labeled namespaces (gpu-nic-pair + intercepted resources, mutually exclusive)
- `/mutate-ext` — Extended resource interception only for all non-system namespaces (ignores gpu-nic-pair)

The mutation pipeline:
1. `handler.go` — `Handler` serves `/mutate` (calls `Mutator.Mutate`), `ExtHandler` serves `/mutate-ext` (calls `Mutator.MutateExtOnly`)
2. `validator.go` — Validates gpu-nic-pair count against NUMA/node limits
3. `mutator.go` — Orchestrates mutation: extracts resources (`extractGPUNICPairCount` + `extractInterceptedResources`), enforces mutual exclusivity, allocates, builds claims, generates JSON patch. `MutateExtOnly` is the intercepted-resource-only path.
4. `claim_builder.go` — `BuildSinglePairClaimSpec` / `BuildExplicitPairClaimSpec` for GPU+NIC pairs, `BuildExtendedResourceClaimSpec` for intercepted resources
5. `allocator.go` — `Allocate` / `AllocateExplicit` for GPU-NIC pairs, `AllocateExtendedResource` for intercepted resources
6. `preflight.go` — Optional availability check against ResourceSlices (gracefully degrades on error)
7. `queue.go` — Priority queue for mutation ordering. Pair-bearing pods always prioritized over intercepted-only pods.

**Reconciler** (`cmd/reconciler` → `internal/reconciler`): Background loop (default 5m interval) that detects and cleans up orphaned ResourceClaimTemplates and ResourceClaims. Uses persistent JSON state file at `/data/reconciler-state.json`.

## Key Design Decisions

- **Dual endpoints**: `/mutate` serves labeled namespaces (full feature set). `/mutate-ext` serves all non-system namespaces (intercepted resources only). `/mutate-ext` uses `failurePolicy: Ignore` so pods pass through if the webhook is down.
- **Namespace opt-in for gpu-nic-pair**: Only namespaces labeled `dra.llm-d.io/webhook-enabled: "true"` get gpu-nic-pair processing. Extended resource interception works cluster-wide via `/mutate-ext`.
- **Mutual exclusivity**: A pod cannot request both `gpu-nic-pair` and an intercepted resource — denied with an error. `/mutate-ext` ignores `gpu-nic-pair` entirely.
- **Per-container claim binding**: Intercepted resources are bound per-container (container A requesting 3 GPUs gets 3 claim refs, container B requesting 1 gets 1), not aggregated.
- **Template reuse**: Each (count, NUMA mode, rail) tuple gets a deterministic template name including a config hash, so multiple pods can share templates.
- **NUMA modes**: Single-NUMA (default, max 4 pairs) vs cross-NUMA (opt-in via `dra.llm-d.io/allow-cross-numa` annotation, max 8 pairs). Requesting all 8 pairs auto-enables cross-NUMA.
- **Transport detection**: `transportMode: auto` (default) reads `dra.net/encapsulation` from ResourceSlices at startup. Ethernet uses IPv4 prefix matching + `matchAttribute: pcieRoot`. InfiniBand uses PCIe address pinning via `ibRails` config.
- **Config source**: All configuration loaded from a ConfigMap (`deploy/configmap.yaml`), never environment variables. Both webhook and reconciler configs live in the same ConfigMap under different keys (`config.yaml` and `reconciler.yaml`).
- **Idempotency**: Already-mutated pods (with `dra.llm-d.io/mutated` annotation) are skipped.
- **Kustomize overlays**: `deploy/base/` has canonical manifests. Cluster-specific config (images, transport, rails) goes in `deploy/overlays/<cluster>/`.

## Constants (internal/webhook/constants.go)

The synthetic resource name is `dra.llm-d.io/gpu-nic-pair`. Key annotations: `dra.llm-d.io/mutated`, `dra.llm-d.io/allow-cross-numa`, `dra.llm-d.io/orphaned-at`. PCIe affinity uses `resource.kubernetes.io/pcieRoot` (Ethernet) or explicit PCIe address CEL selectors (IB). NUMA uses `dra.net/numaNode`. Transport detection uses `dra.net/encapsulation`. Extended resource interception is configured via `Config.InterceptExtendedResources` (list of `resourceName` → `deviceClassName` mappings). CEL selectors for ipv4 use `has()` guards to handle devices without the attribute.

## Testing

Unit tests use Go standard `testing` package (no frameworks). E2e tests require build tag `e2e` and a running cluster with webhook+reconciler deployed and DRA drivers (gpu.nvidia.com, dranet) installed. E2e TestMain validates cluster readiness before running tests. Set `E2E_DEPLOY_OVERLAY` to auto-deploy from a kustomize overlay before tests run. CI pushes images to GHCR as `pr-<number>` for PR testing.
