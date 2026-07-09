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

- ~~No kube-controller-manager runs in the PoC, so nothing writes
  EndpointSlices. The agent answers endpoint queries directly: Services
  watched for ClusterIP+selector; endpoints = Running+Ready pods matching
  the selector, using their podIPs (our routed IPv6 ULAs).~~ Superseded:
  KCM runs now and writes real EndpointSlices, and the resolver consumes
  them from watch-based informer caches (kube-proxy's diet: Services +
  EndpointSlices) — zero API requests at query time, and named targetPorts
  work for free since slices carry resolved port numbers. The readiness we
  report on pods flows through the endpointslice controller back into our
  own data plane. Why the cache matters:
  [client-side-rate-limiting.md](client-side-rate-limiting.md).
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

## Efficiency ceiling of the mark design, and the eBPF path (v3)

The mark-based design still expresses the target address in two places: the
nftables `eps` map (per endpoint) and conntrack (per flow). That's not the
mark stored twice — the map is per-endpoint token→address translation, needed
only because a 32-bit mark can't carry a 128-bit address+port; conntrack is
the per-flow runtime state. The map is cheap (atomic set ops on endpoint
churn, not per flow), but it *is* a translation layer we maintain rather than
something fundamental.

Why we can't just steer the packet in userspace and skip all of it: rewriting
the destination in the NFQUEUE handler without going through `nf_nat` means
conntrack never records a NAT, so reply traffic from the backend is never
rewritten back to the VIP and the client drops it (spike E1d showed the
established-flow half of exactly this). The mark→map→`dnat` path exists solely
to route the userspace decision *through* `nf_nat` so conntrack sets up a
reversible bidirectional manip.

The mechanism that removes the duplication entirely is **eBPF socket-LB
(`cgroup/connect6`, + `sendmsg6`/`recvmsg6` for unconnected UDP)** — the
Cilium model. It rewrites the destination sockaddr at `connect()` time, once
per connection, in process context, before any packet exists. The socket then
talks straight to the backend: no NAT, no conntrack NAT entry, no per-packet
cost, and the endpoint set lives in one BPF map (no conntrack duplication).
Viable on our kernel (`CONFIG_CGROUP_BPF=y`, `CONFIG_BPF_SYSCALL=y`; we build
the kernel, so BTF/CO-RE is ours to enable). execd fits it directly — it's
already the guest agent; it would manage a BPF map instead of nft rules.

Not a custom kernel module: eBPF gives the same power without out-of-tree
maintenance, and `connect6` is a near-exact fit. A module would only be
justified by a gap eBPF can't fill; there isn't one here.

The trade, and why it's not the default yet: `connect6` makes the decision
**map-driven at connect time**, not pop-up-per-flow — you can't cleanly block
a `connect()` on a userspace round-trip, so the BPF program picks from a
userspace-maintained map (round-robin/affinity/weighted in BPF) rather than
execd deciding each flow. For services that's almost always sufficient (the
per-flow choice is just "pick from the current set"). It's the same
"better than your ideal" trade noted at the top of the services table.

Plan: ship the mark-based nftables data plane (proven M1/M2/M3), which fixes
per-flow churn and endpoint-change staleness. Keep `connect6` socket-LB as
the v3 tier that removes the nft-map + conntrack-NAT duplication when node
density or throughput demands it.

## North star: an NFQUEUE verdict that establishes a DNAT

The mark+map design and the eBPF connect-LB both work around a capability the
kernel doesn't quite expose: letting a userspace NFQUEUE handler *steer a
flow* by returning the target, with the kernel setting up the reversible NAT.

The ideal primitive: on the NFQUEUE'd first packet (which already carries an
unconfirmed conntrack), userspace returns a verdict carrying the full DNAT
target `addr:port`; the kernel calls `nf_nat_setup_info(ct, target,
NF_NAT_MANIP_DST)` in the verdict path, then accepts. Consequences:

- **One rule:** `ip6 daddr SVCCIDR ct state new queue num 0`. No second rule,
  no mark, no `eps` map, no token→address translation (the verdict carries
  the 128-bit address directly).
- **Zero per-flow nft ops**, and no per-endpoint map to maintain.
- **UDP handled identically to TCP** — conntrack tracks the pseudo-flow and
  reverses replies; no `sendmsg6`/`recvmsg6` reverse-translation as eBPF
  socket-LB needs for unconnected UDP (a real, recurring Cilium pain point).
- **Decision fully in userspace, per flow** — execd can steer on arbitrary
  state (external health, app attributes, canary/fault injection, tracing)
  that can't live in a BPF map consulted at connect() time.

This is the turn-1 "userspace decides the first packet, kernel steers the
rest" ideal, realized cleanly — possible only because pods run a guest Linux
kernel we control. And it is genuinely upstreamable: "let a userspace NFQUEUE
handler perform NAT" is broadly useful (userspace LBs, custom NAT policy), not
a kube-on-macOS hack.

### Spiked: does it work today? No — but the patch is ~6 lines.

Read the 6.12 source. The NFQUEUE verdict's `NFQA_CT` attributes are parsed by
`ctnetlink_glue_parse_ct()` (net/netfilter/nf_conntrack_netlink.c), which
handles exactly `CTA_TIMEOUT`, `CTA_STATUS`, `CTA_HELP`, `CTA_LABELS`,
`CTA_MARK` — and **nothing NAT**. A verdict carrying `CTA_NAT_DST` is parsed
and silently ignored. So the primitive does not exist today; no userspace
attribute-crafting reaches it.

Deeper finding: NAT setup (`ctnetlink_setup_nat` → `nf_nat_setup_info`) is
reachable only from conntrack **creation**. `ctnetlink_change_conntrack()`
*explicitly* rejects `CTA_NAT_*` with `-EOPNOTSUPP` ("only allow NAT changes …
for new conntracks"). It's a deliberate invariant: NAT is bound at ct
creation, not retrofitted onto an existing entry.

But that invariant *permits* our case cleanly: on the NFQUEUE'd first packet
the ct is NEW and **unconfirmed** — still being created. So the principled
patch is, in `ctnetlink_glue_parse_ct`:

```c
if (cda[CTA_NAT_DST] || cda[CTA_NAT_SRC]) {
    if (nf_ct_is_confirmed(ct))      /* only at creation, honoring the invariant */
        return -EOPNOTSUPP;
    err = ctnetlink_setup_nat(ct, cda);   /* the existing, tested helper */
    if (err < 0)
        return err;
}
```

~6 lines, reusing the existing helper, respecting the creation-time
invariant (unconfirmed cts only). Second-order detail to work out when
building it: ensuring the first packet actually gets mangled after the verdict
given the hook it was queued from (the manip is on the ct; `nf_nat_packet`
applies it on the next nat-hook traversal — may want to queue at/repeat
through the right hook). Decisive spike outcome: **verdict-DNAT is a small,
well-scoped, upstreamable kernel patch, not a userspace trick.** Details in
[conntrack-spike.md](conntrack-spike.md).

Corollary (nice property of where we already are — noted by the project
owner): lazy pop-up + userspace steering compose with a **hot/cold tiering**
optimization — cold services stay lazy (pop up per new flow), and any
high-traffic VIP can be *materialized* into an old-style persistent nftables
rule (numgen or the mark map) so it stops popping up at all. Best of both:
laziness where flows are rare, zero-pop-up in-kernel where they're hot.

Data-plane evolution summary:
- v1: persistent per-VIP numgen rule. Works; stale on endpoint change; no
  per-flow churn but in-kernel-only LB decision. (Superseded.)
- v2 (SHIPPED, mark-based — poc/execd/services.go): one static rule +
  `mark->endpoint` map; execd resolves (cached), picks a backend in userspace
  (round-robin), sets the verdict mark. Zero per-flow nft ops. Endpoint change
  reflected within the cache TTL (fixes v1 staleness); removal does a
  best-effort conntrack flush. Verified: 4/4 LB split, one "LB active" log
  line, backend deletion drains to the survivor with no pod restart. Byte-order
  gotcha for the map key: `mark` set keys are native/little-endian (to match
  `meta mark`); the DNAT reads addr from reg1 and port from reg2 (RegProtoMin
  2, 16-byte register model); the concat map needs `Concatenation: false`
  (that flag is for concatenated keys, not concat data).
- v-next (north star): NFQUEUE verdict establishes the DNAT directly. One
  rule, no map, UDP-uniform, fully userspace-steered. Kernel primitive
  (possibly free via NFQA_CT; else a small upstreamable patch).
- v3 (throughput/density tier): eBPF `cgroup/connect6` socket-LB. No NAT, no
  conntrack state; map-driven at connect time (not pop-up); weaker UDP story.

## Deliberate v1 simplifications

- OUTPUT-hook only (pod-originated traffic). Hostports/NodePort ingress are
  a separate path (host-side, per the main doc).
- TCP+UDP by port; no named ports, no sessionAffinity, no topology hints.
- Random per-flow balancing via numgen (no weights).
- TTL invalidation, no push yet; no conntrack flush on endpoint removal.
- If the endpoint set is empty, execd installs a reject rule (fail fast)
  with a short TTL rather than dropping to a black hole.
