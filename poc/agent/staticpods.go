package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// Static pods: pod manifests as files in a watched directory, run with no
// apiserver involved — the bootstrap primitive that will eventually run the
// control plane itself (research/static-pod-control-plane.md). Once an
// apiserver is reachable, each static pod is reflected as a read-only
// "mirror pod" named <name>-<node> so kubectl can see it; deleting the
// mirror never touches the static pod (the file is the source of truth),
// the agent just recreates the mirror.
//
// The kubelet's annotations are reused so standard tooling understands us:
// config.source=file marks the pod file-driven, config.mirror/config.hash
// carry the manifest hash (also the static pod's internal UID, which is how
// the kubelet server maps a mirror pod back to its VM for logs/exec).

const (
	mirrorAnnotation = "kubernetes.io/config.mirror"
	hashAnnotation   = "kubernetes.io/config.hash"
	sourceAnnotation = "kubernetes.io/config.source"

	// clusterIPAnnotation declares a bootstrap ClusterIP for a static pod:
	// a stable VIP (inside the service CIDR, so the in-guest NFQUEUE data
	// plane intercepts it) that the agent resolves to this pod from its own
	// table — before, and independent of, any apiserver. This is the stable
	// address that goes in control-plane kubeconfigs and cert SANs; the
	// pod's real IP appears nowhere and may change freely across manifest
	// edits. Once an apiserver is up, the VIP is claimed as a real Service
	// (see syncMirrorService) so it is kubectl-visible and can't be
	// allocated to anything else.
	clusterIPAnnotation = "kube-on-macos.io/cluster-ip"
)

type staticPod struct {
	file string
	hash string // sha256 of the manifest file = the pod's internal UID
	vip  string // bootstrap ClusterIP, "" if none
	pod  *corev1.Pod
	vm   *podVM
}

// staticPodLoop owns the manifest directory: files appearing start pods,
// content changes restart them (a changed manifest is a different pod, per
// kubelet semantics), removals stop them. It also keeps mirror pods in sync
// whenever the API is up.
func (a *agent) staticPodLoop(ctx context.Context, dir string) {
	log.Printf("static pods: watching %s", dir)
	running := map[string]*staticPod{} // manifest basename -> pod
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		a.scanManifests(ctx, dir, running)
		if a.cs() != nil {
			a.syncMirrorPods(ctx, running)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *agent) scanManifests(ctx context.Context, dir string, running map[string]*staticPod) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("static pods: reading %s: %v", dir, err)
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") ||
			(!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".json")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		seen[name] = true

		cur := running[name]
		if cur != nil && cur.hash == hash {
			continue // unchanged
		}
		if cur != nil {
			log.Printf("static pod %s: manifest changed, restarting", name)
			a.stopStaticPod(cur)
			delete(running, name)
		}
		pod, err := parseStaticPod(data)
		if err != nil {
			log.Printf("static pod %s: %v (ignoring file)", name, err)
			continue
		}
		// UUID-shaped UID (vmnet-helper requires a UUID interface id),
		// deterministic from the manifest content: a changed manifest is a
		// different pod.
		pod.UID = uidFromHash(sum)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[hashAnnotation] = string(pod.UID)
		pod.Annotations[sourceAnnotation] = "file"
		pod.Spec.NodeName = a.nodeName

		vm := &podVM{
			podUID:     pod.UID,
			name:       pod.Name,
			ns:         pod.Namespace,
			static:     true,
			mirrorName: pod.Name + "-" + a.nodeName,
		}
		vip := pod.Annotations[clusterIPAnnotation]
		if vip != "" {
			if err := a.validateBootstrapVIP(vip); err != nil {
				log.Printf("static pod %s: ignoring %s: %v", name, clusterIPAnnotation, err)
				vip = ""
			}
		}
		a.mu.Lock()
		a.vms[pod.UID] = vm
		if vip != "" {
			if other, taken := a.staticVIPs[vip]; taken && other.podUID != pod.UID {
				log.Printf("static pod %s: VIP %s already claimed by %s/%s; ignoring", name, vip, other.ns, other.name)
				vip = ""
			} else {
				a.staticVIPs[vip] = vm
			}
		}
		a.mu.Unlock()
		if vip != "" {
			log.Printf("static pod %s: bootstrap VIP %s -> %s/%s", name, vip, pod.Namespace, pod.Name)
		}
		running[name] = &staticPod{file: name, hash: hash, vip: vip, pod: pod, vm: vm}
		log.Printf("static pod %s: starting %s/%s (uid %s)", name, pod.Namespace, pod.Name, pod.UID)
		go a.runPod(ctx, pod, vm)
	}
	for name, sp := range running {
		if !seen[name] {
			log.Printf("static pod %s: manifest removed, stopping", name)
			a.stopStaticPod(sp)
			delete(running, name)
		}
	}
}

func (a *agent) stopStaticPod(sp *staticPod) {
	if sp.vip != "" {
		a.mu.Lock()
		if a.staticVIPs[sp.vip] == sp.vm {
			delete(a.staticVIPs, sp.vip)
		}
		a.mu.Unlock()
		// The Service claim dies with the manifest (best-effort; on a
		// manifest *change* the new pod immediately re-claims it).
		if client := a.cs(); client != nil {
			client.CoreV1().Services(sp.pod.Namespace).Delete(context.Background(),
				sp.pod.Name, metav1.DeleteOptions{})
		}
	}
	sp.vm.stopOnce.Do(func() {
		sp.vm.setStopping()
		go a.killVM(sp.vm, podGracePeriod(sp.pod), "static pod manifest removed/changed")
	})
	// runPod's exit path calls finalizePod, which deletes the mirror. If the
	// VM already exited on its own (workload completed), delete it here.
	a.mu.Lock()
	_, alive := a.vms[sp.vm.podUID]
	a.mu.Unlock()
	if !alive {
		a.finalizePod(sp.pod, sp.vm)
	}
}

// validateBootstrapVIP: the VIP must be an IPv6 address inside the service
// CIDR — that's what the guests' NFQUEUE rule intercepts.
func (a *agent) validateBootstrapVIP(vip string) error {
	addr, err := netip.ParseAddr(vip)
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(a.svcCIDR6)
	if err != nil {
		return fmt.Errorf("bad service CIDR %q: %w", a.svcCIDR6, err)
	}
	if !addr.Is6() || !prefix.Contains(addr) {
		return fmt.Errorf("%s is not inside the IPv6 service CIDR %s", vip, a.svcCIDR6)
	}
	return nil
}

// syncMirrorPods makes the API reflect the static pods: create missing
// mirrors, replace stale ones, backfill status, and finish deletions the
// user starts with kubectl (which must not touch the static pod itself).
func (a *agent) syncMirrorPods(ctx context.Context, running map[string]*staticPod) {
	client := a.cs()
	for _, sp := range running {
		if sp.vip != "" {
			a.syncMirrorService(ctx, sp)
		}
		mirror, err := client.CoreV1().Pods(sp.pod.Namespace).Get(ctx, sp.vm.mirrorName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			a.createMirrorPod(ctx, sp)
		case err != nil:
			continue
		case mirror.DeletionTimestamp != nil:
			// kubectl delete on a mirror pod: finish it (grace 0 — no VM to
			// stop, the static pod stays up) and recreate next tick.
			zero := int64(0)
			client.CoreV1().Pods(mirror.Namespace).Delete(ctx, mirror.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})
		case mirror.Annotations[hashAnnotation] != string(sp.pod.UID):
			client.CoreV1().Pods(mirror.Namespace).Delete(ctx, mirror.Name, metav1.DeleteOptions{})
		}
	}
}

func (a *agent) createMirrorPod(ctx context.Context, sp *staticPod) {
	client := a.cs()
	mirror := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sp.vm.mirrorName,
			Namespace: sp.pod.Namespace,
			Labels:    sp.pod.Labels,
			Annotations: map[string]string{
				mirrorAnnotation: string(sp.pod.UID),
				hashAnnotation:   string(sp.pod.UID),
				sourceAnnotation: "file",
			},
		},
		Spec: *sp.pod.Spec.DeepCopy(),
	}
	mirror.Spec.NodeName = a.nodeName
	if _, err := client.CoreV1().Pods(mirror.Namespace).Create(ctx, mirror, metav1.CreateOptions{}); err != nil {
		log.Printf("static pod %s: creating mirror %s: %v", sp.file, mirror.Name, err)
		return
	}
	log.Printf("static pod %s: mirror pod %s/%s created", sp.file, mirror.Namespace, mirror.Name)
	// Backfill the status reported before the mirror existed (e.g. the pod
	// went Running while the apiserver was still booting).
	sp.vm.mu.Lock()
	phase, reason, msg, cs := sp.vm.lastPhase, sp.vm.lastReason, sp.vm.lastMsg, sp.vm.lastCS
	sp.vm.mu.Unlock()
	if phase != "" {
		a.setPhase(ctx, sp.pod, phase, reason, msg, cs)
	}
}

// syncMirrorService claims a static pod's bootstrap VIP as a real Service
// once an apiserver exists: kubectl-visible, and the ClusterIP allocator
// can't hand the address to anything else. Resolution still happens from
// the agent's own table (checked first), so the Service is a reflection,
// not a dependency — same philosophy as mirror pods.
func (a *agent) syncMirrorService(ctx context.Context, sp *staticPod) {
	client := a.cs()
	_, err := client.CoreV1().Services(sp.pod.Namespace).Get(ctx, sp.pod.Name, metav1.GetOptions{})
	if err == nil || !apierrors.IsNotFound(err) {
		return
	}
	var ports []corev1.ServicePort
	for _, p := range sp.pod.Spec.Containers[0].Ports {
		if p.ContainerPort != 0 {
			ports = append(ports, corev1.ServicePort{
				Name: p.Name, Port: p.ContainerPort, Protocol: corev1.ProtocolTCP,
			})
		}
	}
	if len(ports) == 0 {
		log.Printf("static pod %s: VIP %s: no containerPorts declared; not creating mirror service", sp.file, sp.vip)
		return
	}
	ipv6 := corev1.IPv6Protocol
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sp.pod.Name,
			Namespace: sp.pod.Namespace,
			Labels:    sp.pod.Labels,
			Annotations: map[string]string{
				sourceAnnotation: "file",
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:  sp.vip,
			ClusterIPs: []string{sp.vip},
			IPFamilies: []corev1.IPFamily{ipv6},
			Selector:   sp.pod.Labels,
			Ports:      ports,
		},
	}
	if _, err := client.CoreV1().Services(svc.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		log.Printf("static pod %s: claiming VIP %s as service %s/%s: %v", sp.file, sp.vip, svc.Namespace, svc.Name, err)
		return
	}
	log.Printf("static pod %s: VIP %s claimed as service %s/%s", sp.file, sp.vip, svc.Namespace, svc.Name)
}

func parseStaticPod(data []byte) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := yaml.UnmarshalStrict(data, pod); err != nil {
		return nil, err
	}
	if pod.Kind != "Pod" {
		return nil, errNotAPod(pod.Kind)
	}
	if pod.Name == "" || len(pod.Spec.Containers) == 0 {
		return nil, errNotAPod("incomplete")
	}
	if pod.Namespace == "" {
		pod.Namespace = "default"
	}
	return pod, nil
}

type errNotAPod string

func (e errNotAPod) Error() string { return "not a pod manifest: " + string(e) }

// uidFromHash formats the manifest hash as a UUID: deterministic, and
// UUID-shaped because downstream consumers (vmnet-helper's interface id)
// require one.
func uidFromHash(sum [32]byte) types.UID {
	h := hex.EncodeToString(sum[:16])
	return types.UID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32])
}

// podStateUID maps an API pod to the UID that keys its VM state (state dir,
// vsock sockets): a mirror pod carries its static pod's hash-UID in the
// config.hash annotation; every other pod is keyed by its own UID.
func podStateUID(pod *corev1.Pod) types.UID {
	if pod.Annotations[mirrorAnnotation] != "" {
		if h := pod.Annotations[hashAnnotation]; h != "" {
			return types.UID(h)
		}
	}
	return pod.UID
}
