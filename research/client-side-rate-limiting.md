# Client-side rate limiting: the invisible pod-throughput ceiling

We hit this in the PoC (see the postmortem at the bottom), but the bottleneck
is not a PoC artifact — it ships in every real cluster, and it caps pod
launch throughput per node in a way that doesn't show up on any dashboard
until you know to look for it.

## The mechanism

Every client-go client carries a token-bucket rate limiter, configured by
`QPS` (steady-state) and `Burst` (bucket size). It is *client-side*: requests
over the limit aren't rejected, they **queue inside the client process**,
invisibly. No 429s, no apiserver metrics, no events — just every API call
acquiring a token before it's allowed to touch the network. Under sustained
load the queue depth grows and *every* request through that client inherits
seconds of latency, including ones that have nothing to do with the noisy
caller. The failure signature is "the apiserver seems slow" while the
apiserver is idle.

The one visible trace is a client-side log line, easy to misread as an
apiserver problem:

    Waited for 3.19s due to client-side throttling, not priority and fairness ...

## What the real components ship

| Component | Flag / field | Default |
|---|---|---|
| kubelet | `kubeAPIQPS` / `kubeAPIBurst` | 5/10 before v1.27, **50/100 since** |
| kube-controller-manager | `--kube-api-qps` / `--kube-api-burst` | 20/30 |
| kube-scheduler | `clientConnection.qps` / `.burst` | 50/100 |
| controller-runtime (operators) | `rest.Config` | 20/30 |
| client-go raw default | `rest.Config` | 5/10 |

The kubelet history is the interesting one. Its 5/10 default dated from an
era when the apiserver had no self-protection beyond `--max-requests-inflight`,
so every client was asked to self-limit conservatively. API Priority and
Fairness (APF: server-side, per-identity queuing with fairness — beta 1.20,
GA 1.29) removed most of the original justification, and v1.27 raised the
kubelet defaults to 50/100 citing exactly the symptom class we hit: status
updates queueing behind each other on busy nodes.

Notably, upstream raised the limit rather than removing it (`QPS: -1`
disables the limiter entirely, and plenty of modern controllers run that
way). The remaining argument for keeping a client-side bound: it caps the
node's burst behavior even when the apiserver is degraded for unrelated
reasons, and APF's fairness is per-flow — a kubelet that floods its own flow
still starves *itself*, just server-side, with less visibility.

## Why this is a pod-*launch* bottleneck

Launching one pod costs a kubelet roughly 10 writes: a handful of status
patches (Pending → ContainersReady progression, probe transitions) plus
several Events (Pulling, Pulled, Created, Started — per container). Volumes
add configMap/secret GETs unless served from the informer cache.

At the old 5 QPS default, a node cold-starting 100 pods (a reboot, a
rollout, a scale-up on a big node) has ~1,000 API writes to make: **200
seconds of pure client-side queueing**, serialized, before considering the
apiserver at all. Everything the kubelet does through that client — node
status, lease renewal, the next pod's status — waits in the same line. This
is a big part of why dense-node pod-startup latency improved in 1.27+
without any apiserver change: the ceiling was in the client the whole time.

The same arithmetic applies to controllers: a deployment controller at 20
QPS can only create ~20 pods/second cluster-wide, a namespace controller
deletes at its QPS, and so on. For launch-throughput work, the client-side
limits of the kubelet, KCM, and the scheduler form a pipeline where the
lowest number wins — and none of them appear in any server-side metric.

## The sharper lesson: shared clients couple unrelated latencies

Rate limits are per-*client*, not per-*purpose*. Our incident (below) was a
latency-sensitive path (service resolution, guest-side 5s deadline) starving
behind a chatty control loop (unconditional per-pod status pushes) because
they shared one client. Real clusters have the same shape: kubelet probes,
status, events, and leases share a client; an operator's reconcile writes
and its leader-election renewals share a client (leader-election loss under
client-side throttling is a classic outage). The fixes, in order of
preference:

1. **Don't make the requests** — dedupe writes, use informer caches instead
   of per-query GETs/LISTs. (A request avoided beats any limit.)
2. **Raise the limit to match the workload** — the 1.27 kubelet answer.
3. **Split clients** so latency-sensitive paths don't queue behind bulk
   paths (separate `rest.Config`s, or `QPS: -1` on the critical one).

## What we do

The agent runs at **QPS=50 / Burst=100 — the modern upstream kubelet
defaults** — and keeps client-side limiting on rather than going to `-1`:
matching the real kubelet's shipped configuration is the point of this
project, tuning rationale included. Independently, status pushes dedupe on a
digest, so the steady-state request rate is tiny (fix #1 above did the real
work; the QPS bump is headroom). And the lazy service-LB resolver now
applies fix #1's stronger form: it answers from watch-based informer caches
of Services + EndpointSlices (kube-proxy's diet) — zero API requests at
query time, so the latency-sensitive path can't queue behind anything, at
any QPS setting. As a bonus, EndpointSlices carry resolved port numbers, so
named targetPorts went from "not supported" to free.

## Postmortem: how it bit us (2026-07-09)

The first multi-container status loop pushed `UpdateStatus` unconditionally
every 5s per pod. With ~13 pods that's a few requests/second sustained —
comfortably above client-go's raw default of QPS=5, which the agent was
still using. Every API call queued 3+ seconds deep. The lazy service-LB
resolver (guest asks the agent to resolve a ClusterIP, 5s deadline) needs
two LISTs per cache miss; both queued behind the status traffic, the guest
timed out, and **ClusterIP traffic broke cluster-wide** — while the
apiserver sat idle and pod-to-pod networking tested fine. The diagnosis
came from the guest-side log ("no answer from host") paired with the agent's
"client-side throttling, not priority and fairness" lines: the answer was
computed on time, then sat in a token-bucket queue past the deadline.
