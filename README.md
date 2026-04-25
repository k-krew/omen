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

Curious about what's coming next? Check out our [Roadmap](ROADMAP.md) to see our plans for advanced target filtering, ChatOps integrations, and more!

## Breaking Changes in v0.3.0

Version 0.3.0 introduces a major architectural shift to an **opt-in sidecar model** for network chaos, replacing the old ephemeral containers approach.

- **Namespace Opt-in Required:** You must explicitly label target namespaces with `chaos.kreicer.dev/enabled=true`.
- **Sidecar Injection:** A mutating webhook now automatically injects the `omen-agent` sidecar into pods in enabled namespaces.
- **Removed Flags:** The `ProtectedNamespaces` CLI flag and Helm value have been completely removed in favor of the new opt-in label system.

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
  --set manager.agentImage="ghcr.io/k-krew/omen-agent:<version>" \
  --set manager.agentPort=9999
```

### Controller flags

| Flag | Default | Description |
|---|---|---|
| `--webhook-timeout` | `10s` | Timeout for outgoing approval webhook HTTP requests. |
| `--leader-elect` | `false` | Enable leader election for HA deployments. |
| `--metrics-bind-address` | `0` | Address for the metrics endpoint (`0` disables it). |
| `--health-probe-bind-address` | `:8081` | Address for liveness/readiness probes. |
| `--agent-image` | `ghcr.io/k-krew/omen-agent:v0.3.1` | Container image injected as the `omen-agent` sidecar into target pods. |
| `--agent-port` | `9999` | Port the agent sidecar listens on. Change if it conflicts with application ports. |

## Examples

Ready-to-apply YAML manifests live in the [`examples/`](examples/) directory:

| File | Description |
|---|---|
| [`delete-pod-once.yaml`](examples/delete-pod-once.yaml) | One-shot pod deletion, fixed count |
| [`delete-pod-percent.yaml`](examples/delete-pod-percent.yaml) | One-shot pod deletion, percentage-based |
| [`delete-pod-repeat-approval.yaml`](examples/delete-pod-repeat-approval.yaml) | Recurring deletion with manual approval and webhook notification |
| [`network-fault-latency.yaml`](examples/network-fault-latency.yaml) | Inject 100ms latency + 10ms jitter for 5 minutes |
| [`network-fault-packet-loss.yaml`](examples/network-fault-packet-loss.yaml) | Drop 30% of packets for 3 minutes |
| [`network-fault-blackhole.yaml`](examples/network-fault-blackhole.yaml) | Complete network blackhole (100% packet loss) with approval gate |

To approve a pending run:

```bash
kubectl patch experimentrun <run-name> \
  --type=merge \
  -p '{"spec":{"approved":true}}'
```

## Action Types

### `delete_pod`

Deletes the selected pods. Supports `force: true` for immediate deletion (grace period 0).

### `network_fault`

Injects network chaos into target pods using Linux Traffic Control (`tc netem`). The controller sends HTTP requests to the `omen-agent` sidecar running inside each target pod, which applies and removes the fault. The fault is automatically rolled back after the configured `duration`.

**Prerequisite:** The target namespace must be labeled `chaos.kreicer.dev/enabled=true` so that the sidecar is injected (see [Architecture](#architecture) below).

**Parameters (`spec.action.networkFault`):**

| Field | Type | Description |
|---|---|---|
| `latency` | duration | Fixed delay added to outgoing packets (e.g., `100ms`). |
| `jitter` | duration | Random variation on top of latency (e.g., `10ms`). Requires `latency`. |
| `packetLoss` | integer (1-100) | Percentage of packets to drop. Set to `100` for a full blackhole. |
| `duration` | duration | How long to hold the fault before automatic rollback. Defaults to `5m`. |

At least one of `latency` or `packetLoss` must be set.

## Architecture

Omen uses an **opt-in sidecar model**. Chaos is only allowed in namespaces explicitly labeled with `chaos.kreicer.dev/enabled=true`. A Mutating Webhook automatically injects the `omen-agent` sidecar into all new pods in these namespaces.

```
kubectl label namespace <target-ns> chaos.kreicer.dev/enabled=true
```

The controller then:
1. Selects targets only from pods that live in labeled namespaces.
2. For `delete_pod`: deletes the pod via the Kubernetes API.
3. For `network_fault`: sends an HTTP `POST /network-fault` to the agent sidecar inside the pod to apply `tc` rules, then `DELETE /network-fault` after the duration to roll back.

### Security

The `omen-agent` sidecar requires the `NET_ADMIN` Linux capability to run `tc` commands. This means namespaces used for network chaos must allow it via Pod Security Admission:

```bash
kubectl label namespace <target-ns> \
  pod-security.kubernetes.io/enforce=baseline
```

Communication between the controller and agents is authenticated with a shared token (generated by Helm and stored in a Kubernetes Secret). The token is automatically injected into each agent sidecar as `OMEN_SECRET_TOKEN` by the mutating webhook.

A `NetworkPolicy` is shipped with the Helm chart that restricts ingress to agent sidecars so only the controller pod can reach them.

### Pre-flight registry check

On startup, the controller performs a TCP connectivity check to the agent image registry. If the registry is unreachable (e.g., in an air-gapped cluster without proper registry credentials), sidecar injection is **disabled** automatically so that user pods are never blocked by `ImagePullBackOff`. A warning is logged:

```
WARNING: agent image registry is not reachable — sidecar injection will be disabled
```

## Safety: Pod-level Opt-out

Individual pods can be excluded from all chaos experiments by adding the annotation `chaos.kreicer.dev/ignore: "true"`. Annotated pods are neither injected with the agent sidecar nor selected as targets.

```bash
kubectl annotate pod <pod-name> chaos.kreicer.dev/ignore=true
```

Or in the pod template:

```yaml
metadata:
  annotations:
    chaos.kreicer.dev/ignore: "true"
```

Experiment-level protection is also available via `spec.safety.denyNamespaces`:

```yaml
spec:
  safety:
    denyNamespaces:
      - my-critical-namespace
```

## Observability

Every phase transition of an `ExperimentRun` emits a standard Kubernetes Event on the object:

```bash
kubectl describe experimentrun <run-name>
```

Events use `Normal` type for successful transitions (`PreviewGenerated`, `Approved`, `Running`, `Completed`) and `Warning` for failure states (`Failed`, `Expired`).

The `TOTAL` column in `kubectl get expruns` is populated as soon as targets are selected during the `PreviewGenerated` phase, so you can see how many pods will be affected before the run executes.

## Safe Deletion

`Experiment` objects carry a finalizer (`chaos.omen.com/finalizer`). When an `Experiment` is deleted, the controller first deletes all owned `ExperimentRun`s and waits for them to be removed before releasing the finalizer.

`ExperimentRun`s executing a `network_fault` action carry an additional finalizer (`chaos.omen.com/network-fault`). Before the run object is removed, the controller sends `DELETE /network-fault` to the agent in all targets where the fault was still active, ensuring the network is restored even if the experiment is aborted mid-flight.

## Dry Run

Set `dryRun: true` on the `Experiment` to preview target selection without executing any action. For `delete_pod`, no pods are deleted. For `network_fault`, no HTTP requests are sent to the agent. Results are recorded as `Success` in both cases.

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
