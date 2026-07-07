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
