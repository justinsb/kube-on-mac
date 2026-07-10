# Pod launch benchmarks (2026-07-09)

Basic numbers for pod launch latency and throughput, with images prepulled
(busybox:1.28, already flattened to EROFS in the cache — we're measuring the
pod machinery, not the registry). Methodology: `poc/bench/bench.py` creates
N bare pods (`sleep 3600`, no probes, requests 10m/32Mi, limit 128Mi),
wall-clocks `kubectl apply` → all pods Ready as observed by a 0.2s poller,
then derives per-pod `creationTimestamp → running.startedAt` (1s
granularity — the wall clock is the trustworthy number at N=1).

Host: Apple Silicon, 12 cores, 48GB. Full path per pod: apiserver →
kube-scheduler binds → agent reconcile → boot a fresh libkrun/HVF microVM
(own kernel!) → execd assembles EROFS+overlay root, brings up dual-stack
networking, starts the workload → readiness reported back through the
kubelet-style status pipeline.

## Results

| N | apply | all Ready | throughput | per-pod p50 | p90 | max |
|---|---|---|---|---|---|---|
| 1 (×5 runs) | ~0.1s | **0.6–1.1s** | — | ~1s | — | — |
| 10 | 0.2s | **1.9–2.3s** | ~5 pods/s | 1–2s | 2s | 2s |
| 100 | 1.7s | **10.1s** | **~10 pods/s** | 5s | 7s | 8s |

- **Teardown**: 100 pods deleted (graceful, SIGTERM in-guest) in ~4.3s.
- **Footprint**: ~54MB host RSS per idle pod (podvm + gvproxy +
  vmnet-helper for ~104 concurrent VMs ≈ 5.6GB total). The 128Mi limit is a
  ceiling, not a commitment — libkrun allocates lazily.
- **Zero start retries** across the 100-pod burst (the parallel
  vmnet-helper spawn storm has been flaky before; not this run).

## Where the single-pod second goes

Roughly: scheduler bind is ~instant; the agent's reconcile loop is a 1s
list-poll, so detection alone contributes up to ~1s (a pod *watch* would
shave most of that — it's the obvious next optimization and what a real
kubelet does); rootfs staging is milliseconds (the boot dir is execd + a
JSON spec; the image is a shared read-only block device, nothing is
copied); microVM boot to execd-running is a few hundred ms; the first
status push happens immediately at start.

At N=100 the median rises to 5s: 100 `runPod` goroutines contend for 12
cores of process spawning (3 host processes per pod) and VM boots. Still,
100 hardware-isolated VMs from a cold "kubectl apply" to all-Ready in 10s.

## Context: how this compares to real clusters

The upstream scalability SLO is *"99% of stateless pods with prepulled
images start within 5s"* — measured cluster-wide, with pods spread across
many nodes. This toy does p90=7s/max=8s while absorbing all 100 pods on
**one node**, each in its own VM; at N≤10 it's comfortably inside the SLO
with single-digit-second bursts. A production node ingesting 100 pods at
once typically takes minutes: kubelet serializes chunks of its sync loop,
CNI ADD calls queue, and (pre-1.27) the 5 QPS client throttle metered the
status pipeline ([client-side-rate-limiting.md](client-side-rate-limiting.md)).

Why a *toy* beats them: no CRI round-trips, no CNI plugin chain (networking
is pre-wired into the VM at boot), no cgroup surgery (the VM boundary does
isolation), image "mounting" is opening a shared EROFS file, and the whole
control plane is three localhost hops away. It's a useful existence proof
of how much of real-world pod latency is integration overhead rather than
anything fundamental — hardware virtualization included, the floor is
well under a second.

Caveats, honestly: trivial workload (busybox sleep), warm image cache and
warm control plane, no projected SA tokens to mount, single node, 1s
timestamp granularity on per-pod stats, and our status pipeline reports
readiness faster than a kubelet's default 10s sync loop would.
