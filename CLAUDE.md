# CLAUDE.md

## Project Overview

VPSie Cluster Scaler — a CAPI-native cost-optimization autoscaler for VPSie Kubernetes clusters. Manages `ScalingPolicy` CRD (`optimization.vpsie.com/v1alpha1`) targeting MachineDeployments. Two scaling modes: **vertical** (VM plan right-sizing) and **horizontal** (replica count adjustment).

## Build & Development

```bash
go build ./...                    # Build
go test ./... -count=1            # Run unit tests (scheduler, selector, pricing, utilization, vpsie, workload)
make manifests                    # Regenerate CRDs + RBAC after changing types or markers
make generate                     # Regenerate deepcopy after changing API types
make docker-buildx                # Build+push multi-arch image (amd64+arm64)
```

Controller tests require envtest — install with `make envtest` first, then set `KUBEBUILDER_ASSETS`.

## Deploy

```bash
# Build and push with unique tag
docker buildx build --platform linux/amd64,linux/arm64 --push --no-cache -t ghcr.io/vpsieinc/vpsie-cluster-scaler:<tag> .

# Deploy to management cluster
kubectl set image deployment/vpsie-scaler-controller-manager -n vpsie-scaler-system \
  manager=ghcr.io/vpsieinc/vpsie-cluster-scaler:<tag> --kubeconfig /Users/zozo/.kube/config-vpie-beta
```

## Architecture

- **api/v1alpha1/** — ScalingPolicy CRD: targetRef, constraints, aggressiveness, horizontal, dryRun, utilization thresholds
- **internal/controller/** — Reconciler (`reconcileHorizontal` + vertical direction + plan selection), Rebalancer (background loop)
- **internal/pricing/** — Thread-safe cache of VPSie plans, scorer with aggressiveness weights
- **internal/selector/** — Plan selection with ScalingDirection (Up/Down/Any), constraint filtering, fit check, scoring
- **internal/utilization/** — Calculator: pod requests + metrics-server aggregation, asymmetric threshold evaluation
- **internal/scheduler/** — Bin-packing simulator: first-fit-decreasing, taints/tolerations, nodeSelector, nodeAffinity
- **internal/workload/** — CAPI kubeconfig Secret reader, cached workload cluster clients (5m TTL)
- **internal/vpsie/** — PricingClient: GET categories, POST /apps/v2/resources for plans
- **internal/metrics/** — 8 Prometheus metrics (selections, rebalancing, savings, price, utilization, simulations)

## Key Patterns

- **After modifying `api/v1alpha1/` types**: Run `make generate && make manifests`
- **Authentication**: VPSie API uses `Vpsie-Auth` header. Client reads from Secret `data.apiKey`.
- **Horizontal scale-down**: Threshold check + bin-packing simulation on N-1 nodes before reducing replicas
- **Horizontal scale-up**: +1 replica per reconcile when pending pods detected (avoids over-provisioning)
- **CAPI v1beta2 readyReplicas**: Use `md.Status.Deprecated.V1Beta1.ReadyReplicas` — top-level requires all conditions True
- **Category A (Shared CPU)**: Memory ballooning — Talos gets balloon minimum, not advertised RAM. Exclude for Talos clusters.
- **Vertical scaling direction**: Upscale on max(scheduled,actual) > 75% for CPU OR memory. Downscale on min(scheduled,actual) < 5% for BOTH + scheduling sim safe.
- **DryRun mode**: `spec.dryRun: true` logs recommendations without making changes
- **Testability**: Reconciler uses `NewPricingClient` factory field and `WorkloadClients` interface for DI. Tests use fakes.
- **Logging**: Uses `klog` V(2) for normal flow, V(4) for detailed API responses
- **Staging API**: Set `VPSIE_API_URL=https://api2.vpsie.com` env var on the deployment
- **Go version**: 1.24, controller-runtime v0.22.5, CAPI v1.12.3
