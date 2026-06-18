# Omen Roadmap

**Core Philosophy:** Lightweight and controlled chaos for those who want to get started easily. 

Omen is built so that adopting Chaos Engineering doesn't require learning monstrous systems. We focus on transparency (you always know the targets before the attack), safety (blast radius limits and manual approvals), and ease of use.

---

## Completed (v0.1.x - v0.3.x)
*The foundation for controlled chaos and network faults.*

- [x] **Pod Deletion** — Basic action to test application resilience.
- [x] **Network Chaos** — Inject latency, packet loss, corruption, and duplication via an opt-in sidecar architecture (`omen-agent`).
- [x] **Advanced Target Filtering** — Select exact target counts or percentage of total replicas. Skip terminating pods and pods with opt-out annotations.
- [x] **Transparent Target Selection (Preview)** — Locking in the exact list of targets before the experiment starts.
- [x] **Manual Control (Approval)** — Ability to confirm attacks via Webhook or `kubectl patch`.
- [x] **Schedules (Cron)** — Regular chaos tests with Concurrency Policies to prevent overlaps.
- [x] **Safety by Default** — Opt-in namespace architecture (`chaos.kreicer.dev/enabled=true`) ensures critical workloads are never accidentally targeted.
- [x] **Observability** — Emits standard Kubernetes Events on state changes and provides Prometheus metrics for both controller and agent.
- [x] **Dry Run** — Safe simulation to verify selectors and limits without causing actual harm.

---

## Near-term Plans (v0.4.x)
*Expanding the attack arsenal and integrations.*

- [ ] **Resource Stress**
  - Artificial CPU/Memory consumption inside targeted pods using the `omen-agent` sidecar.
- [ ] **Messenger Integration (ChatOps)**
  - Send Approval requests directly to Slack / Telegram / Discord.
  - Interactive "Approve" / "Deny" buttons right in the chat.
- [ ] **Grafana Dashboards**
  - Ready-to-use Grafana dashboards for controller and agent metrics.

---

## Mid-term Goals (v0.5.x)
*Smart chaos.*

- [ ] **Automatic Halt & Rollback**
  - Prometheus integration: if service metrics (e.g., 5xx error rate) exceed a threshold during an experiment, immediately abort the experiment and roll back faults.
- [ ] **Chaos Templates**
  - Pre-defined experiment templates (`ExperimentTemplate` CRD) so beginners don't have to write YAML from scratch.

---

## Long-term Goals (v1.0.x)
*The ultimate user experience.*

- [ ] **Lightweight Web UI (Optional)**
  - Minimalist interface just to view active experiments, run history (`ExperimentRuns`), and click "Approve".

---

## Development Principles (What we will NOT do)

To ensure Omen stays true to its core philosophy, we strictly adhere to the following constraints:
1. **No hidden actions:** Users must always be able to see exactly what will happen before it happens (Dry Run & Preview).
2. **No heavy node agents:** We avoid root-privileged DaemonSets wherever possible, preferring Kubernetes API interactions or lightweight sidecars.
3. **Safety by default:** Global namespaces and the operator itself are always protected, and blast radius limits are mandatory.
