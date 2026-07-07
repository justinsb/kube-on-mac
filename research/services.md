# Services: lazy, pop-up-to-userspace ClusterIP load balancing

Status: design + build notes, 2026-07
Context: the guest-side service data plane from
[macos-kubelet.md](macos-kubelet.md), realized with the project's preferred
control model: **first packet of a new flow pops up to userspace, which
decides and programs the kernel lazily; every subsequent packet flows with
zero userspace involvement.**

## The arc

The original design discussion asked for exactly this model and concluded
macOS couldn't provide it (no NFQUEUE/divert-reinject equivalent, no way to
install flow state from userspace). The resolution then was "move the
decision into the guest at connect time." Now that the dataplane *is* a
guest Linux kernel, the preferred model is simply available: netfilter has
NFQUEUE (punt a packet to userspace, verdict + reinject) and fully
programmable NAT. We get the macOS-impossible design on a macOS node.

## Why lazy beats eager fan-out here

Eager kube-proxy-style programming pushes every Service to every node. Our
"nodes" are per-pod microVMs — eager means every service table in every pod
VM, updated on every endpoint change: O(pods × services) churn through the
vsock control channel.

Lazy inverts it: a pod VM learns only the services it actually dials, on
first use, and caches the programmed rule. Most pods talk to a handful of
services; most service updates then touch few or no VMs.

## Data plane design (in each pod VM)

Base state, installed by execd at boot via netlink (google/nftables — no
nft binary needed in images):

```
table ip6 kube  {
  chain output {                       # type nat hook output
    ip6 daddr <SERVICE_CIDR> ct state new  queue num 0
    # (per-VIP rules get inserted above the queue rule, lazily)
  }
}
```

Flow for a *new* (VIP, port) the pod has never used:

1. First packet (TCP SYN / UDP dgram) to the VIP matches the queue rule →
   NFQUEUE 0 → execd (florianl/go-nfqueue).
2. execd asks the host agent over vsock (guest-initiated port 1025, the
   reverse direction of the exec channel): "endpoints for [VIP]:port?"
3. Host agent answers from its Service/Pod watch.
4. execd inserts a per-VIP DNAT rule *ahead of* the queue rule:
   `ip6 daddr VIP tcp dport P dnat to numgen random mod N map { 0: ep0, … }`
5. execd verdicts the queued packet NF_REPEAT — it re-traverses, hits the
   new rule, gets DNAT'd, conntrack records the flow.
6. Every later packet of that flow: conntrack fast path. Every later *flow*
   to that VIP: the nft rule + numgen, in-kernel random load balancing.
   Userspace never sees this VIP again.

Invalidation: v1 uses a TTL (execd re-queries and rebuilds the rule); v2
adds host-pushed invalidation over the existing host→guest channel when
endpoints change. A removed endpoint additionally needs conntrack entries
flushed (CTNETLINK) so established flows don't keep hitting it.

**Known gap in the as-built code (see the endpoint-change note below):** the
persistent per-VIP DNAT rule is inserted *above* the queue rule and has no
`ct state new` guard, so once installed it shadows the queue rule — no packet
re-queues, the TTL never triggers a refresh, and there is no rule-expiry
timer. Net effect today: endpoint changes are not reflected until the pod VM
restarts. Fixing this is the next services increment; the preferred direction
(per-flow userspace decision instead of a persistent rule) is validated in
[conntrack-spike.md](conntrack-spike.md).

### Direction for the redesign (spiked, see conntrack-spike.md)

We explored "make the routing decision in userspace per new flow, program the
kernel so packets flow in-kernel, no persistent per-VIP rule." Findings:

- **Directly injecting a NAT'd conntrack entry via ctnetlink does NOT drive
  DNAT** — the manip is only set up by a packet traversing a real nat rule.
- **A transient per-flow (5-tuple) DNAT rule does the job**: install on the
  NFQUEUE pop-up, `NF_REPEAT`, then delete once conntrack holds the flow
  (verified: the flow keeps working after the rule is gone). This gives
  per-flow userspace decisions with an in-kernel per-packet path and no
  persistent-rule staleness — the model the project prefers. Endpoint removal
  still needs a conntrack flush for pinned established flows.

  Caveat: this still does 2 nft ops per new flow — the kube-proxy-iptables
  churn hotspot. Superseded by the mark-based design below.

- **Mark-based single-rule design — zero nft ops per flow (recommended).**
  One static rule DNATs by packet mark via a `mark -> endpoint` map; execd
  sets the mark on the NFQUEUE verdict (`SetVerdictWithMark`, `NF_REPEAT`)
  instead of touching the ruleset. The map changes only on endpoint churn, at
  atomic set-element granularity. Per-flow cost = one vsock query + one mark
  set, **no nftables operations**. execd picks the backend in userspace, so
  LB policy is arbitrary (beats in-kernel `numgen`). Fully spiked (M1/M2/M3)
  in [conntrack-spike.md](conntrack-spike.md) — this is the data plane to
  build for v2.

## Control plane (host agent)

- No kube-controller-manager runs in the PoC, so nothing writes
  EndpointSlices. The agent answers endpoint queries directly: Services
  watched for ClusterIP+selector; endpoints = Running+Ready pods matching
  the selector, using their podIPs (our routed IPv6 ULAs).
- ClusterIPs must be IPv6: apiserver runs with
  `--service-cluster-ip-range=<v6 prefix>` (single-stack IPv6 services).
  The apiserver allocates ClusterIPs itself — no controller needed.
- vsock wiring: the harness adds `krun_add_vsock_port(1025, <sock>)` — the
  guest-initiated direction (execd dials CID 2:1025 on cache miss); the
  agent listens on the per-pod unix socket.

## Kernel requirement — and how it's being built

The Kata prebuilt kernel has conntrack/NAT/NFQUEUE-netlink but **no
nftables** (`CONFIG_NF_TABLES is not set`, no ip6tables either). Apple's
containerization config has the full set (NF_TABLES_IPV6, NFT_NAT,
NFT_QUEUE, NFT_NUMGEN) — validating the direction, but their config
targets VZ's device expectations.

Decision: first self-built kernel = **Kata 6.12.28 config (proven under
libkrun) + a small enable-delta**: NF_TABLES, NF_TABLES_INET, NFT_CT,
NFT_NAT, NFT_QUEUE, NFT_NUMGEN, NFT_COUNTER, NFT_MASQ, NFT_REJECT, and
ip6tables compat. This is the design doc's "known config + small delta"
milestone arriving on demand.

Build environment: **a pod on this cluster** (debian, 8 vCPU, 10Gi —
`kubectl exec` driving apt + kernel.org source + make). The PoC builds its
own kernel; docker remains the fallback if virtiofs I/O makes it painful.

## As-built (works end to end)

Verified: a client pod curling an IPv6 ClusterIP triggers exactly one
NFQUEUE pop-up to execd, which queries the host, installs a numgen-random
DNAT rule across both backends, and NF_REPEATs. 10 requests → 3/7 split
across backends, and the guest installed the rule **once** — every later
flow ran entirely in-kernel (rule + conntrack), zero userspace. Exactly the
"pop up on the first packet, program lazily, then get out of the way" model.

Findings that bit us:

- **Kernel**: the Kata prebuilt kernel has no nftables. Built a custom
  arm64 kernel = Kata 6.12.28 config + the NFT enable-delta (in a container;
  the in-cluster pod build also works but pod ephemerality made a plain
  `docker run` the more convenient driver). Bind-mounted volumes tripped
  tar (Docker Desktop gRPC-FUSE), so extract+build inside the container and
  copy only the Image out.
- **google/nftables data-map lookup needs `IsDestRegSet: true`.** A
  numgen→address map lookup (`expr.Lookup{DestRegister:1, SetName…}`)
  serializes to malformed bytecode without it — kernel rejects the whole
  rule with a bare EINVAL (`netlink receive: invalid argument`). Single
  immediate DNAT (no map) worked, which is what isolated it.
- **execd must survive service-LB failure.** An early nil-context panic in
  go-nfqueue's socket callback (pass `context.Background()`, not `nil`)
  crashed execd — i.e. PID-1-ish — rebooting the whole VM. Now the LB
  goroutine has a recover; the workload is never taken down by a services
  bug.
- **Dual-stack apiserver.** An IPv6-only `--service-cluster-ip-range` won't
  start (the apiserver's own kubernetes service / advertise address wants
  IPv4). Run dual-stack v4-primary + v6-secondary; Services request the v6
  family (`ipFamilies: [IPv6]`) to get pod-reachable ClusterIPs.

## Deliberate v1 simplifications

- OUTPUT-hook only (pod-originated traffic). Hostports/NodePort ingress are
  a separate path (host-side, per the main doc).
- TCP+UDP by port; no named ports, no sessionAffinity, no topology hints.
- Random per-flow balancing via numgen (no weights).
- TTL invalidation, no push yet; no conntrack flush on endpoint removal.
- If the endpoint set is empty, execd installs a reject rule (fail fast)
  with a short TTL rather than dropping to a black hole.
