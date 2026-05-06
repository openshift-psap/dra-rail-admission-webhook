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

This is a Kubernetes mutating admission webhook that converts a synthetic resource request (`dra.llm-d.io/gpu-nic-pair: "N"`) into full Dynamic Resource Allocation (DRA) objects. Two components:

**Webhook** (`cmd/webhook` → `internal/webhook`): HTTPS server on :8443 that intercepts pod creation. The mutation pipeline is:
1. `handler.go` — HTTP handler, decodes AdmissionReview, calls Mutator
2. `validator.go` — Validates count against NUMA/node limits
3. `mutator.go` — Orchestrates mutation: checks idempotency, extracts count, validates, optionally runs preflight, builds claim template, generates JSON patch
4. `claim_builder.go` — Constructs ResourceClaimTemplateSpec with GPU+NIC device requests, PCIe root pairing constraints, NUMA co-location constraints, and NIC driver parameters
5. `preflight.go` — Optional availability check against ResourceSlices (gracefully degrades on error)

**Reconciler** (`cmd/reconciler` → `internal/reconciler`): Background loop (default 5m interval) that detects and cleans up orphaned ResourceClaimTemplates and ResourceClaims. Uses persistent JSON state file at `/data/reconciler-state.json`.

## Key Design Decisions

- **Namespace opt-in**: Only namespaces labeled `dra.llm-d.io/webhook-enabled: "true"` are processed. Unlabeled namespaces bypass the webhook entirely.
- **Template reuse**: Each (count, NUMA mode, rail) tuple gets a deterministic template name including a config hash, so multiple pods can share templates.
- **NUMA modes**: Single-NUMA (default, max 4 pairs) vs cross-NUMA (opt-in via `dra.llm-d.io/allow-cross-numa` annotation, max 8 pairs). Requesting all 8 pairs auto-enables cross-NUMA.
- **Transport detection**: `transportMode: auto` (default) reads `dra.net/encapsulation` from ResourceSlices at startup. Ethernet uses IPv4 prefix matching + `matchAttribute: pcieRoot`. InfiniBand uses PCIe address pinning via `ibRails` config.
- **Config source**: All configuration loaded from a ConfigMap (`deploy/configmap.yaml`), never environment variables. Both webhook and reconciler configs live in the same ConfigMap under different keys (`config.yaml` and `reconciler.yaml`).
- **Idempotency**: Already-mutated pods (with `dra.llm-d.io/mutated` annotation) are skipped.
- **Kustomize overlays**: `deploy/base/` has canonical manifests. Cluster-specific config (images, transport, rails) goes in `deploy/overlays/<cluster>/`.

## Constants (internal/webhook/constants.go)

The synthetic resource name is `dra.llm-d.io/gpu-nic-pair`. Key annotations: `dra.llm-d.io/mutated`, `dra.llm-d.io/allow-cross-numa`, `dra.llm-d.io/orphaned-at`. PCIe affinity uses `resource.kubernetes.io/pcieRoot` (Ethernet) or explicit PCIe address CEL selectors (IB). NUMA uses `dra.net/numaNode`. Transport detection uses `dra.net/encapsulation`.

## Testing

Unit tests use Go standard `testing` package (no frameworks). E2e tests require build tag `e2e` and a running cluster with webhook+reconciler deployed and DRA drivers (gpu.nvidia.com, dranet) installed. E2e TestMain validates cluster readiness before running tests. Set `E2E_DEPLOY_OVERLAY` to auto-deploy from a kustomize overlay before tests run. CI pushes images to GHCR as `pr-<number>` for PR testing.
