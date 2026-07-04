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
  (envtest binaries), writes an admin kubeconfig, registers the Node,
  heartbeats Ready, binds unassigned pods (stand-in for kube-scheduler), and
  reconciles bound pods into `podvm` processes. One pod = one microVM.
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
   OVERLAY_FS, VIRTIO_PMEM, BPF — the whole roadmap in one kernel.
3. Rootfs: Alpine minirootfs (aarch64) extracted to
   `_artifacts/rootfs-alpine`.
4. `make -C harness` (compiles + codesigns; if the linker recorded
   `libkrun.2.dylib`, re-point it:
   `install_name_tool -change libkrun.2.dylib @rpath/libkrun.dylib podvm`
   and re-sign).
5. Envtest binaries:
   `go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.21 use 1.33.0 --bin-dir _artifacts/envtest -p path`
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
  the image's Entrypoint/Cmd; image env vars/WorkingDir are not yet honored.
- **No pod networking**: no IPs, no services. TSI was deliberately disabled
  (it needs libkrunfw's patched kernel); the real design is routed IPv6 via
  virtio-net + guest-side service LB.
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
- **No probes, single container per pod, no volumes**, restartPolicy only
  honored as never-restart, control plane is envtest (no controller-manager,
  agent includes a 20-line bind-to-node "scheduler").

## Measured

- Guest kernel boot → workload exec → shutdown: ~37ms guest-clock on an
  M-series Mac (Kata kernel, 1 vCPU, 256MiB, virtio-fs root).
- Wall clock for `podvm` end-to-end (VMM setup + boot + run + teardown):
  ~0.2s.
- `kubectl apply` → pod `Running`: bounded by the agent's 1s reconcile poll,
  not the VM.
