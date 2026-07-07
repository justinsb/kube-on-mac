# Next task: service endpoint lifecycle (conntrack flush + map GC)

Status: design / ready to build. Prereqs confirmed (see "What we can observe").
Context: the mark-based v2 data plane ([services.md](services.md)) shipped, but
two lifecycle gaps remain. Both are now unblocked.

## The two gaps

1. **Endpoint removal doesn't reset pinned established flows.** When a backend
   is removed, new flows correctly avoid it (execd re-resolves within the
   cache TTL), but connections already in conntrack keep being DNAT'd to the
   now-dead backend. The v2 code has a `flushConntrackTo` that shells out to
   the `conntrack` binary — absent from workload images — so it's a no-op in
   practice. This is the correctness gap.

2. **The eps map / id allocator only grows.** `idFor` allocates a mark id +
   an `eps` element per distinct endpoint and never frees them: reusing an id
   while a removed endpoint still has live conntrack entries could misroute an
   in-flight mark. So over a long-lived pod the map accumulates every endpoint
   ever seen. Per-op cost stays O(1); memory is unbounded. This is the
   bounded-memory gap.

Both need the same capability: **knowing which endpoints still have live
conntrack flows**, and **being able to delete conntrack entries by endpoint**.

## What we can observe (confirmed 2026-07 on the nft kernel)

- Kernel config: `CONFIG_NF_CONNTRACK_EVENTS=y`, `CONFIG_NF_CT_NETLINK=y`.
  **`CONFIG_NF_CONNTRACK_PROCFS` is NOT set** — there is no
  `/proc/net/nf_conntrack`; conntrack must be read via ctnetlink.
- Dump (`conntrack -L`) shows the DNAT plainly: for a flow to a ClusterIP the
  **reply-tuple source address is the backend endpoint**. Example:
  ```
  tcp TIME_WAIT src=<client> dst=<VIP> sport=X dport=80
               src=<BACKEND> dst=<client> sport=8080 dport=X [ASSURED]
  ```
  So "flows pinned to endpoint E" = conntrack entries whose reply-src == E.
- Events: `conntrack -E` streamed live `[NEW]` events (with the DNAT already
  visible); `[UPDATE]`/`[DESTROY]` ride the same ctnetlink multicast groups.
  So we can be event-driven, not poll-driven.

## Design

**Use an in-process Go ctnetlink client, not the `conntrack` binary.** Same
pattern as our go-nfqueue / google-nftables usage (talk netlink directly, no
external tool in the guest image). `github.com/florianl/go-conntrack` (same
author/style as go-nfqueue, built on mdlayher/netlink) supports dump, delete,
and event subscription; `ti-mo/conntrack` is an alternative. Add it to execd.

**Phase 1 — conntrack flush on removal (correctness).**
- Replace `flushConntrackTo(addr)` with a ctnetlink delete filtered by
  reply-src == addr (IPv6). Called from `reconcileEndpoints` when an endpoint
  drops out of the resolved set.
- Effect: flows pinned to the removed backend are torn down and re-establish
  through the current set on their next packet.

**Phase 2 — id / map GC (bounded memory).**
- Subscribe to conntrack `DESTROY` events; maintain a per-endpoint refcount of
  live flows (increment on NEW with reply-src==E, decrement on DESTROY).
- An endpoint's mark id + `eps` element may be freed once: (a) it's not in the
  live resolved set, AND (b) its refcount is 0. Return the id to a free pool
  for reuse. This makes the map track live-plus-draining endpoints, not
  all-ever-seen.
- Simpler alternative if events prove fiddly: periodic conntrack dump, GC ids
  whose endpoint is absent from both the live set and the dump. Poll instead
  of subscribe; same safety property, coarser timing.

**Phase 3 (optional) — agent push for immediacy.** Today removal is detected
lazily (on the next re-resolve, ≤ cache TTL). The agent could watch
Services/EndpointSlices and push removals over the existing host→guest svc
channel so execd flushes immediately instead of within a TTL. Ties to the
"watch and push" note in services.md. Correctness (Phase 1) doesn't need this;
it's a latency improvement.

## Scope notes / interactions

- **Add direction is already handled**: a newly-added endpoint is picked up on
  re-resolve and gets an id/element on first selection — no lifecycle work
  needed for adds.
- **Safe id reuse is the whole point of Phase 2**: never reuse an id until its
  endpoint's element is removed and its flows have drained, or an in-flight
  first-packet mark could resolve to a different endpoint. The refcount (or
  dump check) is the gate.
- **North-star obviates most of this**: the verdict-DNAT kernel primitive
  (services.md) carries the target on the NFQUEUE verdict, so there is no
  `eps` map to GC at all — only the Phase-1 conntrack flush would remain (and
  even that is just "delete flows to a removed backend," unchanged). Worth
  remembering that Phase 2 is workaround-maintenance the primitive would
  delete.

## Acceptance test

- Long-lived connection (not wget): hold a TCP stream from a client pod to a
  ClusterIP pinned to backend A. Delete A. Confirm the stream resets (Phase 1
  flush) rather than hanging, and a fresh connection lands on a survivor.
- Churn a service's endpoints repeatedly; confirm the `eps` map size tracks
  live endpoints (Phase 2 GC), not cumulative.
