# Spike: can userspace program conntrack directly to DNAT a flow?

Status: in progress, 2026-07
Goal: validate the "direct conntrack programming" service data plane
([services.md](services.md) endpoint-change discussion) — userspace makes a
per-flow routing decision, injects a NAT'd conntrack entry via ctnetlink,
and the kernel DNATs that flow's packets with no persistent nftables rule
and no per-packet userspace involvement.

## The core question, isolated

Does a **userspace-injected conntrack entry with a DNAT manip** actually
cause the kernel's `nf_nat_packet()` to rewrite that flow's packets? If no,
the whole idea is dead and we fall back to per-flow nft rules or the
push-map model. If yes, we then tackle the first-packet/NFQUEUE timing.

Precedent: conntrackd failover replicates already-NAT'd conntrack entries
via ctnetlink onto a backup firewall, and they take effect — strong hint the
mechanism works. But that injects entries for flows whose first packet
happened elsewhere; our case establishes NAT on a live first packet.

Feasibility gate (checked): the kata base kernel config has
`CONFIG_NF_CT_NETLINK=y` and `CONFIG_NF_CONNTRACK_EVENTS=y`, so our built
nft kernel can both inject conntrack entries and subscribe to conntrack
events (the latter matters for cache/endpoint-removal handling later).

## Experiment ladder

- **E1 (UDP, static inject):** cleanest test of the core mechanism — no TCP
  state machine. Fixed source port so the full tuple is known up front.
  Register an empty nat OUTPUT hook (so nf_nat is active), inject a
  UDP DNAT conntrack entry ::1:VIP -> ::1:BACKEND, send a datagram from the
  fixed source port to the VIP, see if it lands on the backend.
- **E2 (TCP, static inject):** same with TCP + fixed source port; surfaces
  the TCP-state question (injected ESTABLISHED vs an incoming SYN).
- **E3 (TCP via NFQUEUE):** the real design — queue the SYN, learn the
  source port, inject in the handler, verdict, confirm the held packet and
  its replies NAT correctly.

Self-contained trick: do it all inside one pod over loopback — VIP = ::1:VIP,
backend = ::1:BACKEND with a server bound there. Local DNAT via the OUTPUT
hook is a standard pattern, and it keeps the spike to a single pod.

## Log

### E1 (loopback) — inconclusive, confounded

First attempt did the whole thing over loopback (`::1:VIP` -> `::1:backend`)
inside one pod. Result: nothing delivered — **but even a real nft DNAT rule
failed the same way** over `::1`->`::1`. So loopback-to-loopback DNAT
delivery is broken in this environment (locally-generated packet already
routed to lo before OUTPUT DNAT re-points it); it's a test artifact, not a
finding. Lesson: test on the real inter-pod path where DNAT is known to work
(the services milestone proved nft DNAT works pod-to-pod).

### E1c/E1d (two-pod) — pure ctnetlink injection does NOT drive DNAT

Setup: backend pod runs a UDP server on `[podIP]:7777`; spike pod is the
client; `VIP = fd42:6b75:6265:1::abcd` (in the service /112, off-link).

- Control 0 (direct to backend): delivered. Path works.
- A (nft rule `dnat to [backend]:7777`): delivered. Mechanism works on this
  path.
- B (empty nat hook + `conntrack -I` asymmetric entry, correct tuples
  `orig=client->VIP`, `reply=backend->client`): **backend got nothing.** The
  entry existed with the right shape but the datagram was not rewritten — it
  left addressed to the (off-link) VIP and was lost. Entry stayed
  `[UNREPLIED]`.

**Conclusion: injecting an asymmetric conntrack entry via ctnetlink is
descriptive, not enforced.** The packet-mangling manip is established by
`nf_nat_setup_info()`, which runs only when a packet traverses a real nat
rule on the first packet — not on ctnetlink insert. (`conntrack -I` with
tuple asymmetry doesn't carry the `CTA_NAT_*` setup that would trigger it.
conntrackd failover works because the backup also has the nat *rules*, or
uses explicit nat attributes — not the bare tuple trick. A raw-netlink
attempt with explicit CTA_NAT attributes might behave differently; unverified
and not worth it given E2.)

### E2 (two-pod) — transient per-flow rule + conntrack: WORKS

The viable shape of "userspace decides per flow, kernel handles packets":

1. Install a 5-tuple DNAT rule (`ip6 daddr VIP udp dport 9999 dnat to
   [backend]:7777`).
2. Client sends pkt1 -> delivered. The rule ran `nf_nat_setup_info`;
   conntrack now holds the flow with a real manip.
3. **Delete the rule** (`nft flush chain`; 0 dnat rules remain).
4. Client sends pkt2 on the same flow (same source port) -> **still
   delivered**, purely via the conntrack entry. No rule, no userspace.

Backend log showed both `pkt1-rule-present` and `pkt2-rule-deleted`.

## Verdict & design implication

The elegant "no rules at all, just program conntrack" idea does not work with
plain ctnetlink. But the goal it was serving — per-flow userspace decision,
no persistent per-VIP rule, per-packet path fully in-kernel, new flows always
current — is achieved by the **transient per-flow rule** pattern:

- execd NFQUEUEs the first packet of a new flow, asks the agent for the
  current endpoints (or uses a hot cache), picks one.
- Installs a **flow-specific** (5-tuple) DNAT rule, `NF_REPEAT`s the packet.
  The repeated packet traverses the rule, `nf_nat` sets up the manip,
  conntrack records the flow.
- Removes the rule once the flow is established (poll conntrack, or — better —
  subscribe to conntrack NEW events; `CONFIG_NF_CONNTRACK_EVENTS=y` is on).
  A short rule timeout would also do.
- Every later packet of the flow: in-kernel via conntrack. Every later *flow*
  to that VIP: pops up again, so the decision always uses current endpoints —
  no staleness, which was the whole motivation.

Cost vs today's persistent-numgen-rule design: a vsock round-trip + two nft
ops per *new flow* (not per packet), against gaining always-current endpoint
selection and natural endpoint-change handling for new flows. Endpoint
*removal* still needs conntrack flush for established flows pinned to the
removed backend (CTNETLINK delete by reply-src) — unchanged by any of this.

Open alternative not spiked: single persistent rule doing `dnat to
<map: ct mark -> addr>`, with execd setting the mark per-flow from the NFQUEUE
verdict (userspace picks the backend, one rule, no per-flow churn). Keeps a
small index->addr map that execd updates on endpoint change. Worth a spike if
per-flow rule churn proves costly at scale.

## Follow-up spike: mark-based single-rule design (no per-flow churn)

Motivation: per-flow nft rule add/delete is the classic kube-proxy-iptables
scaling wound (ruleset reloads O(rules)). The transient-rule design above
still does 2 nft ops per new flow. Can we get to **zero nft ops per flow**?

Idea: one static DNAT rule keyed on the packet mark; execd sets the mark on
the NFQUEUE verdict (free — no netlink ruleset change); a mark->endpoint map
translates in-kernel; the map changes only when *endpoints* change.

```
chain output type nat hook output priority dstnat {
  ip6 daddr SVCCIDR meta mark != 0 dnat ip6 to meta mark map @eps  # re-traversal
  ip6 daddr SVCCIDR meta mark 0     queue num 0                    # first packet
}
map eps { type mark : ipv6_addr . inet_service }  # {id -> addr:port}
```

Spiked on a two-pod path (mark-spike client, backend pod), UDP:

- **M1 — static mark rule carries many flows, zero churn.** One rule
  `meta mark 0x1 dnat to [backend]:7777`. Three flows (distinct source
  ports) marked 0x1 via `SO_MARK`, one unmarked control. All three marked
  flows delivered; the unmarked control was not DNAT'd. The rule was never
  edited between flows.
- **M2 — mark->endpoint map selects backends.** `dnat ip6 to meta mark map
  @eps` with `{0x1: backend:7777, 0x2: backend:7778}`. Marked-1 flows landed
  on 7777, marked-2 on 7778. Endpoint change = a `nft add/delete element`
  (atomic set op, no ruleset reload), never a rule edit.
- **M3 — the crux: does the NFQUEUE verdict mark survive NF_REPEAT?** Yes. A
  tiny go-nfqueue daemon (`SetVerdictWithMark(id, NfRepeat, 1)`) marked each
  first packet; the re-traversed packet matched `meta mark != 0` and was
  DNAT'd via the map to the backend; conntrack recorded the manip. All three
  nfqueue-driven flows delivered.
  - Aside: only one process can bind a given queue number — the marktest
    daemon EPERM'd on queue 0 because the pod's own execd already owns it for
    the real service LB. Confirms the single-handler model; not a problem in
    production (execd is that handler).

### Verdict: this is the design to build

Per-flow cost collapses to: one vsock query (cacheable in execd) + one
`SetVerdictWithMark`. **Zero nftables operations per flow.** nft changes only
on endpoint churn, at set-element granularity (atomic, no reload). And
because execd picks the backend in userspace per flow, LB policy is arbitrary
(round-robin / weighted / affinity) — strictly more flexible than the
in-kernel `numgen random` the current PoC uses.

Shape for execd:
- Maintain an endpoint-id allocator: each live (addr,port) endpoint gets a
  small stable int id; `eps` map holds `{id -> addr . port}`.
- Base rules installed once at boot (the two above + the map).
- On NFQUEUE pop-up: resolve/lookup endpoints (hot cache), pick one, ensure
  its id/map-element exists, `SetVerdictWithMark(NfRepeat, id)`. No per-flow
  nft op.
- On endpoint change (host push): add/remove `eps` elements; flush conntrack
  entries whose reply-src is a removed endpoint (established flows pinned to
  it). Still the one unavoidable conntrack-flush step.
- No connmark needed: only the first packet consults the mark; conntrack
  carries the rest regardless. (Established packets never traverse the chain —
  the nat hook applies the stored manip directly — so they never hit the
  queue rule.)

Only-loose-end: a removed endpoint's id must not be reused until its
conntrack entries are gone, or an in-flight mark could map to a new endpoint.
Delay id reuse (or gate on a conntrack-empty check).

## Spike: verdict-DNAT primitive — source verdict (does NFQA_CT carry NAT?)

Question: can an NFQUEUE verdict carry `CTA_NAT_*` (nested in `NFQA_CT`) and
have the kernel set up the DNAT, giving the one-rule/no-map north-star design?

Read linux v6.12 `net/netfilter/nf_conntrack_netlink.c` +
`nfnetlink_queue.c`. Path: verdict `NFQA_CT` → `nfqnl_ct_parse` →
`nfnl_ct_hook->parse` = `ctnetlink_glue_parse` → `ctnetlink_glue_parse_ct`.

`ctnetlink_glue_parse_ct` handles only: `CTA_TIMEOUT`, `CTA_STATUS`,
`CTA_HELP`, `CTA_LABELS`, `CTA_MARK`. **No NAT.** `CTA_NAT_DST` on a verdict
is parsed into `cda[]` and ignored.

`ctnetlink_setup_nat()` (→ `ctnetlink_parse_nat_setup` → `nf_nat_setup_info`)
is the helper we'd want; it's called only from the conntrack **create** path.
`ctnetlink_change_conntrack()` deliberately rejects `CTA_NAT_*` with
`-EOPNOTSUPP` ("only allow NAT changes … for new conntracks"). So the kernel
enforces "NAT bound at creation, not retrofitted."

Verdict: **doesn't work today; needs a kernel patch** — but a clean, small,
principled one. On the NFQUEUE'd first packet the ct is new + unconfirmed
(still "being created"), so calling `ctnetlink_setup_nat` there honors the
invariant. Patch = ~6 lines in `ctnetlink_glue_parse_ct` guarded by
`!nf_ct_is_confirmed(ct)`, reusing the existing helper (see services.md for
the snippet). Upstreamable: "let a userspace NFQUEUE handler set up NAT on the
flow it's steering" is generally useful, not a kube-on-macOS special.

Second-order detail for when we build it: the manip lands on the ct, but the
first packet still needs to traverse a nat hook post-verdict for
`nf_nat_packet` to mangle it — queue placement / an implicit repeat needs
care. Not a blocker, just design.

Conclusion for now: the mark-based v2 (M1/M2/M3, proven, zero-userspace-code
kernel) is what we build; the verdict-DNAT patch is the documented north star
for a later kernel contribution.
