# vmnet on macOS: data plane notes for pod networking

Status: research notes, 2026-07 (macOS 26 era)
Context: milestone 3 (routed IPv6 pod IPs) of [macos-kubelet.md](macos-kubelet.md).

## Why vmnet at all: real IPs still need a wire

"Real pod IPs" is an L3 plan; it says nothing about how ethernet frames get
off a pod VM. A virtio-net device is a queue of raw frames in guest memory —
something on the host must switch them between pod VMs, the host stack, and
(eventually) a physical NIC. On Linux that's tap + bridge. macOS has no tap
devices and no AF_PACKET; the platform's virtual switch is the **vmnet
framework**: interfaces join an OS-managed bridge (e.g. bridge100/101), the
host and all VMs share that L2, and the OS does the switching. Our IPv6
addressing rides on top untouched — vmnet is the wire, not a middlebox.

Contrast with gvproxy (our interim outbound NAT): gvproxy *terminates* guest
TCP connections in a userspace netstack and re-originates them from host
sockets — which is why every gvproxy pod has the same fake address and pods
can't talk to each other. A vmnet path has no protocol logic at all.

## The permission landscape changed in macOS 26

- **macOS 15 and earlier**: vmnet requires root or the Apple-restricted
  `com.apple.vm.networking` entitlement. Hence "privileged network helper"
  in the original design doc.
- **macOS 26+**: vmnet is rootless. Plain user processes can create vmnet
  interfaces. The design doc's root-helper caveat is obsolete on current
  macOS.

## The players

- **vmnet framework**: in-process C API (`vmnet_start_interface()` hands the
  *calling process* a frame channel). Not a socket — a VMM must either link
  it or talk to something that does.
- **vmnet-helper** (nirs/vmnet-helper): a small adapter process that owns a
  vmnet interface and pumps opaque frames to/from a unixgram socket — the
  exact interface libkrun's `krun_add_net_unixgram` documents. No TCP stack,
  no NAT, no addressing opinions. One helper process per VM interface.
  Rootless on macOS 26 (brew-installable); on ≤15 it needs the sudoers
  install. Benchmarks ~10× faster than gvproxy/socket_vmnet.
- **VZ native vmnet (macOS 26)**: Virtualization.framework can attach a VM
  directly to a vmnet "virtual network" with **native packet forwarding** —
  no application code on the frame path, 3–6× faster than vmnet-helper and
  lower CPU. This is a VZ feature; HVF-based VMMs (libkrun, QEMU) cannot use
  it.
- **vmnet-broker** (nirs/vmnet-broker): NOT a data path. A virtual network
  reservation belongs to the process that created it; the broker is a shared
  XPC service that creates networks by name and shares them, so multiple VMM
  processes (VZ-native and helper-based alike) can join one L2. Apple's
  `container` uses the same pattern privately.

## Can libkrun skip the helper? (investigated)

No — checked libkrun source (2026-07): its only network backends are
unixstream/unixgram (+ tap on Linux); the only vmnet mentions are references
to socket_vmnet and vmnet-helper as external socket peers. The vmnet-helper
architecture doc is explicit: "Without vmnet-helper, VMs using QEMU or
libkrun cannot join a native vmnet network used by the Virtualization
framework."

So for the libkrun-based PoC, vmnet-helper is the integration-ready path,
and the per-frame cost is: guest ↔ VMM (virtio) ↔ unixgram ↔ helper ↔ vmnet.
A userspace copy hop remains — frame shuttling only, no protocol processing.

## Paths to eliminating the dependency later

1. **Contribute a native vmnet backend to libkrun.** On macOS 26 vmnet is
   rootless, so the VMM can call `vmnet_start_interface()` itself — no
   privileges, no helper process, one less copy hop. (It still won't match
   VZ's native forwarding, which wires virtio queues to vmnet inside the
   framework — that trick is VZ-only.) Contribution-sized; likely welcome
   upstream given krunkit ships vmnet-helper integration today.
2. **VZ backend for the harness.** Our VMM abstraction keeps this open; VZ
   gets the full native path. Trade-off: VZ has no virtio-fs DAX (the
   density lever) — so this is only attractive if DAX disappoints or Apple
   adds a DAX equivalent.
3. **vmnet-broker adoption** is orthogonal: worth it if/when we want pod VMs
   to share an L2 with VZ-based VMs (e.g. Apple `container` workloads) or
   want named/custom subnets rather than the system shared network.

## As-built findings (things that bit us)

- **Checksum offload mismatch, the ping-works-TCP-hangs trap**: libkrun's
  `COMPAT_NET_FEATURES` offers VIRTIO_NET_F_CSUM/TSO, so guests emit TCP
  with partial checksums expecting the backend to complete them. gvproxy's
  netstack does; vmnet-helper by default does not (`--enable-checksum-offload`
  and `--enable-tso` are opt-in). Result: ICMPv6 (software-checksummed)
  flows while every TCP SYN is silently dropped by the receiver. Fix: offer
  `features=0` on the vmnet NIC (guest computes full checksums), or enable
  the helper's offload flags to match. We chose features=0; revisit for
  throughput later.
- **macOS advertises a NAT66 prefix on the shared bridge** (the helper
  reports it as `vmnet_nat66_prefix`); guests pick up a SLAAC address from
  it and get working outbound IPv6 through Apple's NAT66 — verified ping to
  2001:4860:4860::8888 from a pod. Free v6 egress; also means pods have two
  global-scope v6 addresses (ULA ours, NAT66 theirs) — source-address
  selection picks the ULA for fd00::/8 destinations (longest match), so
  pod-to-pod is unaffected.
- **Pod-to-pod RTT ~0.4ms** across two microVMs via the vmnet bridge
  (ICMPv6, no tuning, no offloads).
- Host→pod needs the host to hold an address in the pod /64 on bridge100
  (`ifconfig bridge100 inet6 alias …` — root, see poc/host-net.sh) —
  bridge100 exists only while at least one vmnet interface is up, so the
  alias must be reapplied after idle periods.

## Integration plan for milestone 3 (as built)

- One vmnet-helper per pod VM in `--operation-mode shared`: every helper
  interface lands on the same macOS shared bridge, so all pods + the host
  share one L2 without needing the broker.
- The helper reports the vmnet-assigned MAC on startup (JSON on stdout);
  the harness sets that MAC on the guest's virtio-net device.
- Pod addressing: agent assigns each pod a static IPv6 from a ULA /64
  (fd… — the "test /64" from the design doc); execd configures it via
  netlink on eth1. The host gets ::1 in the same /64 on the bridge.
  Pod↔pod and host↔pod are on-link — no NAT, no routes needed for v1.
- gvproxy remains on eth0 for outbound IPv4 (macOS shared-mode NAT could
  replace it later — it hands out DHCPv4 on the same bridge — but that
  needs a DHCP client in execd; consolidation deferred).
