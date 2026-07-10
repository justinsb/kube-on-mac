# Is libkrun the right VMM? The macOS virtualization landscape (2026-07)

Question (Justin): happy with libkrun, but is it the right approach —
extend it, wait for it to be extended, or switch tech? Does Apple ship a
VM command?

## What Apple ships

Apple does **not** ship a general-purpose VM CLI. The stack is:

- **Hypervisor.framework (HVF)** — the kernel-level API (vCPUs, memory
  maps). Everything below sits on it, including libkrun.
- **Virtualization.framework (VZ)** — the high-level Swift/ObjC API:
  virtio-blk/fs/net/balloon/vsock, direct Linux kernel boot
  (`VZLinuxBootLoader`), native NAT networking, Rosetta translation for
  amd64 binaries inside arm64 guests. API only — the CLIs on top (vfkit,
  Lima, Tart, UTM) are all third-party.
- **`container` 1.0** (June 2026, Apache 2.0, Swift, macOS 26 + Apple
  Silicon only) — Apple's shipped CLI for *container* VMs, built on their
  **Containerization** framework: one lightweight VM per container, custom
  minimal kernel config, `vminitd` (a Swift init — their execd),
  sub-second starts, EXT4 block images built from OCI layers.

`container` is the headline: **Apple independently converged on this
project's architecture** — microVM-per-workload, custom kernel, purpose-
built guest init, block-device images. Strong validation of the thesis;
also the thing to watch. Differences that keep this project distinct: they
are VM-per-*container* (no pod grouping — our multi-container pods share a
VM/kernel, which is what makes them faithful pods); no Kubernetes surface;
EXT4 images vs our shared read-only EROFS.

## The contenders, against our actual requirements

Requirements: library-or-CLI control of device config; direct kernel boot
(our 0.5s pod launch); multiple read-only virtio-blk disks (per-image
EROFS); multiple virtiofs shares; vsock ports bridged to host unix sockets
(exec/attach/probes/status/service-LB all ride this); two NICs on host
sockets (gvproxy + vmnet); someday virtio-pmem for DAX.

| | libkrun (current) | VZ / Containerization | QEMU (hvf accel) |
|---|---|---|---|
| Control surface | C library, we own every device (podvm.c ≈ 300 lines) | Swift/ObjC API; framework opinions | CLI flags, everything configurable |
| Direct kernel boot | yes (proven ~0.5s pod launch) | yes (`VZLinuxBootLoader`, container proves sub-second) | yes, but heavier process/device init |
| vsock→unix bridging | yes (`krun_add_vsock_port`) | yes (`VZVirtioSocketDevice`) | yes |
| Native NAT networking | no (we run gvproxy per pod) | **yes** — could delete gvproxy, maybe vmnet-helper | user-mode net (slirp) |
| Rosetta (amd64 images) | no | **yes** — VZ-exclusive | TCG emulation (slow) |
| virtio-pmem / DAX | no — but OSS Rust, extendable | no, and closed: can't add it ourselves | **yes** — the only macOS option today |
| Balloon / memory reclaim | no | yes | yes |
| macOS support | 13+ | container needs 26 | any |

## Assessment

**libkrun remains the right primary.** It's the only option that is both
minimal *and* ours to extend: when we want virtio-pmem, that's a tractable
upstream contribution to an active OSS Rust project (rust-vmm and
cloud-hypervisor have implementations to crib from) — versus VZ, where the
device list is Apple's and closed. "Wait for it to be extended" is the
wrong frame; nobody else needs it on macOS yet.

**The real hedge is architectural, and we already have it**: everything
VMM-specific lives behind the podvm harness's flag interface. Agent and
execd don't know libkrun exists. A `podvm-vz` (Swift, on Virtualization
.framework or Apple's Containerization) implementing the same flags would
be a small experiment, and would buy native NAT (delete per-pod gvproxy),
Rosetta for amd64 images, and ballooning — worth doing the day one of
those matters more than DAX.

**QEMU's role is narrow but useful**: it has virtio-pmem *today*, so a
one-off QEMU experiment is the cheap way to measure the EROFS+DAX density
win ([erofs-dax.md](erofs-dax.md)) before investing in a libkrun device.

**Watch apple/container.** If Containerization gains pod-shaped VM
grouping or their kernel/vminitd stack races ahead, the harness boundary
is where we'd adopt it — as a backend, not a rewrite.
