![Claude Assisted](https://img.shields.io/badge/Made%20with-Claude-8A2BE2?logo=anthropic)
![CI](https://github.com/k-krew/omen/actions/workflows/ci.yml/badge.svg)

# Omen

A lightweight Kubernetes chaos engineering operator with transparent target selection and optional manual approval.

## Overview

Omen lets you declaratively define chaos experiments against your workloads. Each run:

1. Selects a fixed set of target pods (preview)
2. Optionally waits for manual approval
3. Executes the chaos action against those exact targets
4. Records per-target results and a summary

Two CRDs are provided:

- **Experiment** — defines the schedule, target selector, action, safety limits, and approval policy
- **ExperimentRun** — a single execution instance created by the controller, holding the target preview, approval state, and results

## Roadmap

Curious about what's coming next? Check out our [Roadmap](ROADMAP.md) to see our plans for network chaos, advanced target filtering, ChatOps integrations, and more!

## Install via Helm

```bash
helm install omen oci://ghcr.io/k-krew/charts/omen \
  --namespace omen-system \
  --create-namespace \
  --version <version>
```

To customise the installation:

```bash
helm install omen oci://ghcr.io/k-krew/charts/omen \
  --namespace omen-system \
  --create-namespace \
  --version <version> \
  --set manager.leaderElect=true \
  --set resources.limits.memory=256Mi \
  --set manager.webhookTimeout=30s \
  --set manager.protectedNamespaces="{kube-system,omen-system,kube-public,my-critical-ns}"
```

### Controller flags

| Flag | Default | Description |
|---|---|---|
| `--webhook-timeout` | `10s` | Timeout for outgoing approval webhook HTTP requests. Transient failures are retried by the controller with exponential backoff; the run only fails if still undelivered when the approval TTL expires. |
| `--leader-elect` | `false` | Enable leader election for HA deployments. |
| `--metrics-bind-address` | `0` | Address for the metrics endpoint (`0` disables it). |
| `--health-probe-bind-address` | `:8081` | Address for liveness/readiness probes. |
| `--protected-namespaces` | `kube-system,omen-system,kube-public` | Comma-separated list of namespaces that cannot be targeted by any experiment. Enforced at both the validating webhook and target selection time. |

## Safety: Protected Namespaces

Omen enforces a list of protected namespaces that can never be targeted, regardless of what an `Experiment` specifies. The defaults are `kube-system`, `omen-system`, and `kube-public`.

The list is configured via the `--protected-namespaces` flag (comma-separated) and exposed in the Helm chart as `manager.protectedNamespaces`:

```yaml
manager:
  protectedNamespaces:
    - kube-system
    - omen-system
    - kube-public
    - my-critical-namespace
```

Protection is enforced in two places:

- **Validating webhook** — rejects `Experiment` objects whose `spec.selector.namespace` is in the protected list at admission time.
- **Controller** — filters out any pods in protected namespaces during target selection, even for cluster-scoped selectors.

## Run locally (against Kind or Minikube)

### Prerequisites

- Go 1.26+
- `kubebuilder` v4
- `kubectl` pointing at a local cluster

```bash
# Install CRDs
GOTOOLCHAIN=local make install

# Run the controller locally (uses ~/.kube/config)
GOTOOLCHAIN=local make run
```

The controller reads `POD_NAMESPACE` to exclude its own pods from target selection. Set it when running locally:

```bash
POD_NAMESPACE=omen-system GOTOOLCHAIN=local make run
```

## Example Experiments

### One-shot, no approval, fixed count

```yaml
apiVersion: chaos.kreicer.dev/v1alpha1
kind: Experiment
metadata:
  name: kill-one-pod
  namespace: default
spec:
  runPolicy:
    type: Once
  selector:
    namespace: default
    labels:
      app: my-app
  mode:
    type: random
    count: 1
  action:
    type: delete_pod
  safety:
    maxTargets: 1
```

### One-shot, percentage-based target selection

```yaml
apiVersion: chaos.kreicer.dev/v1alpha1
kind: Experiment
metadata:
  name: kill-third-of-fleet
  namespace: default
spec:
  runPolicy:
    type: Once
  selector:
    namespace: default
    labels:
      app: my-app
  mode:
    type: random
    percent: 33   # kill ~33% of matching pods, minimum 1
  action:
    type: delete_pod
  safety:
    maxTargets: 5
```

`percent` is mutually exclusive with `count`. The calculated pod count is always rounded up and floored at 1, so the experiment always has an effect even against small replica sets. `safety.maxTargets` is applied as a hard cap after the percentage is resolved.

### Recurring, with approval

```yaml
apiVersion: chaos.kreicer.dev/v1alpha1
kind: Experiment
metadata:
  name: weekly-chaos
  namespace: default
spec:
  runPolicy:
    type: Repeat
    schedule: "0 10 * * 1"   # every Monday at 10:00
    cooldown: 24h
    concurrencyPolicy: Forbid
  selector:
    namespace: staging
    labels:
      app: api-server
  mode:
    type: random
    count: 2
  action:
    type: delete_pod
  approval:
    required: true
    ttl: 30m
    webhook:
      url: https://hooks.example.com/omen-approval
  safety:
    maxTargets: 2
```

To approve the run, patch the generated `ExperimentRun`:

```bash
kubectl patch experimentrun <run-name> \
  --type=merge \
  -p '{"spec":{"approved":true}}'
```

### Dry run

Set `dryRun: true` on the `Experiment` to preview target selection without executing the action. Targets are recorded in `ExperimentRun.status.previewTargets` and results are marked `Success` without any pods being deleted.

## Observability

Every phase transition of an `ExperimentRun` emits a standard Kubernetes Event on the object. Use `kubectl describe` to follow the lifecycle:

```bash
kubectl describe experimentrun <run-name>
```

Events use `Normal` type for successful transitions (`PreviewGenerated`, `Approved`, `Running`, `Completed`) and `Warning` for failure states (`Failed`, `Expired`).

## Safe Deletion

`Experiment` objects carry a finalizer (`chaos.omen.com/finalizer`). When an `Experiment` is deleted, the controller first deletes all owned `ExperimentRun`s and waits for them to be removed before releasing the finalizer. This prevents orphaned runs from executing chaos actions after the parent is gone.

## Development

```bash
# Regenerate CRDs and RBAC after editing types
GOTOOLCHAIN=local make manifests generate

# Build the binary
GOTOOLCHAIN=local make build

# Run tests (requires setup-envtest)
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS=$(setup-envtest use --print path)
go test ./... -v
```
