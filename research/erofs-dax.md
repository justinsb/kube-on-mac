# EROFS + DAX: what it would buy us, and what's missing

Question (Justin, 2026-07-10): would DAX improve performance in conjunction
with EROFS?

Short answer: **not launch latency — memory density.** And the entire stack
is already DAX-ready except for one missing piece: libkrun has no
virtio-pmem device.

## The two DAX flavors, and which one pairs with EROFS

1. **virtiofs DAX** (`FUSE_DAX`): the guest maps a shared-memory window and
   the virtiofs daemon maps file extents into it — guest reads become
   direct access to host page cache, no copies, no guest page cache.
   libkrun supports this today (`krun_add_virtiofs3`'s `shm_size`; our
   harness already plumbs it as `--dax-mb`, unused). But it only covers
   virtiofs *shares* — for us that's the tiny boot dir and volumes, which
   are cold paths. Our container roots left virtiofs deliberately
   (case-sensitivity, ownership fidelity → EROFS block images).

2. **EROFS `-o dax` over virtio-pmem** (`FS_DAX`): the image file is mapped
   into the guest as a pmem region; EROFS in DAX mode maps file pages
   *directly* from it. Guest page cache for image content disappears —
   every pod on the host shares the **one** copy in host page cache. This
   is the flavor that pairs with our architecture, and it's the proven
   high-density design (Alibaba's runD runs EROFS images exactly this way;
   Kata does the virtiofs variant).

## Readiness check (measured 2026-07-10)

- Guest kernel (6.12.28): `CONFIG_VIRTIO_PMEM=y`, `CONFIG_LIBNVDIMM=y`,
  `CONFIG_FS_DAX=y`, `CONFIG_FUSE_DAX=y` — fully ready, nothing to rebuild.
- Our EROFS writer: DAX-compatible **by construction** — kernel EROFS only
  supports DAX for uncompressed data, and we write uncompressed flat-plain
  4K-block images (a happy accident of choosing the simplest format).
- libkrun: virtiofs DAX only; **no virtio-pmem API**. This is the single
  missing piece. (rust-vmm/cloud-hypervisor have implementations to crib
  from; Firecracker also lacks it.)

## What it would (and wouldn't) improve

**Launch latency: essentially nothing.** Image reads at pod boot are a few
MB pulled through virtio-blk from warm host page cache — milliseconds of
the ~500ms launch. The boot time lives in VM creation, kernel boot, and
network setup, not image I/O.

**Memory density: the real win.** Measured on idle nginx pods (186MB
image): each guest holds ~19MB of page cache, dominated by image pages
(binary, .so's, config) — duplicated per pod, on top of the single host
page cache copy, and each copy counts against that VM's memory. Host-side,
an idle nginx VM is ~17MB RSS, so image-page duplication is roughly a
third of the per-pod footprint — more for fatter runtimes (JVM, Python)
that fault in hundreds of MB of shared text. At 100 nginx pods, ~1.9GB of
duplicated cache would collapse to one ~19MB shared copy. DAX also
decouples guest memory pressure from image size: image pages stop
competing with the app for the VM's memory limit.

## If/when we do it

Per distinct image: attach the EROFS as a virtio-pmem device instead of
virtio-blk; execd mounts `-o dax`. The agent/execd changes are small (a
different device path and mount option); the work is adding virtio-pmem to
libkrun. Fits naturally with the per-layer-EROFS future work — both point
at the same end state: *container images as shared mappable memory, not
per-pod copies*.
