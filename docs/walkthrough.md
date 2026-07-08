# Walkthrough: the Kubernetes Guestbook tutorial on kube-on-macos

Working through
[Deploying PHP Guestbook application with Redis](https://kubernetes.io/docs/tutorials/stateless-application/guestbook/)
against the PoC (poc/README.md): a real kube-apiserver + etcd (envtest), one
node agent on macOS, every pod a hardware-virtualized Linux microVM (libkrun /
Hypervisor.framework), routed IPv6 pod networking, lazy NFQUEUE-based
ClusterIP services.

The guestbook is a genuinely multi-tier app — a Redis leader, replicated Redis
followers, and a PHP frontend that finds both **by DNS name** — so it
exercises exactly the parts of the PoC that are newest: services, inter-pod
networking, and (it turns out) several things the PoC didn't have yet. This
document is the honest log of what worked, what didn't, and what had to be
built along the way.

## Ground rules

- Use the tutorial's real manifests (fetched from `k8s.io/examples`) wherever
  possible; every divergence is called out in a **Divergence** block with the
  reason.
- When a step fails because the node is missing a capability, build the
  capability (that's the point of the exercise), rerun, and document both the
  failure and the fix.

## Step 0: the cluster

The agent was already running (see poc/README.md for setup), with the
nftables-enabled guest kernel that services need:

```
$ cd poc/agent && go build -o agent . && ./agent \
    --assets ../_artifacts/envtest/k8s/1.33.0-darwin-arm64 \
    --kernel ../_artifacts/vmlinux-nft-arm64
$ export KUBECONFIG=$PWD/poc/_artifacts/kubeconfig
$ kubectl get nodes -o wide
NAME        STATUS   ROLES    AGE    VERSION                  INTERNAL-IP   OS-IMAGE                                    KERNEL-VERSION   CONTAINER-RUNTIME
macos-poc   Ready    <none>   7h5m   kube-on-macos-poc-v0.1   127.0.0.1     Linux microVM (libkrun/HVF) on macOS host   6.12.28-kata     podvm://0.1
```

Tutorial manifests were fetched unmodified into `poc/demo/guestbook/`:

```
for f in redis-leader-deployment redis-leader-service \
         redis-follower-deployment redis-follower-service \
         frontend-deployment frontend-service; do
  curl -fsSLO https://k8s.io/examples/application/guestbook/$f.yaml
done
```

## Pre-flight: known gaps

Reading the manifests against the PoC surfaced four things before running
anything:

1. **All three tutorial images are amd64-only.**
   `registry.k8s.io/redis@sha256:cb111d…` (leader),
   `gb-redis-follower:v2`, and `gb-frontend:v5` publish no linux/arm64
   manifest (checked with `crane config`). Pods here are arm64 microVMs on
   Apple Silicon — there is no emulation layer, so amd64 images cannot run at
   all. This is an ecosystem problem, not a PoC problem (the same tutorial
   fails on any arm64 cluster, e.g. Graviton or Raspberry Pi). Plan: leader →
   `redis:6.0.5` (multi-arch; it's what the GKE variant of this same tutorial
   uses), follower + frontend → rebuilt for arm64 from Google's published
   source (GoogleCloudPlatform/kubernetes-engine-samples, quickstarts/guestbook).
2. **No Deployment controller.** The control plane is envtest (apiserver +
   etcd only); nothing turns a Deployment into pods.
3. **No cluster DNS.** The frontend connects to `redis-leader` /
   `redis-follower` by name; pods' resolv.conf points at gvproxy (external
   DNS only).
4. **Pod `env:` isn't plumbed** into the workload (the manifests set
   `GET_HOSTS_FROM=dns`).

Each of these gets hit — and fixed — in sequence below.

## Step 1: Redis leader — and building a Deployment controller

Applying the tutorial's first manifest, unmodified:

```
$ kubectl apply -f redis-leader-deployment.yaml
$ kubectl get deployment,rs,pods
NAME                           READY   UP-TO-DATE   AVAILABLE
deployment.apps/redis-leader   0/1     0            0
```

No ReplicaSet, no pods, forever: with no controller-manager, a Deployment is
inert apiserver state. **Built:** `poc/agent/deployments.go`, a minimal
stand-in for the deployment + replicaset + garbage controllers in the same
polling style as the agent's scheduler stand-in. It manages pods *directly*
(no ReplicaSet objects, so no rolling updates), claims them via an
`ownerReference`, replaces missing/failed replicas, deletes newest-first on
scale-down, garbage-collects pods whose Deployment is gone, and reports
`status.replicas/readyReplicas/observedGeneration`.

**Divergence:** pods are named `redis-leader-9tb9w` (one random segment, from
`generateName`), not `redis-leader-<rs-hash>-<rand>`; `kubectl get rs` shows
nothing; a template change only affects future pods (no rollout).

### Finding #1: pull-by-digest silently ignores platform

With the controller running, the pod appeared — and image pull surprised us.
Expected: a clean "no linux/arm64 manifest" error. Got:

```
pulling image "registry.k8s.io/redis@sha256:cb111d…": extracting image:
open …/usr/share/man/man2/_exit.2.gz: too many levels of symbolic links
```

Pulling by digest pins a single-arch manifest, so go-containerregistry's
`WithPlatform(linux/arm64)` has nothing to select against — it happily
returned the amd64 image, which then failed mid-extract on a symlink loop.
Even a successful extract would have produced a rootfs of unrunnable amd64
binaries. **Built:** an architecture check after pull; the agent now fails
with `image is linux/amd64; this node runs only linux/arm64 microVMs (no
emulation)`.

### Finding #2: failed pods + a naive controller = pod churn

Worse: the agent reported the pull failure as phase `Failed`, the new
deployment controller replaced the failed pod, and the pair made a doomed pod
every ~15s (5 pods in the first minute). The kubelet's actual contract
matters here: image pull failures keep the pod **Pending** in
`ErrImagePull`/`ImagePullBackOff` with retry backoff — the pod object is not
consumed. **Built:** exactly that (10s→320s doubling backoff, prompt
finalization if the pod is deleted mid-backoff).

### Finding #3: image WorkingDir has to be honored

With the leader image swapped to multi-arch `docker.io/redis:6.0.5` (see the
Divergence comment in the manifest), the pod started — and crash-looped
spewing:

```
chown: changing ownership of './proc/64/ns/uts': Operation not permitted
```

The redis image's entrypoint runs `find . ! -user redis -exec chown redis {}`
before dropping privileges — scoped to its `WORKDIR /data` in Docker. The PoC
didn't honor image WorkingDir, so `.` was `/` and the entrypoint tried to
chown the entire filesystem, /proc included. **Built:** WorkingDir plumbing
(pod spec `workingDir` overriding image config, applied by execd to the
workload and to `kubectl exec` sessions), alongside the env plumbing from
gap #4 (image config env merged under pod spec env — `valueFrom` is still
unimplemented and skipped with a log line).

After that:

```
$ kubectl get pods
NAME                 READY   STATUS    RESTARTS   AGE
redis-leader-ftmwp   1/1     Running   0          15s
$ kubectl logs deployment/redis-leader | tail -1
62:M 07 Jul 2026 17:56:23.575 * Ready to accept connections
```

(Also pleasing: `kubectl logs deployment/…` name resolution is apiserver
magic and just works.) The redis entrypoint's chown + setpriv
privilege-drop behaved on the virtiofs rootfs.

```
$ kubectl apply -f redis-leader-service.yaml
$ kubectl get svc redis-leader
NAME           TYPE        CLUSTER-IP               PORT(S)
redis-leader   ClusterIP   fd42:6b75:6265:1::9f7d   6379/TCP
```

**Divergence:** the Service manifests add `ipFamilies: [IPv6]` — the PoC's
service data plane is IPv6-only, and the apiserver's primary family is kept
IPv4 so envtest's own plumbing stays untouched. The apiserver allocates the
v6 ClusterIP itself; no controller needed.

## Step 2: Redis followers — and building cluster DNS

The follower runs `redis-server --replicaof redis-leader 6379` — the first
DNS-by-service-name consumer. There is no kube-dns/CoreDNS here, and running
one would fight the PoC's grain: it wants *lazy, node-local* resolution, the
same shape as the NFQUEUE service LB.

**Built:** a loopback DNS proxy in execd (`poc/execd/dns.go`, ~190 lines):

- execd binds `127.0.0.1:53` (and `[::1]:53`) inside each pod VM **before
  the workload starts**, and writes the standard kubelet resolv.conf
  (`nameserver 127.0.0.1`, `search default.svc.cluster.local svc.cluster.local
  cluster.local`, `ndots:5`).
- Queries for `<svc>.<ns>.svc.cluster.local` ride the *existing* lazy vsock
  service channel to the agent, which answers straight from apiserver state
  (name → ClusterIPs, TTL 5s, cached in-guest). AAAA gets the v6 ClusterIP;
  other cluster.local forms get NXDOMAIN.
- Everything else is forwarded verbatim to the upstream (gvproxy) resolver,
  so external DNS still works.

No cluster DNS *server*, no watch machinery, no sync: a name is resolved when
(and only when) some pod asks for it, with apiserver state as the single
source of truth. Same trade as the service LB: first-lookup latency (one
vsock round-trip) for zero steady-state machinery.

Then:

```
$ kubectl apply -f redis-follower-deployment.yaml -f redis-follower-service.yaml
$ kubectl get pods
NAME                   READY   STATUS    RESTARTS   AGE
redis-follower-5ktll   1/1     Running   0          24s
redis-follower-wh9h4   1/1     Running   0          24s
redis-leader-ftmwp     1/1     Running   0          87s

$ kubectl logs redis-follower-5ktll | tail -1
66:S … * MASTER <-> REPLICA sync: Finished with success
```

That one log line exercises the whole new stack. Agent-side view of a
follower finding its leader:

```
dns query redis-leader.default -> [fd42:6b75:6265:1::9f7d]
service query [fd42:6b75:6265:1::9f7d]:6379/TCP -> default/redis-leader
    targetPort=6379 endpoints=[fd42:6b75:6265:0:337f:ff93:8dad:49a4]
```

i.e. glibc asks execd's loopback DNS → vsock name query → AAAA ClusterIP →
redis connects to the VIP → first packet pops up via NFQUEUE → endpoints
resolved, backend picked, mark set → in-kernel DNAT for the rest of the
replication stream.

## Step 3: the frontend

Three replicas of the rebuilt `gb-frontend` (PHP 7.4 + Apache + Predis),
with the tutorial's `GET_HOSTS_FROM=dns` env var carried by the new env
plumbing. The PHP app connects to `redis-leader` for writes and
`redis-follower` for reads.

```
$ kubectl apply -f frontend-deployment.yaml -f frontend-service.yaml
$ kubectl get deployment
NAME             READY   UP-TO-DATE   AVAILABLE
frontend         3/3     3            3
redis-follower   2/2     2            2
redis-leader     1/1     1            1
```

Six pods, six microVMs. End-to-end test through all three tiers, from a
client pod via the frontend's ClusterIP:

```
$ kubectl run client --image=alpine:3.22 --restart=Never -- sleep 3600
$ VIP=$(kubectl get svc frontend -o jsonpath='{.spec.clusterIP}')
$ kubectl exec client -- wget -qO- "http://[$VIP]/guestbook.php?cmd=set&value=Hello%20from%20a%20microVM%20guestbook"
{"message": "Updated"}
$ kubectl exec client -- wget -qO- "http://[$VIP]/guestbook.php?cmd=get"
{"data": "Hello from a microVM guestbook"}
```

The `set` traverses client → frontend VIP (LB across 3 PHP pods) → PHP →
`redis-leader` by DNS → leader; the `get` reads back through
`redis-follower` by DNS — so the value came via real Redis replication. The
agent log shows the frontend VIP resolving to all three endpoints:

```
service query [fd42:…:1::e0c0]:80/TCP -> default/frontend targetPort=80
    endpoints=[fd42:…:141e:10ba fd42:…:d7f0:dc2b fd42:…:3e09:f5e9]
```

### Viewing it from the host browser

`kubectl port-forward` isn't implemented, and `type: LoadBalancer` has no
provider. But pod IPs are *real routed IPv6 addresses*, so after giving the
host an address on the pod bridge (one sudo, must be re-run if the bridge is
recreated):

```
$ ./poc/host-net.sh
$ kubectl get pods -l app=guestbook -o wide   # take any pod IP
open http://[fd42:6b75:6265:0:…]/             # the guestbook UI, in Safari
```

(Host → *ClusterIP* doesn't work: the service data plane hooks the OUTPUT
chain inside each pod VM; the host never traverses it. Pod IP only.)

## Step 4: scale

```
$ kubectl scale deployment frontend --replicas=5   # 2 new microVMs, Running in ~20s
$ kubectl scale deployment frontend --replicas=2   # newest pods deleted first
$ kubectl get deployment frontend
NAME       READY   UP-TO-DATE   AVAILABLE
frontend   2/2     2            2
```

`kubectl scale` is apiserver-side (the scale subresource), so it worked
against the PoC controller with zero extra code.

## Step 5: cleanup

The tutorial's exact commands:

```
$ kubectl delete deployment -l app=redis
$ kubectl delete service -l app=redis
$ kubectl delete deployment frontend
$ kubectl delete service -l app=guestbook
$ kubectl get pods
NAME     READY   STATUS    RESTARTS   AGE
client   1/1     Running   0          5m5s
```

With no garbage collector controller, cascade deletion is the agent's job:
the deployment loop deletes owned pods whose Deployment UID no longer exists,
each getting a graceful (SIGTERM-in-guest) VM shutdown. Only the hand-made
`client` pod survived, as it should.

## Scorecard

Tutorial outcome: **works end-to-end**, with divergences that fall into two
buckets:

**Ecosystem, not PoC** — all three tutorial images are amd64-only; any arm64
cluster fails this tutorial today. Leader → multi-arch `redis:6.0.5`;
follower + frontend rebuilt unmodified¹ from Google's published source for
linux/arm64 (pushed to ttl.sh, so the agent's normal registry pull path was
exercised). ¹Two build fixes forced by bitrot: buster's apt repos are
archived (→ `php:7.4-apache`), and `git-all`'s emacs integration fails to
install in containers (→ `git`).

**Note:** ttl.sh images expire after 24h (they stay in the local
`_artifacts/images/` cache once pulled). To rebuild:

```
git clone --depth 1 https://github.com/GoogleCloudPlatform/kubernetes-engine-samples
cd kubernetes-engine-samples/quickstarts/guestbook
sed -i '' 's|php:7.4-apache-buster|php:7.4-apache|; s|git-all|git|' php-redis/Dockerfile
docker build --platform linux/arm64 -t ttl.sh/kube-on-macos-gb-redis-follower:24h redis-follower/
docker build --platform linux/arm64 -t ttl.sh/kube-on-macos-gb-frontend:24h php-redis/
docker push ttl.sh/kube-on-macos-gb-redis-follower:24h
docker push ttl.sh/kube-on-macos-gb-frontend:24h
```

**PoC capabilities built for this walkthrough:**

| Gap hit | What was built |
|---|---|
| Deployments inert (no controller-manager) | Minimal deployment controller in the agent: pods direct from template, ownerRef-claimed, scale up/down, orphan GC, status. No ReplicaSets/rollouts. *Since replaced by the real kube-controller-manager — see epilogue.* |
| amd64 image pulled silently via digest | Post-pull architecture check with an honest error |
| Pull failure churned pods forever | Kubelet-faithful `ErrImagePull` + Pending + backoff instead of `Failed` |
| No cluster DNS | Lazy loopback DNS proxy in execd over the existing vsock service channel; kubelet-standard resolv.conf; upstream forwarding |
| Pod `env:` ignored | Image config env + pod spec env plumbed to workload and exec sessions (`valueFrom` still unimplemented) |
| Image WorkingDir ignored | Plumbed (pod spec overrides image config) — redis's entrypoint chowns `.`, which was `/` without it |

Still missing, discovered or reconfirmed here: `kubectl port-forward`; `type:
LoadBalancer`/NodePort (host can only reach pod IPs, not VIPs); ReplicaSets +
rolling updates (fixed in the epilogue below); `env.valueFrom`;
IPv4/dual-stack services (manifests must request `ipFamilies: [IPv6]`);
headless-service DNS (endpoints as A/AAAA) and SRV/pod records.

## Epilogue: the real kube-controller-manager

The PoC deployment controller did its job — it made the tutorial run and, more
usefully, made the *shape* of the missing control plane concrete (replica
management, orphan GC, the failed-pod-replacement interaction with
ErrImagePull). But reimplementing controllers one at a time is a treadmill:
next would come ReplicaSets, then rollouts, then endpoints… when a perfectly
good implementation exists. So: run the real thing.

envtest doesn't ship kube-controller-manager, but KCM is pure Go and builds
for darwin/arm64 straight from the kubernetes tree, at the version matching
the apiserver:

```
git clone --depth 1 --branch v1.33.0 https://github.com/kubernetes/kubernetes
cd kubernetes && go build -o …/poc/_artifacts/kube-controller-manager ./cmd/kube-controller-manager
```

What it took to wire in (`poc/agent/kcm.go`):

- The agent spawns KCM as a supervised child with `--kubeconfig` (the admin
  kubeconfig it already writes), `--leader-elect=false`, and
  `--service-account-private-key-file` pointed at envtest's own
  `sa-signer.key` — so the token controller signs with the key the apiserver
  already trusts.
- **Node Leases.** KCM's node-lifecycle controller watches
  `kube-node-lease/<node>` renew times; a node agent that only writes status
  would eventually be marked unhealthy and every pod taint-evicted. The agent
  now renews its Lease alongside the 10s status heartbeat — a real piece of
  the kubelet contract that the PoC had skipped and KCM immediately demanded.
- **ServiceAccount admission re-enabled.** It had been disabled precisely
  because nothing created default ServiceAccounts; KCM's SA controller now
  does (all namespaces had one within seconds), so pods pass admission
  un-faked. (Pods now carry a projected token volume the agent ignores —
  volumes are still unimplemented.)
- `poc/agent/deployments.go` **deleted** (−170 lines). The ErrImagePull
  backoff stays — the churn interaction it fixed exists with the real
  ReplicaSet controller too.

What changed observably, rerunning the guestbook:

```
$ kubectl get rs
NAME                       DESIRED   CURRENT   READY
frontend-58c4b4dddd        3         3         3
redis-follower-c8c686f7f   2         2         2
redis-leader-5f4cc9b47d    1         1         1

$ kubectl get pods            # two-segment names, exactly the tutorial's output
frontend-58c4b4dddd-mml7h  …

$ kubectl rollout restart deployment frontend
$ kubectl rollout status deployment frontend
deployment "frontend" successfully rolled out   # 12s: surge microVM up, old drained

$ kubectl get endpointslices  # real IPv6 slices from the endpointslice controller
frontend-p22gr   IPv6   80   fd42:…:b8ac,fd42:…:46c4,fd42:…:710

$ kubectl delete deployment redis-follower      # real GC cascade: deployment → RS → pods
```

Rolling updates, `kubectl rollout undo`, EndpointSlices, namespace deletion,
real cascade GC — all free. The agent is back to being purely a kubelet
stand-in, which is the thesis of the project anyway. (Possible follow-up: the
agent's service resolver still computes endpoints from selectors; it could
consume the now-real EndpointSlices instead. And kube-scheduler could replace
the bind loop the same way.)

