# A kubelet for macOS: design sketch

Status: draft / research
Working name: `macos-kubelet` (placeholder)

## Thesis

Kubernetes doesn't support macOS nodes because containers require Linux kernel
primitives. If each pod runs in its own lightweight Linux VM (microVM), the
host OS stops mattering to the workload — macOS becomes a viable node host.
Apple's Containerization framework (the open-source `container` project,
2025) demonstrates the runtime layer is practical: per-container Linux VMs on
Virtualization.framework with sub-second boot and a minimal purpose-built
kernel.

The remaining work — and the subject of this doc — is everything *above* the
runtime: the kubelet contract, pod networking, and service routing.

We deliberately do not frame this as a virtual-kubelet provider. Virtual
kubelet's center of gravity is "pretend node backed by an external system,"
and its escape hatches (no real exec, weak probes, fake stats) are exactly the
things we must not skip. This is a **reimplementation of the kubelet API
contract for macOS**: a real node, with real pods, real pod IPs, and the full
kubelet server surface. (We may still vendor virtual-kubelet's node-controller
machinery as a library — it implements the node registration/lease/status
plumbing correctly — but the contract we hold ourselves to is kubelet's, not
VK's.)

## Alternative considered: node-per-microVM

Instead of pod-per-microVM, we could boot one larger Linux VM per *node* —
stock kubelet + containerd + CNI + kube-proxy inside, macOS as a hypervisor
(scaling by adding node VMs). That buys zero reimplementation and free
conformance, and it is the strongest argument against this design. It is
also already shipped: colima, lima+k3s, Docker Desktop, kind-on-Mac. Choosing
it reduces this project to VM packaging.

Pod-per-microVM is the version where macOS is genuinely a Kubernetes node:

- **Elastic footprint**: no statically preallocated node VM (the #1
  Docker Desktop complaint); per-pod memory + balloon + DAX sharing.
- **Hardware isolation per pod** (node-per-VM gives namespaces again;
  fixing that inside a node VM needs nested virt: M3+, macOS 15+).
- **Blast radius**: a guest kernel panic kills one pod, not the node.
- **No Linux node stack at all** — not just "no containerd": no systemd,
  kubelet, runc, CNI binaries, kube-proxy, no in-VM package manager or
  patch cadence. The guest inventory is a kernel binary plus a ~1MB static
  init; guests are immutable and stateless. Every node component is a
  macOS process we own. (If containerd's image machinery is ever wanted,
  its core builds on darwin and could slot in host-side without changing
  the pod-per-VM shape.)
- **Graceful degradation**: pod-per-VM can still boot a fat Linux VM as an
  auxiliary stock node when needed (or for conformance comparison); a
  node-per-VM design can never grow per-pod properties without doing all
  of this work anyway.

Cost, honestly: we own the kubelet contract surface and must track
Kubernetes releases. Mitigations: the contract moves slowly, Windows nodes
prove it's implementable off-Linux, and the conformance suite is the
objective finish line.

## Goals

- macOS machine joins an existing Kubernetes cluster as a worker node.
- Linux pods run unmodified: one pod per microVM, containers as processes
  inside the guest.
- Real, routable pod IPs — IPv6, single-stack preferred. No NAT between pods.
- Full `kubectl exec / logs / port-forward / attach / debug` support.
- Probes, graceful termination, restart policy, volumes (the common types),
  resource requests/limits honored via VM sizing.
- Service ClusterIPs work from pods with no userspace component on the data
  path after connection setup.

## Non-goals (for now)

- macOS-native workloads (macOS guests are heavyweight and EULA-capped at 2
  per host; a different problem).
- Running the control plane on macOS (apiserver/scheduler/controller-manager
  are pure Go and already run on darwin; uninteresting).
- CSI, device plugins, in-place resize, swap, full QoS/eviction parity —
  later.
- Windows pods, obviously.

## Architecture overview

```
                    ┌────────────────────────────── macOS host ─────────────────────────────┐
                    │                                                                       │
 apiserver ◄──────► │  macos-kubelet (Go)                                                   │
 (watch pods,       │  ├─ node controller: register, lease, status, conditions              │
  post status,      │  ├─ pod controller: reconcile bound pods → VM lifecycle               │
  10250 server)     │  ├─ kubelet server :10250 — exec/attach/logs/port-forward/stats       │
                    │  ├─ prober: liveness/readiness/startup against pod IPs                │
                    │  ├─ image manager: OCI pull → per-pod ext4 (APFS clonefile for COW)   │
                    │  ├─ volume manager: emptyDir / configMap / secret / projected         │
                    │  └─ service programmer: watch Services+EndpointSlices                 │
                    │        ├─► guest eBPF maps (via vsock)      [primary data plane]      │
                    │        └─► userspace splice proxy (host)    [NodePort/external only]  │
                    │                                                                       │
                    │  Virtualization.framework                                             │
                    │  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐              │
                    │  │ pod VM        │  │ pod VM        │  │ pod VM        │   … one per  │
                    │  │ linux kernel  │  │ linux kernel  │  │ linux kernel  │      pod     │
                    │  │ guest agent   │  │ guest agent   │  │ guest agent   │              │
                    │  │ c1 c2 (cgrps) │  │ c1            │  │ c1 c2 c3      │              │
                    │  └──────┬────────┘  └──────┬────────┘  └──────┬────────┘              │
                    │         │ virtio-net       │                  │                       │
                    │      ═══╧═════════ vmnet bridge / routed IPv6 ╧═══                    │
                    └───────────────────────────────────────────────────────────────────────┘
```

Components:

| Component | Runs | Notes |
|---|---|---|
| `macos-kubelet` | host, Go | the node agent; Code-Hex/vz bindings for Virtualization.framework (or a small Swift/XPC shim) |
| guest agent | in each pod VM, PID 1-ish | reuse candidate: Apple's `vminitd` (gRPC over vsock) or `kata-agent` (ttrpc); manages containers as cgroup-scoped processes inside the guest |
| network helper | host, root | vmnet setup (vmnet requires root or the restricted `com.apple.vm.networking` entitlement); NodePort splice proxy |

Key simplification we get for free: the guest is a full Linux kernel, so
*inside* the pod VM we have namespaces, cgroups, eBPF, nftables — everything.
Per-container isolation within a pod, resource shares between containers,
and (critically) the service data plane all live in the guest, where the
standard toolbox works.

## The kubelet contract, enumerated

What the rest of the cluster actually requires of a kubelet:

**Outbound (agent → apiserver)**
- Create/update Node object; report addresses (node IPv6), capacity,
  allocatable, conditions, nodeInfo.
- `coordination.k8s.io` Lease heartbeats.
- Watch pods bound to this node (`spec.nodeName` field selector).
- Pod status: phase, conditions (Ready, ContainersReady, PodScheduled),
  containerStatuses (state, restartCount, imageID, containerID), podIP(s).
- Events (pulled image, started container, probe failures, …).
- Optionally: CSR flow for serving certs (kubelet-serving signer) so the
  10250 endpoint has a cluster-trusted cert.

**Inbound (apiserver → agent, HTTPS :10250)**
- `/pods`, `/exec`, `/attach`, `/portForward`, `/containerLogs`, `/logs`,
  `/metrics`, `/metrics/resource`, `/stats/summary`.
- Delegated authn/authz: TokenReview + SubjectAccessReview against the
  apiserver, same as kubelet.
- Streaming: SPDY/WebSocket remotecommand protocol; terminate on host, proxy
  to guest agent over vsock.

**Behavioral**
- Graceful deletion: observe deletionTimestamp, honor
  terminationGracePeriodSeconds, preStop hooks, SIGTERM→SIGKILL in guest.
- restartPolicy / backoff; startup, liveness, readiness probes (exec probes
  run via guest agent; http/tcp probes from host against pod IP — pod IPs are
  really routable, so this is honest).
- Init containers, sidecar (restartPolicy: Always init) containers.
- Downward API, projected service account tokens, **service environment
  variables** (yes, still part of the contract — needs the service lister at
  pod start).
- imagePullPolicy, imagePullSecrets.
- Ephemeral containers (`kubectl debug`) — a new process in an existing VM;
  cheap for us, do it early, it's a great demo.

## Pod runtime: one pod per VM

- **Mapping**: pod → exactly one VM. Containers in the pod are processes in
  the guest, each in its own cgroup + (optionally) namespaces via a minimal
  in-guest runtime (crun). Shared network/IPC namespace within the pod is
  automatic — it's one kernel.
- **Boot**: minimal kernel + initramfs containing the guest agent. Decided:
  start from the kernel Apple's Containerization framework builds. Checked
  their `kernel/config-arm64` (2026-07): it already ships almost everything
  we need — `CONFIG_VIRTIO_FS`, `CONFIG_FUSE_DAX`, `CONFIG_FS_DAX(_PMD)`,
  `CONFIG_DAX`, `CONFIG_ZONE_DEVICE`, `CONFIG_VIRTIO_PMEM`,
  `CONFIG_OVERLAY_FS`, `CONFIG_BPF_SYSCALL` + `CONFIG_CGROUP_BPF`. The only
  gap for us is `CONFIG_EROFS_FS` (not set; squashfs is their read-only fs).
  So the config delta is essentially one option. Continue stripping
  unnecessary drivers over time — a smaller kernel is both boot time and
  per-VM memory. Target <1s pod-VM boot, which changes what "pending" feels
  like.
- **Images**: pull OCI images host-side into a read-only store; guests get
  the image as a read-only lower layer + tmpfs/ext4 writable upper via
  overlayfs. The lower-layer transport depends on the VMM (see the
  memory-sharing section): virtiofs+DAX share of the unpacked tree on
  libkrun; **EROFS** image on virtio-blk (one shared host file per image) on
  Virtualization.framework. Never ext4-copy-per-pod.
- **Sizing**: VM memory = pod limits (or a default for limitless pods) with a
  virtio balloon device to reclaim; vCPUs from CPU limits, cpu.shares inside
  the guest for per-container requests. Report allocatable conservatively:
  each VM carries fixed overhead (guest kernel + VMM, order tens of MB).
- **Volumes**:
  - emptyDir → tmpfs or ext4 in-guest (medium-dependent).
  - configMap/secret/downwardAPI/projected → host-materialized dirs shared
    read-only via virtio-fs; atomic-symlink update dance same as kubelet.
  - hostPath → virtio-fs from the mac filesystem; allowed but flagged (paths
    mean macOS paths — mostly useful for our own plumbing).
  - PVC/CSI → out of scope initially.
- **Logs**: guest agent streams container stdout/stderr over vsock; host
  writes rotated files, serves `/containerLogs`.

### Memory sharing across pod VMs (EROFS + DAX)

Decided: we prioritize per-VM memory reduction over pre-booted VM pooling.
Pooling only hides boot latency (already ~sub-second); shared memory attacks
the density ceiling, which is the number that decides whether this is a
20-pod or 200-pod node.

The plan, in order of increasing win:

1. **Slim kernel** (above): fewer drivers, smaller text, smaller boot-time
   allocations. Same kernel image file for every VM.
2. **Read-only EROFS images, one host file per image**: every pod VM running
   the same image reads the same backing file, so the *host* page cache holds
   each block once regardless of VM count. Without DAX, each guest still
   duplicates what it reads into its own guest page cache — better than
   ext4-per-pod-copy, but the guest-side duplication remains.
3. **DAX — the real prize**: map the EROFS image into a shared-memory window
   in guest physical address space; guests mount with `-o dax` and execute
   pages *directly from host page cache*. N pods running the same image share
   one copy of the image's hot pages, host-wide. Guest page cache for image
   content drops to ~zero. This is the Kata Containers / runD playbook
   (virtio-fs DAX windows, virtio-pmem + EROFS).

**VMM support for DAX — investigated 2026-07, source-verified:**

- **Virtualization.framework: no.** No virtio-pmem device, and its virtio-fs
  (`VZVirtioFileSystemDevice`) exposes no DAX/shared-memory-region API. On VZ
  we can do (1) and (2) but not (3).
- **libkrun: yes, shipping today.** Verified in the libkrun source (not just
  docs):
  - Public API: `krun_add_virtiofs2(ctx, tag, path, shm_size)` — `shm_size`
    is documented as "size of the DAX SHM window in bytes"
    (`include/libkrun.h`).
  - The macOS passthrough filesystem implements the FUSE
    `SetupMapping`/`RemoveMapping` ops
    (`src/devices/src/virtio/fs/macos/passthrough.rs`): it mmaps the host
    file and sends a mapping request to the VMM worker thread.
  - The worker (`src/vmm/src/worker.rs` → `src/vmm/src/macos/vstate.rs`
    `add_mapping`) remaps the window in guest physical address space via
    Hypervisor.framework. (The message is named `GpuAddMapping` — shared
    mechanism with the virtio-gpu shared-memory path — but the fs device
    uses it too.)
  - `krun_set_kernel(ctx, kernel_path, format, initramfs, cmdline)` boots an
    external kernel, so our Apple-config-derived kernel works on libkrun as
    well as VZ.

So the DAX experiment requires **zero VMM patches**: libkrun + a virtio-fs
share of the read-only unpacked image tree, mounted in the guest with
`-o dax`. Guests then execute image pages directly out of host page cache.

Two DAX-capable layouts, in adoption order:

- **v1: virtiofs + DAX over the unpacked image directory** (works today, no
  patches). EROFS is not on this path — DAX'd virtiofs shares page cache at
  file granularity without needing a block image. Cost: FUSE metadata round
  trips (lookup/getattr per file) and directory-tree semantics rather than a
  sealed image.
- **later, if warranted: EROFS over virtio-pmem** (the runD/Kata model —
  sealed uncompressed EROFS image mapped as a pmem window, mounted
  `-t erofs -o dax`). libkrun has no virtio-pmem device today, so this is a
  real (modest) VMM patch; notably the guest side is already ready — Apple's
  kernel config ships `CONFIG_VIRTIO_PMEM=y` + `CONFIG_ZONE_DEVICE=y`.
  Adopt only if FUSE metadata overhead or image sealing proves to matter.

Plan: build the VMM interface as a narrow abstraction from day one
(create/boot/vsock/net/blk/fs — we need little else). v1 can run on either
VMM; VZ gets strategy (2) (EROFS on virtio-blk, shared host file), libkrun
gets strategy (3). Given DAX ships in libkrun today, libkrun is the likely
primary — keep the VZ backend as the conservative fallback (it's the
platform-blessed API and Apple's Containerization uses it). Do not bet
milestone 1 on DAX.

## Networking: real IPv6 pod IPs

**v1 scope (decided): single machine, test /64, traffic need not leave the
host.** Grab a ULA /64 (e.g. carve from a generated fd00::/48 cluster
block) for the pod prefix; the host routes it locally between pod VMs and
itself. Everything below about cross-node routing and egress is deferred —
listed so the addressing scheme doesn't paint us into a corner.

Plan:

- Each node owns a routed IPv6 prefix, e.g. a /64 per node (matches the
  kube-controller-manager IPv6 node mask default). Source: a ULA block
  (fd::/48 for the cluster) if there's no delegatable GUA space; GUA if the
  network can route a prefix to the Mac.
- Pod VMs attach via virtio-net to a vmnet network; the host acts as the
  IPv6 router for the pod prefix (RA or static config pushed by the guest
  agent — static preferred, we own both ends).
- Data path is vmnet (kernel-mediated switching) — explicitly **not** a
  userspace switch like gvisor-tap-vsock; we keep userspace off the per-packet
  path everywhere, on principle.
- Pod-to-pod on the same node: bridged L2, no NAT. Pod-to-pod across nodes:
  routed via the node prefixes (each node advertises/NDP-proxies or the
  upstream router carries a route per node — same problem kOps-style
  cloud-route or BGP setups solve; simplest v1 is static routes or a tiny
  route-sync daemon between macOS nodes).
- Egress to the IPv4 internet from a v6-only cluster: NAT64/DNS64. Options:
  macOS's built-in developer NAT64 (Internet Sharing), or run Tayga/Jool in a
  dedicated pod. Open question; not needed for the first milestone.
- DNS: CoreDNS runs as ordinary pods (possibly on this same node); pods reach
  its ClusterIP via the service data plane below.

## Services: ClusterIP without a userspace data path

Requirement statement (from the project owner): each *connection* gets a
routing/DNAT decision, ideally by a userspace daemon; after that decision,
packets must flow with no userspace hooks.

### Where can the decision live on macOS?

| Option | Decision made by | Data path after decision | Verdict |
|---|---|---|---|
| pf `rdr` anchor on host | pf, in-kernel, from daemon-maintained rules (round-robin / random / source-hash pools) | kernel state table, zero userspace | ✅ good; daemon is on the *control* path only |
| NetworkExtension transparent proxy | our daemon, per flow | **all bytes proxied through userspace forever** | ❌ for pod traffic; acceptable fallback for host-originated only |
| Literal first-packet-to-daemon, then kernel takeover | our daemon | would require injecting flow state into the kernel | ❌ no public macOS API for this (no NFQUEUE/divert-reinject equivalent, no conntrack-insert) |
| **In-guest eBPF connect-time LB** (recommended) | guest kernel at `connect()`, from maps our daemon programs over vsock | packet leaves the VM already addressed to the backend pod IP; **no NAT state anywhere, no hooks host-side** | ✅✅ |
| Gateway/router VM (shared Linux VM doing nftables) | gateway VM kernel | extra hop for all service traffic; a fat shared VM re-introduces the thing we're avoiding | ❌ as primary; maybe later for ingress |

### Recommended design

**Primary (pod-originated traffic — which is nearly all service traffic):
source-side, connect-time translation inside each pod VM.**

- The host `service programmer` watches Services + EndpointSlices and pushes a
  compact VIP→backends map into every pod VM over vsock; the guest agent loads
  it into eBPF maps consumed by `cgroup/connect6` (+ `sendmsg6`/`recvmsg6` for
  UDP) hooks — the Cilium socket-LB model, but distributed per-pod-VM and
  programmed centrally from the mac.
- Effect: when a container calls `connect()` to a ClusterIP, the guest kernel
  rewrites the destination to a chosen backend pod IP before a single packet
  exists. Everything after is plain IPv6 routing. This *exceeds* the stated
  ideal: not only are there no userspace hooks after the first packet, there
  is no DNAT state and no service VIP on the wire at all.
- The "userspace decides" property is preserved at the policy level: the host
  daemon owns backend selection policy (weights, topology, drain) and the
  guest kernel just executes the map. If we ever want genuinely per-connection
  userspace decisions (custom LB), the guest is Linux — we can add a map-miss
  upcall to the guest agent — but default to in-map policy for simplicity.
- Fallback inside the guest if eBPF feels heavy for v1: plain nftables DNAT
  rules programmed the same way. Same zero-userspace data path; per-connection
  decision at first packet in the guest kernel; conntrack in the guest. Easier
  to ship, slightly worse (NAT state, VIP on the guest's own wire until
  rewritten).

**Secondary (host-originated + external traffic): userspace is acceptable
here.**

Decision: external/NodePort traffic is not expected to be high-performance on
these nodes, so we don't need to hold this path to the zero-userspace
standard — that standard applies to pod-to-service traffic, which the guest
data plane already satisfies.

- NodePort / LoadBalancer ingress, v1: a plain accept-and-splice proxy on the
  host (listen on the node port, dial the chosen backend pod IP, splice).
  Simple, portable, debuggable, and the daemon making the per-connection
  decision *is* the proxy — no kernel programming required.
- Optional later optimization: pf `rdr` rules in a `kube-services` anchor
  (stateful in-kernel DNAT after a first-packet decision from
  daemon-maintained pools). Only worth it if NodePort throughput ever
  matters; macOS pf is an old fork (OpenBSD ~4.3 era + Apple patches) and its
  IPv6 `rdr` behavior would need empirical verification first.
- Host processes reaching ClusterIPs (rare; our own probes use pod IPs, per
  contract): our components resolve endpoints via the API; anything else can
  go through the same userspace proxy.
- Endpoint removal cleanup: prune guest conntrack (nftables mode) / nothing
  needed (eBPF connect-time mode — one more reason to prefer it); the
  userspace proxy just stops dialing removed backends.

## Node identity: we report `os=linux`

Decided: the node reports `kubernetes.io/os=linux` (and `arch=arm64`). The OS
that runs pods is Linux — from a workload's point of view this is a Linux
node, exactly as gVisor and Kata nodes report `linux`. Reporting `darwin`
would break every `nodeSelector: kubernetes.io/os: linux` workload for the
sake of describing an implementation detail.

Consequence to manage: `linux` attracts DaemonSets (CNI, kube-proxy,
node-exporter) that must not run here. Mitigation: a node label
(`kube-on-macos.io/runtime=vm`) and a startup taint
(`kube-on-macos.io/vm-isolation=NoSchedule`) so only tolerating workloads land
until we trust the surface.

## Security & platform constraints

- Virtualization.framework needs the `com.apple.security.virtualization`
  entitlement (freely available); vmnet needs root or the *restricted*
  `com.apple.vm.networking` entitlement — plan on a small root helper.
- Isolation story is a feature: every pod is hardware-virtualized. Stronger
  than namespaces; worth stating in the README.
- Mac-specific operational realities: sleep/power management (nodes must
  disable sleep or handle NotReady gracefully), App Store review is
  irrelevant but SIP and TCC are not — file sharing via virtio-fs from
  protected locations will prompt.
- VM density: unknown practical ceiling on concurrent VZ VMs; memory overhead
  per VM bounds pods-per-node well below Linux norms. Measure early; balloon
  aggressively; this is the #1 "is this viable at all" number.

## Reuse candidates

- **Apple Containerization** (Swift): kernel config, `vminitd` guest agent,
  OCI pull + ext4 tooling. Interop cost: Swift↔Go boundary.
- **kata-agent**: battle-tested guest agent with exactly our pod-in-VM
  semantics (ttrpc over vsock). Rust; protocol is stable.
- **virtual-kubelet** node libraries: node/lease/status controllers only.
- **Code-Hex/vz**: mature Go bindings for Virtualization.framework.
- **Cilium**'s socket-LB eBPF programs as reference (not vendored).

## PoC results (2026-07, see ../poc/)

Milestone-0 proven end-to-end on an M-series Mac: `kubectl apply` against a
real kube-apiserver → node agent binds the pod → APFS-cloned Alpine rootfs →
libkrun/HVF microVM boots Kata's prebuilt kernel → pod reports Running →
workload output captured → Succeeded/Failed with real exit code on VM exit.
Numbers: **~37ms guest-clock from kernel boot to workload exec** (~0.2s wall
including VMM setup) at 1 vCPU / 256MiB over virtio-fs root.

Findings that feed back into this design:

- **Kata's prebuilt `vmlinux.container` (6.12.x) beats building our own to
  start**: VIRTIO_FS, FUSE_DAX, FS_DAX, EROFS, OVERLAY_FS, VIRTIO_PMEM, and
  BPF are all enabled — strictly more complete than Apple's config (which
  lacks EROFS). Zero kernel builds needed for the whole roadmap.
- libkrun main (2.0-dev) builds cleanly on macOS (rustup + musl target for
  the static guest init, llvm for bindgen) and boots external kernels.
- TSI networking requires libkrunfw's patched guest kernel — irrelevant to
  us (we want virtio-net + routed IPv6 anyway), but don't enable TSI flags
  with a vanilla kernel: the cmdline flag leaks into workload argv.
- Workload argv must go out-of-band (libkrun passes exec args via kernel
  cmdline, which splits on whitespace). libkrun's init natively reads an
  OCI-runtime-spec `/.krun_config.json` from the rootfs — that's our
  interface for the real agent, and it aligns with OCI image config.

## Milestones

1. **Pod runs** (single node, ULA test /64, host-local traffic only):
   hand-written Pod → VM boots (Apple Containerization kernel + our config
   delta, EROFS rootfs) → containers run → status Ready → `kubectl logs`
   works. Volumes: emptyDir + configMap/secret.
2. **Node is honest**: probes, graceful termination, restarts, exec/attach/
   port-forward, /stats/summary, node conditions under load.
3. **Network is real**: IPv6 pod IPs routable across ≥2 macOS nodes;
   pod-to-pod cross-node with no NAT.
4. **Services**: guest-side connect-time LB (nftables first, eBPF second);
   CoreDNS resolvable from pods; NodePort via the userspace splice proxy.
5. **Density**: measure per-VM overhead honestly (pods/node with and without
   shared images); balloon tuning; the DAX experiment on libkrun behind the
   VMM abstraction — go/no-go on switching primary VMM.
6. **Polish**: eviction basics, NAT64 egress story, `kubectl debug`,
   multi-node (cross-node /64 routing).

## Open questions

- Prefix delegation in real networks (post-v1): how do nodes get a routed
  /64? (ULA + static routes is the v1 answer; what's the good answer?)
- ~~libkrun virtio-fs DAX status on macOS~~ — **resolved**: ships today via
  `krun_add_virtiofs2(..., shm_size)`; mapping path source-verified down to
  Hypervisor.framework. virtio-pmem remains unimplemented in libkrun (only
  needed for the later EROFS-over-pmem variant).
- ~~Apple kernel config DAX/EROFS coverage~~ — **resolved**: everything
  present except `CONFIG_EROFS_FS`; delta is essentially one config option.
- Memory overhead per VM after ballooning + DAX sharing — the density
  ceiling; how much does DAX actually buy at, say, 50 pods over 5 images?
- virtiofs+DAX (file-granular, FUSE metadata costs) vs EROFS-over-pmem
  (sealed image, needs a libkrun virtio-pmem patch) — decide with density
  data in hand.
