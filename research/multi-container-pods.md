# Multi-container pods: one VM per pod, one supervisor per VM

We are VM-per-pod, not VM-per-container. A pod's containers share a kernel,
so localhost, IPC, and /dev/shm behave like a Kubernetes pod *by
construction* — the pod boundary and the VM boundary are the same thing.
What multi-container support actually required was isolating the containers
*within* the VM (separate root filesystems) and supervising them
independently.

## Filesystems: one EROFS per image, one overlay per container

Each distinct image in the pod arrives as its own read-only virtio-blk EROFS
(the harness's `--root-image` is now repeatable; the Nth becomes
`/dev/vd{a+N}`). Containers that share an image share the device — and,
across pods, the same image file shares the host page cache. Per container,
execd assembles:

    /dev/vdX (erofs, ro)   ->  /lower-vdX      shared per image
    tmpfs                  ->  /rw-<name>      upper + work
    overlay(lower, upper)  ->  /roots/<name>   the container's chroot

plus proc/sysfs/devtmpfs/devpts inside each root, and one pod-wide tmpfs
bind-mounted at every root's /dev/shm (shared IPC, like a pod). Volumes are
one virtio-fs device per *volume*, mounted into every container that
declares a volumeMount — a shared emptyDir between containers is just two
mounts of the same device, which is exactly the official shared-volume
tutorial.

## Lifecycle: container restarts moved in-guest

The old model — VM exit means container exit, agent restarts the VM — can't
express "one sidecar crashed". So the supervision split is now:

- **execd** (in-guest) runs init containers sequentially (a failed init with
  a retrying policy re-runs; pod policy `Always` maps to `OnFailure` for
  inits, kubelet-style), then main containers in parallel, and restarts
  each container individually per restartPolicy with crash backoff. The VM
  exits only when the *pod* is done.
- **The agent** (host) polls execd's `status` op (5s, pushed to the API only
  on change) and reads a final `status.json` from the boot share when the VM
  exits. A VM that dies *without* writing final status is the pod-level
  analogue of a node crash: the agent restarts the whole VM (per
  restartPolicy, with backoff).

Probe-driven restarts also stay container-scoped: liveness/startup failure
sends execd a `kill` op (SIGTERM, SIGKILL after grace) for that one
container, and its in-guest supervisor restarts it. `kubectl exec`/`attach`,
exec probes, and `kubectl logs -c` all carry the container name now; execd
writes `/logs/<name>.log` per container into the boot virtiofs share, so the
host serves per-container logs directly.

VM sizing: memory is the sum of the main containers' limits (inits only
raise the floor), cpus likewise, with 256MiB/1cpu defaults.

## Validated against

- The official [shared-volume tutorial](https://kubernetes.io/docs/tasks/access-application-cluster/communicate-containers-same-pod-shared-volume/):
  nginx + debian writer over a shared emptyDir; `curl localhost` from the
  nginx container returns the debian container's page; pod settles at
  `1/2 NotReady` exactly like a kubelet.
- The official [init-containers example](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#init-containers-in-use):
  pod blocks at `Init:0/2` on cluster DNS until the Services are created,
  inits complete sequentially, then the app starts. (One adaptation: the
  namespace is hardcoded to `default` — we don't project the ServiceAccount
  namespace file yet.)
- Killing a main container's process restarts *that container* in-guest
  (RESTARTS increments, VM uptime unaffected, inits not re-run).
- Image dedup: 3× busybox containers → one block device; nginx + debian →
  two.

## Divergences (stated, not hidden)

- A container's writable overlay upper is **not** wiped on in-guest
  restarts; kubelet gives every restart a fresh container filesystem. Ours
  is fresh only when the VM itself restarts.
- Init containers that mount API-object volumes share the pod-level
  static-pod restriction (no configMap/secret volumes in static pods).
- Restart counts reset when the VM restarts (execd's counters die with it).

## A note on rate limits

The status-poll loop initially pushed UpdateStatus unconditionally every 5s
per pod, which saturated client-go's default QPS=5 and queued every API call
seconds deep — starving the lazy service-LB resolution path (guest-side 5s
deadline) and breaking ClusterIP traffic cluster-wide. Fixed twice over:
status pushes dedupe on a digest, and the agent now runs at kubelet-grade
QPS (50/100). The lesson generalizes: anything on the pod data path must not
compete with a chatty control loop for the same client.
