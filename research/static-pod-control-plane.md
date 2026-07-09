# Self-hosting the control plane as static pods (without kubeadm)

Status: draft / research

## Thesis

The control plane should run as static pods on the macOS node, from the
official multi-arch `registry.k8s.io` images — and the bootstrap should be
**legible**: a Kubernetes cluster at rest is nothing more than a small set of
plain files (keys, certs, kubeconfigs, pod manifests) plus an etcd data
directory. If we can enumerate every one of those files and say why it
exists, then "creating a cluster" stops being magic: write the files, start
the kubelet.

kubeadm is the cautionary tale, not the template. It solved the "kube-up is
8000 lines of bash and salt" problem, but replaced it with a black box:
phases, hidden defaulting, config uploaded into a ConfigMap *behind the
apiserver it configures*, certs materialized by machinery you can't read.
The actual requirements are small — **PKI, network connectivity, and a
persistent volume for etcd** — and this note tries to write them all down
explicitly. If the inventory below is complete, our bootstrap tool can be a
couple hundred lines that a reader can hold in their head.

Why move off the current setup at all: envtest is invisible machinery of our
own (certs and kubeconfigs appear from nowhere, etcd is a temp dir wiped on
every agent restart), and it forced us to build kube-controller-manager from
source because upstream publishes no darwin server binaries — a treadmill
that gets worse with every component (scheduler, CoreDNS, …). Official
linux/arm64 images running in microVMs is exactly what this node already
does; the control plane should be no different. It is also the strongest
version of the project's thesis: the macOS host is *just a node*.

## What a cluster actually is: the file inventory

Everything in one place, say `/etc/kubernetes/` (repo-relative for the PoC).
The bootstrap tool's entire job is to write these files idempotently.

### PKI (`pki/`)

One cluster CA to start. (Production splits this three ways — cluster CA,
etcd CA, front-proxy CA — so that compromise of one plane doesn't own the
others; we start with one and note the collapse.)

| file | why it exists |
|---|---|
| `ca.crt`, `ca.key` | trust root: apiserver verifies client certs against it; every kubeconfig embeds `ca.crt` to verify the apiserver |
| `apiserver.crt`, `.key` | apiserver serving cert. SANs must cover every name a client uses: `127.0.0.1` (host-side forward), the apiserver pod's IPv6, the `kubernetes` service ClusterIP, and `kubernetes.default.svc.cluster.local` & friends |
| `sa.key`, `sa.pub` | ServiceAccount token signing keypair (apiserver signs & verifies; KCM's token controller signs with the private half) |
| `admin.crt`, `.key` | CN=`admin`, O=`system:masters` → `admin.kubeconfig` |
| `controller-manager.crt`, `.key` | CN=`system:kube-controller-manager` (RBAC binds this user to exactly the controllers' powers) |
| `scheduler.crt`, `.key` | CN=`system:kube-scheduler` |
| `agent.crt`, `.key` | the node agent's client identity. Today (via envtest) the agent is `system:masters`; the honest identity is CN=`system:node:macos-poc`, O=`system:nodes` + the Node authorizer. Start with masters, note the downgrade path |
| `apiserver-kubelet-client.crt`, `.key` | apiserver's client cert for connecting to the kubelet's :10250 (logs/exec). The agent's kubelet server currently checks nothing; verifying this cert is the natural first authn there |
| `etcd-server.crt`/`.key`, `etcd-client.crt`/`.key` | see the etcd security decision below — likely needed, because our pod network is a shared L2 |

The certificate *identities* (CN/O) are the legibility payoff: RBAC group
membership is literally a string in a file you can `openssl x509 -text` —
this is where kubeadm's opacity hurts most, and where writing it down helps
most.

### Kubeconfigs (4 files)

`admin.kubeconfig`, `controller-manager.kubeconfig`, `scheduler.kubeconfig`,
`agent.kubeconfig` — each is just (server URL, ca.crt, client cert+key).
Four small files that state exactly who talks to the apiserver as what.

### Static pod manifests (`manifests/`)

`etcd.yaml`, `kube-apiserver.yaml`, `kube-controller-manager.yaml`,
`kube-scheduler.yaml` — plain pod specs, checked into the repo,
human-readable. The component flags *are* the cluster configuration; nothing
is generated or defaulted at runtime. hostPath mounts bring in `pki/` and
etcd's data dir.

### State

`/var/lib/etcd` (hostPath) — the only mutable state in the cluster. Persisting
it fixes today's "every agent restart wipes the cluster" behavior for free.

## Network connectivity: enumerate the flows

Most distributions dodge this section with `hostNetwork: true` — control
plane pods share the node's network namespace, so everything is
`127.0.0.1`/node-IP. **That doesn't map here and we shouldn't pretend it
does**: every pod is its own kernel, and "the host" is macOS — there is no
host netns to join. Instead of emulating hostNetwork, enumerate the actual
flows. (Arguably this is *more* legible: hostNetwork is exactly the kind of
implicit assumption that makes bootstrap opaque.)

| flow | how |
|---|---|
| kubectl (host) → apiserver (pod) | gvproxy port-forward `127.0.0.1:6443` → guest (gvproxy is already per-pod and supports forwards; no sudo, unlike the `host-net.sh` bridge alias, which evaporates when bridge100 is recreated) |
| agent (host) → apiserver (pod) | same forward — the agent is just another client once bootstrap hands it a kubeconfig |
| KCM / scheduler (pods) → apiserver (pod) | routed IPv6, pod→pod — already works. Needs a **stable apiserver address** for kubeconfigs and cert SANs → pinned pod IPs (trivial: the agent already derives pod IPv6 from UID; static pods get a deterministic address, e.g. derived from the manifest name or an explicit annotation) |
| apiserver (pod) → etcd (pod) | pinned IP; transport security decided below |
| apiserver (pod) → kubelet (host :10250) for logs/exec | **pod→host, easy to forget.** Today the apiserver is on the host so 127.0.0.1 works. From a pod, the host is reachable at gvproxy's host gateway (192.168.127.254) or the vmnet host address; the Node's registered InternalIP must change from `127.0.0.1` to a pod-reachable address |
| apiserver (pod) → webhooks / aggregated APIs (pods) | *improves*: today the host apiserver can't reach pod IPs without the sudo bridge alias; an apiserver-in-a-pod reaches pods natively |

### The etcd security decision

On a normal control-plane host, apiserver→etcd over localhost is plausible.
Here the flow crosses the shared vmnet L2 — **any pod on the bridge could
reach a plaintext etcd**, and etcd access is cluster-admin (it bypasses RBAC,
authn, admission — everything). Options:

1. **etcd client-cert TLS** (3 more files in the inventory) — probably right;
   legibility survives, and it's the same trade real clusters make.
2. Colocate etcd in the apiserver pod and bind to localhost — blocked on
   multi-container pods (the PoC is single-container), and couples their
   lifecycles.
3. Plaintext + "it's a PoC" — fine for a first cut *if flagged loudly*, but
   it undermines the "node is real" story.

## Bootstrap, end to end

The whole sequence, no phases:

1. `pki` tool (a small Go program; goal: ~200 legible lines) writes
   `pki/*` and the four kubeconfigs. Idempotent; every file's purpose is a
   comment.
2. The static pod manifests are already in the repo — copy/edit, don't
   generate.
3. The agent starts, scans `manifests/`, and boots those microVMs **before
   any apiserver exists** — that's the definition of static pods — and sets
   up the `127.0.0.1:6443` forward.
4. When the apiserver answers, the agent registers the Node and posts
   **mirror pods** so the control plane is visible to kubectl (deleting a
   mirror pod must not kill the static pod — kubelet contract).
5. Everything else is ordinary reconciliation. etcd's hostPath means agent
   restarts now *resume* the cluster instead of erasing it.

What we deliberately don't take from kubeadm, even conceptually: phases,
config-uploaded-to-a-ConfigMap (cluster config living behind the apiserver
it configures), join tokens (single node), and runtime defaulting. If a
value matters, it appears verbatim in a file above.

## Prerequisites and sequencing

Each step lands value on its own; the flip comes last.

1. **hostPath volumes** (virtiofs) — the hard dependency, and independently
   the biggest missing kubelet feature (unlocks configMap/secret volumes,
   SA token projection). Caution to measure: etcd is fsync-heavy; verify
   virtiofs fsync durability and throughput on APFS before trusting it with
   the cluster's only state.
   **DONE (2026-07-08):** hostPath (Directory/DirectoryOrCreate) + emptyDir,
   one virtio-fs device per volume, readOnly enforced VMM-side. Acceptance
   test passed: official `registry.k8s.io/etcd:3.5.15-0` (arm64) in a
   microVM on a hostPath data dir — key written, pod deleted and recreated,
   key survived. fsync numbers below. (Bonus find: a day-one execd race
   silently dropped fast-exiting workloads' output; extra virtiofs device
   threads exposed it. Fixed.)
2. **Static pods + mirror pods** in the agent, proven with something
   trivial (a static nginx) before the control plane rides on it.
   **DONE (2026-07-08):** manifest-dir watcher; pods verified running
   *before* envtest's apiserver finished booting; kubelet-style mirror pods
   (`<name>-<node>`, `config.mirror/hash/source` annotations, live status,
   logs/exec); mirror deletion leaves the static pod untouched (same VM
   pid); manifest edit = new pod (content-hash UUID), removal = graceful
   stop. Start failures retry per restartPolicy — a static pod has no
   ReplicaSet to replace it, so a transient vmnet hiccup must not be fatal.
3. **Pinned pod IPs** and the **gvproxy 6443 forward** (both small).
   **DONE (2026-07-08), with a better design than pinning:** static pods
   declare a **bootstrap ClusterIP** (`kube-on-macos.io/cluster-ip`, inside
   the service CIDR so the NFQUEUE data plane intercepts it). The agent
   answers VIP→pod from its own table — no apiserver involved, which breaks
   the circularity that forces real clusters onto hostNetwork+nodeIP for
   control-plane kubeconfigs. Pod IPs appear in no kubeconfig and no cert
   SAN: verified by editing the manifest (pod IP changed, VIP constant,
   clients unaffected). Once an apiserver exists the VIP is claimed as a
   real Service (a reflection, not a dependency — mirror-pod philosophy;
   deleting it costs nothing, it is re-claimed, and it dies with the
   manifest). Host-side, `containers[].ports[].hostPort` is honored via
   gvproxy's control API: 127.0.0.1:<hostPort> forwards into the pod, no
   sudo, re-created with the pod — live before the apiserver was up. The
   flip's kubeconfigs therefore use `https://[VIP]:6443` in-cluster and
   `https://127.0.0.1:6443` from the host, and both stay valid across
   control-plane upgrades (a manifest edit).
4. **`pki` tool** replacing envtest's invisible cert machinery.
   **DONE (2026-07-08), more declarative than sketched:** each cert/keypair/
   kubeconfig is a YAML spec file with the generated material alongside it
   (`apiserver.yaml` → `apiserver.crt` + `.key`), cert-manager field names,
   reconciled kubectl-apply style — missing/matching/drifted, reasons
   logged, CA rotation cascading through children and kubeconfigs. The full
   inventory from this note now exists as commented specs in
   `poc/etc/kubernetes/`; identities (CN/O) are read directly from the
   files. Acceptance: real etcd as a static pod with `--client-cert-auth`
   on the generated material — writes/reads through its bootstrap VIP with
   the apiserver-etcd-client cert (proving the VIP SAN), rejected without a
   client cert. ~550 lines including the Kubeconfig renderer; the "~200
   legible lines" estimate held for the certificate core.
5. **The flip**: control plane from official `registry.k8s.io` arm64 images
   via `manifests/`, then delete envtest and `agent/kcm.go`. kube-scheduler
   arrives here as a static pod on day one — it never needs a darwin build.
   **DONE (2026-07-08).** The agent boots etcd → kube-apiserver → KCM +
   kube-scheduler from `etc/kubernetes/manifests/` and joins the cluster it
   just started. envtest, the darwin KCM build, and the bind-loop scheduler
   stand-in are all deleted. Verified: full guestbook on the self-hosted
   control plane (real scheduler binding, per-controller RBAC), and — the
   payoff — **agent restarts resume the cluster** (deployments, services,
   and running workloads all reappear from persistent etcd; only ephemeral
   container state resets, as it should).

## Findings from the flip

Composing proven pieces still surfaced real bugs; recording them because
each is a lesson about the boundaries between the pieces:

- **Distroless images broke the boot shim.** Every pod was launched via
  `/bin/sh /entry.sh`; the control-plane images have no shell. execd (a
  static binary at the rootfs root) is now exec'd directly.
- **The service data plane silently required the nftables kernel flag.**
  With the default (non-nft) Kata kernel, the nftables netlink calls hang
  rather than error. The nft kernel is now the agent default.
- **VIP routability depended on router-advertisement timing.** VIP-bound
  packets were only routable once the bridge's NAT66 RA installed a default
  route — seconds after boot, too late for the apiserver's first etcd dial.
  execd now installs an explicit on-link route for the service CIDR before
  anything else runs.
- **Conntrack poisoning: NAT is decided on a flow's first packet.** A
  connection opened before the LB rules exist is never translated —
  retransmits bypass the NAT chain and the flow black-holes for its whole
  life. The LB setup is now synchronous, before the workload starts.
  (This was invisible for a year of pods that didn't dial VIPs in their
  first second.)
- **RESOLVED — the "vsock mystery" was IPv6 Duplicate Address Detection.**
  The pod's ULA is *tentative* for its first ~1–2s, and a tentative address
  may not be used as a source — so the kernel picked **::1** for boot-time
  outbound flows (caught by in-guest tcpdump: the DNAT'd SYN to etcd left
  as `::1 → etcd:2379`; the reply went to etcd's own loopback). A TCP
  socket pins its source at connect, so retransmits keep ::1 and the flow
  is dead for its whole life. Scriptable clients recover on their next
  socket; the apiserver's gRPC makes ONE 20s connect attempt at t≈0.5s,
  hits the storage-factory deadline, crash-loops, and lands back inside
  the window — every time. All earlier correlations (memory size,
  hostPorts, vCPUs, the vsock channel itself) were restart-timing
  coincidences; the true variable was "first flow in the first two
  seconds". Fix: the ULA is added with **IFA_F_NODAD** (it is derived from
  the pod UID on a private bridge — duplicate detection buys nothing) and
  the service-CIDR route pins `src` to the pod address. Verified: dial 0
  at t=0.02s with the correct source, and the apiserver assembles over the
  etcd VIP with zero crash-loops — the manifest uses the VIP path again
  (etcd keeps hostPort 2379 as a host-side etcdctl convenience). The 3s
  bound on execd's vsock dials stays as hygiene: a wedged dial must never
  freeze the LB queue behind its mutex. Debugging aid that made this
  findable, kept: `kube-on-macos.io/vmm-log-level: debug` annotation
  (libkrun debug logging per pod), and the in-guest tcpdump-to-virtiofs
  trick (the pcap lands in the pod's host-visible rootfs).
- **RBAC is real now, and it said no.** KCM as a single user may not create
  ReplicaSets; the bootstrap bindings target per-controller ServiceAccounts,
  activated by `--use-service-account-credentials=true` (why kubeadm sets
  it).
- **The node's InternalIP had to stop being 127.0.0.1** — the apiserver
  *pod* dials the kubelet at that address for logs/exec; it is now gvproxy's
  host gateway, and exec/logs work through it.
- **Orphaned mirror pods** (static pod gone, mirror recovered from
  persistent etcd) had no finalizer; the agent now completes their deletion.

## Measured: etcd's disk pattern over virtiofs (2026-07-08)

The plan rests on trusting virtiofs with the cluster's only mutable state,
so this was measured first, inside a microVM against the virtiofs rootfs
(the identical data path a hostPath volume uses). etcd's canonical fio
benchmark — `--rw=write --ioengine=sync --fdatasync=1 --bs=2300 --size=22m`
(WAL-sized writes, sync after each); the etcd docs' bar is **p99 fdatasync
< 10ms**:

```
write: IOPS=10.6k, BW=23.4MiB/s
fdatasync percentiles (usec):
  50th=33  90th=41  99th=84  99.9th=147  99.99th=1352
```

**p99 = 84µs — two orders of magnitude inside the bar.** Control run
without `--fdatasync=1`: 32k IOPS, i.e. each sync costs a real ~35µs
(virtqueue round-trip → host `fsync(2)`), confirming syncs pass through to
the host rather than being absorbed in the guest.

Durability caveat, stated honestly: on macOS, `fsync(2)` flushes to the
drive but *not* the drive cache — full power-loss durability needs
`F_FULLFSYNC`, which is what makes native mac databases slow. So etcd in a
microVM here gets exactly the durability of any mac-native database using
plain fsync: survives guest crash, VMM crash, and host process crash;
a power cut can lose the last moments. For a dev cluster that is the right
trade, and it isn't a virtiofs limitation — it's the host OS contract.

## Open questions

- etcd TLS vs colocation (above) — leaning TLS.
- One CA vs split CAs; front-proxy CA is deferred until aggregated APIs.
- Agent identity: when to drop `system:masters` for
  `system:node:` + Node authorizer + NodeRestriction.
- Kubelet server authn: verify the apiserver's kubelet-client cert (cheap,
  in the inventory) vs delegated TokenReview (real, later).
- Cert rotation/renewal: out of scope — these are dev artifacts;
  regenerate = rerun the tool. Note it as the gap it is.
- ~~Upgrades: a version bump is editing the image tag in a manifest file —
  worth demonstrating explicitly once this lands.~~ **DONE (2026-07-09),
  measured — the pure-declarative upgrade story.** v1.33.0 → v1.33.13, the
  entire operation was editing one image tag in each of three manifest
  files, applied kubeadm-order (apiserver, then KCM + scheduler), each
  edit restarting its static pod on the new image:

  - **64 seconds** from first edit to all three components serving
    v1.33.13, image pulls included (apiserver alone: 35s edit-to-serving).
  - API outage: **33s** (one apiserver pod; an HA setup would roll).
  - Workload pods: zero restarts, established connections unaffected.
  - Honest finding: **16 of 197 guestbook probes failed during the API
    window** — not because workloads depend on the control plane, but
    because the PHP frontend resolves `redis-follower` per request and the
    lazy DNS resolver consults the apiserver on cache miss (5s TTL).
    Hardening noted below. Rollback is the same edit in reverse: the
    desired version lives in a file, nowhere else.
  - The upgraded controllers were exercised immediately (scale up/down:
    ReplicaSet grew, the new scheduler bound the pod, Running in 21s).
- Lazy-resolution hardening: the execd DNS/service resolver should serve
  stale answers when the apiserver is unreachable (CoreDNS effectively
  does this by owning a watch-fed cache). Today an apiserver outage
  degrades *new* name lookups after the 5s TTL; existing flows are
  untouched.
