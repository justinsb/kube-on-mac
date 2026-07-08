# kube-on-macos proof of concept

Kubernetes pods running in hardware-virtualized Linux microVMs on a macOS
host, reconciled by a node agent speaking to a real kube-apiserver.

What this demonstrates:

- **A Linux microVM boots on macOS in ~40ms** (guest kernel boot → workload
  exec, per the guest's own clock; ~0.2s wall including VMM setup), using
  libkrun on Hypervisor.framework with Kata Containers' prebuilt kernel.
- **The pod lifecycle round-trips through a real apiserver**: `kubectl apply`
  → agent binds the pod to the node → APFS-clones a rootfs → boots a microVM
  → reports `Running` → captures workload output → reports
  `Succeeded`/`Failed` with the real exit code on VM exit.
- The node registers as `os=linux/arch=arm64` (the OS that runs pods is
  Linux; the host being macOS is an implementation detail), matching the
  design in [../research/macos-kubelet.md](../research/macos-kubelet.md).

```
$ KUBECONFIG=poc/_artifacts/kubeconfig kubectl get nodes -o wide
NAME        STATUS   ROLES    VERSION                  OS-IMAGE                                    KERNEL-VERSION   CONTAINER-RUNTIME
macos-poc   Ready    <none>   kube-on-macos-poc-v0.1   Linux microVM (libkrun/HVF) on macOS host   6.12.28-kata     podvm://0.1

$ kubectl apply -f poc/demo/pod.yaml && kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE
hello-macos   1/1     Running   0          4s
```

## Layout

- `harness/podvm.c` — boots one microVM: external kernel (Kata
  `vmlinux.container`), rootfs directory over virtio-fs (with optional DAX
  window via `--dax-mb`), libkrun's built-in init execs the workload,
  stdout/stderr stream to the harness's stdio. Signed ad-hoc with the
  `com.apple.security.hypervisor` entitlement.
- `agent/` — the PoC node agent (Go). Boots kube-apiserver + etcd locally
  (envtest binaries) and a real kube-controller-manager, writes an admin
  kubeconfig, registers the Node, heartbeats Ready (status + node Lease),
  binds unassigned pods (stand-in for kube-scheduler), and reconciles bound
  pods into `podvm` processes. One pod = one microVM.
- `execd/` — the in-guest supervisor/exec daemon (Go, built static for
  linux/arm64 into `_artifacts/execd`; the agent clones it into each pod
  rootfs and `/entry.sh` execs it).
- `demo/pod.yaml` — example pod.
- `_artifacts/` (gitignored) — libkrun.dylib + header, guest kernel, base
  Alpine rootfs, envtest binaries, kubeconfig, per-pod state
  (`pods/<uid>/{rootfs,container.log,vmm.log}` — container.log is workload
  stdout/stderr only; VMM and guest-kernel diagnostics go to vmm.log).

## Building / running

Prereqs: Xcode CLT, Homebrew (`rustup`, `llvm`, `lld`, `xz`), Go.

1. Build libkrun (main branch) on macOS:
   `LIBCLANG_PATH=/opt/homebrew/opt/llvm/lib make BLK=1`
   (needs the `aarch64-unknown-linux-musl` rust target for the static guest
   init). Copy `target/release/libkrun.dylib` and `include/libkrun.h` into
   `_artifacts/`.
2. Guest kernel: Kata Containers' prebuilt `vmlinux.container` (arm64 raw
   Image) from the kata-static release tarball → `_artifacts/vmlinux-arm64`.
   Its config already includes VIRTIO_FS, FUSE_DAX, FS_DAX, EROFS,
   OVERLAY_FS, VIRTIO_PMEM, BPF. For **Services** you need nftables, which
   the Kata kernel lacks: build a custom kernel (Kata 6.12.28 .config + the
   NF_TABLES/NFT_* enable-delta) → `_artifacts/vmlinux-nft-arm64` and pass
   `--kernel` to the agent. See research/services.md for the exact delta.
3. Rootfs: Alpine minirootfs (aarch64) extracted to
   `_artifacts/rootfs-alpine`.
4. `make -C harness` (compiles + codesigns; if the linker recorded
   `libkrun.2.dylib`, re-point it:
   `install_name_tool -change libkrun.2.dylib @rpath/libkrun.dylib podvm`
   and re-sign).
5. gvproxy (outbound pod networking): build from
   github.com/containers/gvisor-tap-vsock (`go build ./cmd/gvproxy`) into
   `_artifacts/gvproxy`.
6. Envtest binaries:
   `go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.21 use 1.33.0 --bin-dir _artifacts/envtest -p path`
6. kube-controller-manager (envtest doesn't ship it; it's pure Go and builds
   natively on macOS):
   `git clone --depth 1 --branch v1.33.0 https://github.com/kubernetes/kubernetes`
   then in that tree
   `go build -o <poc>/_artifacts/kube-controller-manager ./cmd/kube-controller-manager`
   (or `--kube-controller-manager=''` to run without it — Deployments etc.
   will be inert).
6. `cd agent && go build -o agent . && ./agent --assets ../_artifacts/envtest/k8s/1.33.0-darwin-arm64`
7. In another shell:
   `export KUBECONFIG=$PWD/_artifacts/kubeconfig; kubectl get nodes; kubectl apply -f demo/pod.yaml`

Standalone harness smoke test (no Kubernetes):

```
./harness/podvm --kernel _artifacts/vmlinux-arm64 \
    --rootfs _artifacts/rootfs-alpine -- /bin/sh /entry.sh
```

(Workload argv travels as a script file in the rootfs: libkrun passes exec
args over the kernel cmdline, which splits on whitespace. The production
path is the OCI-style `/.krun_config.json` that libkrun's init also reads.)

## Honest accounting of what's faked

- **Image pull is real but flat**: images are pulled for linux/arm64 (via
  go-containerregistry), flattened to a rootfs dir, and cached under
  `_artifacts/images/`; pods get APFS clones. No layer store, no
  imagePullSecrets, anonymous registry auth only. Pods with no command use
  the image's Entrypoint/Cmd; image env vars and WorkingDir are honored (pod
  spec `env`/`workingDir` override them; `env.valueFrom` is not implemented).
  Pull-by-digest of a non-arm64 image fails with an explicit architecture
  error; pull failures keep the pod Pending in `ErrImagePull` with doubling
  backoff (kubelet-style) instead of failing the pod.
- **Host filesystem semantics leak into guests** (verified empirically):
  the rootfs is a virtiofs share of an APFS directory, so guests see APFS
  case-insensitivity (`/Foo` and `/foo` are the same file) and host
  ownership (image files appear as the host user's uid, and guest `chown`
  is not faithful). Workloads that depend on case-sensitivity or strict
  ownership (postgres data dirs, sshd strict modes) can misbehave. Fix
  (future work): serve the image as a real Linux filesystem in a block
  image (EROFS/ext4 lower + writable upper), keeping virtiofs for
  configMap/secret-style shares and DAX.
- **Pod networking: routed IPv6 pod IPs are real; services aren't yet.**
  Every pod VM has two NICs, both plumbed by execd via netlink (images
  can't be assumed to ship iproute2):
  - eth0 → per-pod gvproxy (userspace NAT): outbound IPv4 + DNS.
    `apt-get update`/`install` work from a debian pod.
  - eth1 → per-pod vmnet-helper on the macOS shared vmnet bridge
    (rootless on macOS 26): each pod gets a stable IPv6 from a ULA /64
    (derived from the pod UID), reported as the pod IP in status —
    `kubectl get pods -o wide` shows real, distinct addresses, and
    pod↔pod HTTP over IPv6 works (~0.4ms RTT). Bonus: macOS advertises a
    NAT66 prefix on the bridge, so pods have outbound IPv6 too.
  See research/vmnet.md for the architecture and the checksum-offload
  trap. host→pod needs one sudo (`./host-net.sh`); cross-node routing and
  gvproxy→vmnet IPv4 consolidation are future work.
- **ClusterIP Services work, lazily, with zero per-flow nftables churn.**
  IPv6 Services get a ClusterIP; the first packet of a new flow pops up from
  the guest kernel (NFQUEUE) to execd, which resolves endpoints from the
  agent (cached), picks a backend **in userspace** (round-robin), and sets a
  packet **mark** on the verdict. One static rule DNATs by mark through a
  `mark → addr:port` map; the map changes only when endpoints change, never
  per flow. Every later packet is handled in-kernel by conntrack. Verified:
  8 requests split 4/4 across two backends with a single "LB active" log line
  and no per-flow rule ops; deleting a backend moved all new flows to the
  survivor within the cache TTL (no pod restart — the old numgen-rule design
  went stale until restart). Requires a custom guest kernel with nftables
  (Kata config + NFT delta; see research/services.md for the mark design and
  the north-star verdict-DNAT kernel primitive). Still v2: OUTPUT-hook /
  pod-originated only (no NodePort); endpoint-removal conntrack flush is
  best-effort (shells out to `conntrack`, absent from most images) so
  long-lived flows to a removed backend aren't force-reset; no
  affinity/weights/topology; no named ports.
- **Cluster DNS is real, lazily, with no DNS server to run.** execd serves
  DNS on each pod's loopback (kubelet-standard resolv.conf: search path +
  ndots:5); `<svc>.<ns>.svc.cluster.local` resolves over the same vsock
  channel as the service LB (agent answers from apiserver state, 5s TTL),
  everything else forwards to gvproxy's resolver. `redis-server --replicaof
  redis-leader 6379` and PHP/Predis both just work (see
  docs/walkthrough.md). No headless-service endpoints, SRV, or pod records.
- **The real kube-controller-manager runs** (built from the kubernetes tree
  for darwin/arm64 — envtest doesn't ship it; the agent spawns and
  supervises it, wired to envtest's service-account signing key). So
  Deployments → ReplicaSets → pods, rolling updates (`kubectl rollout`),
  EndpointSlices, garbage-collection cascades, namespace deletion, and
  default-ServiceAccount creation are all the genuine articles; the agent
  renews a node Lease (kubelet contract) so the node-lifecycle controller
  stays happy. The full guestbook tutorial runs on this, rolling updates
  included — see docs/walkthrough.md.
- **Partial kubelet server**: `kubectl logs` (with `-f`, `--tail`),
  `kubectl exec` (including `-it` with pty + resize + exit codes), and
  `kubectl attach` (so `kubectl run -it --image=debian:latest -- bash`
  gives an interactive root shell in a microVM) all work, served on :10250
  using kubelet's own streaming library. Still missing: authn/authz on the
  endpoint (delegated TokenReview/SubjectAccessReview), `port-forward`,
  and multi-attach (one attach session at a time).
- **Probes and lifecycle are real**: startup/readiness/liveness probes with
  thresholds/periods/initialDelay; exec probes run via the exec channel,
  httpGet/tcpSocket probes run *inside* the guest via execd (the moral
  equivalent of kubelet probing the pod IP — localhost in the pod VM is the
  pod's network view). Graceful termination delivers SIGTERM in the guest
  and escalates to SIGKILL after the grace period (host-side harness kill
  as backstop). restartPolicy is honored with a naive doubling crash
  backoff; each restart is a fresh microVM with a fresh container
  filesystem (re-cloned from the image base — APFS clonefile makes this
  ~free), matching kubelet semantics. Named probe ports and lifecycle
  hooks (postStart/preStop) are not implemented.
- **In-guest supervision** is `execd` (poc/execd): a static Go daemon that
  libkrun's init execs; it runs the workload (on a pty when the pod sets
  `tty: true`), mirrors output to the console log, and serves exec/attach
  over vsock (guest port 1024 ↔ host unix socket in /tmp — sun_path is
  ~104 bytes on macOS, so the deep per-pod dir can't hold it).
- **Static pods and mirror pods are real.** `--manifest-dir` (default
  `_artifacts/manifests`) is watched: manifests start pods with *no
  apiserver involved* — verified running before envtest finishes booting —
  manifest edits restart them (a changed manifest is a different pod, fresh
  deterministic UUID from the content hash), removals stop them. Once the
  API is up each static pod gets a kubelet-style mirror pod
  (`<name>-<node>`, `kubernetes.io/config.*` annotations) with live status;
  logs/exec work through it, and deleting it never touches the static pod —
  the file is the source of truth. This is the bootstrap primitive for
  running the control plane itself as pods
  (research/static-pod-control-plane.md). Start failures now retry per
  restartPolicy instead of failing the pod (kubelet semantics; nothing
  exists to replace a failed static pod).
- **Bootstrap VIPs and hostPorts give static pods stable addresses.** A
  static pod may declare `kube-on-macos.io/cluster-ip: <v6-in-svc-cidr>`:
  the agent resolves that VIP to the pod from its own table — before, and
  independent of, any apiserver (the NFQUEUE data plane needs only the
  answer, and it takes precedence over API services). Once an apiserver
  exists the VIP is claimed as a real Service (mirror-pod philosophy: a
  reflection, not a dependency — deleting it changes nothing and it is
  re-claimed; it dies with the manifest). Verified: clients ride a manifest
  edit that changes the pod IP with zero configuration change — the VIP is
  what goes in kubeconfigs and cert SANs, pod IPs appear nowhere.
  `containers[].ports[].hostPort` is honored via each pod's gvproxy control
  API: 127.0.0.1:<hostPort> on the macOS host forwards into the pod, no
  sudo, re-created with the pod on restart (the future
  127.0.0.1:6443 → apiserver path). Divergence: forwards bind 127.0.0.1
  only, and hostPort is TCP-only.
- **Volumes: hostPath and emptyDir only.** Each volume is its own virtio-fs
  share (readOnly enforced VMM-side and with MS_RDONLY in the guest);
  emptyDir lives under the pod state dir, surviving container restarts and
  dying with the pod. Verified with real etcd (official arm64 image) keeping
  its data across pod deletion. hostPath types File/Socket/etc., subPath,
  configMap/secret/downward/projected volumes, and PVCs are not implemented
  — the ServiceAccount token volume that admission injects into every pod is
  skipped, everything else unsupported fails the mount (pod stays Pending in
  FailedMount with backoff).
- **Single container per pod**; control plane is envtest
  apiserver+etcd plus a real kube-controller-manager; still no
  kube-scheduler (the agent's bind-to-node loop stands in).

## Measured

- Guest kernel boot → workload exec → shutdown: ~37ms guest-clock on an
  M-series Mac (Kata kernel, 1 vCPU, 256MiB, virtio-fs root).
- Wall clock for `podvm` end-to-end (VMM setup + boot + run + teardown):
  ~0.2s.
- `kubectl apply` → pod `Running`: bounded by the agent's 1s reconcile poll,
  not the VM.
