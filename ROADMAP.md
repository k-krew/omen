# Omen Roadmap

**Core Philosophy:** Lightweight and controlled chaos for those who want to get started easily. 

Omen is built so that adopting Chaos Engineering doesn't require learning monstrous systems. We focus on transparency (you always know the targets before the attack), safety (blast radius limits and manual approvals), and ease of use.

---

## Current State (v0.1.x)
*The foundation for controlled chaos.*

- [x] **Pod Deletion** — Basic action to test application resilience.
- [x] **Transparent Target Selection (Preview)** — Locking in the exact list of targets before the experiment starts.
- [x] **Manual Control (Approval)** — Ability to confirm attacks via Webhook or `kubectl patch`.
- [x] **Schedules (Cron)** — Regular chaos tests with Concurrency Policies to prevent overlaps.
- [x] **Dry Run** — Safe simulation to verify selectors and limits without causing actual harm.

---

## Near-term Plans (v0.2.x - v0.3.x)
*Expanding the attack arsenal without complicating the architecture.*

- [ ] **Default Namespace Protection**
  - Automatically protect `kube-system`, `omen-system`, and other critical namespaces from being targeted by default, requiring explicit overrides if users really want to target them.
- [ ] **Network Chaos**
  - **Network Latency** to simulate slow connections.
  - **Packet Drop** to test microservice timeouts.
  - *Focus: Implementation via lightweight sidecars or eBPF, strictly avoiding heavy node-level dependencies.*
- [ ] **Resource Stress**
  - Artificial CPU/Memory consumption inside targeted pods.
- [ ] **Advanced Target Filtering**
  - Target selection by percentage of total replicas (e.g., "kill 10% of pods, but at least 1").
  - Exclude recently created pods (protection against killing recovering replicas).

---

## Mid-term Goals (v0.4.x - v0.5.x)
*Integrations, observability, and ChatOps for teams.*

- [ ] **Messenger Integration (ChatOps)**
  - Send Approval requests directly to Slack / Telegram / Discord.
  - Interactive "Approve" / "Deny" buttons right in the chat.
- [ ] **Out-of-the-box Observability**
  - Export metrics to Prometheus (successful/failed experiments, recovery time).
  - Ready-to-use Grafana dashboards.
- [ ] **Experiment Notifications**
  - Alerts on chaos test start and completion (Kubernetes Events, Webhooks).

---

## Long-term Goals (v1.0.x)
*Smart chaos and absolute safety.*

- [ ] **Automatic Halt & Rollback**
  - Prometheus integration: if service metrics (e.g., 5xx error rate) exceed a threshold during an experiment, immediately abort the experiment.
- [ ] **Chaos Templates**
  - Pre-defined experiment templates (`ExperimentTemplate` CRD) so beginners don't have to write YAML from scratch.
- [ ] **Lightweight Web UI (Optional)**
  - Minimalist interface just to view active experiments, run history (`ExperimentRuns`), and click "Approve".

---

## Development Principles (What we will NOT do)

To ensure Omen stays true to its core philosophy, we strictly adhere to the following constraints:
1. **No hidden actions:** Users must always be able to see exactly what will happen before it happens (Dry Run & Preview).
2. **No heavy node agents:** We avoid root-privileged DaemonSets wherever possible, preferring Kubernetes API interactions or lightweight sidecars.
3. **Safety by default:** Global namespaces and the operator itself are always protected, and blast radius limits are mandatory.
