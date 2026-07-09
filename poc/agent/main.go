// PoC node agent for kube-on-macos.
//
// A kubelet stand-in for macOS: the control plane itself (etcd,
// kube-apiserver, kube-controller-manager, kube-scheduler — official
// images) runs as static pods from etc/kubernetes/manifests, each in its
// own Linux microVM; the agent boots those before any apiserver exists,
// then joins the cluster it just started as a node (agent.kubeconfig, via
// the apiserver pod's 127.0.0.1:6443 hostPort forward) and reconciles
// scheduled pods into further microVMs (libkrun on Hypervisor.framework).
// Pod phase/status is reported back honestly.
//
// Implemented so far: real image pulls (flattened, cached), kubectl
// logs/exec/attach via the :10250 kubelet server, startup/readiness/liveness
// probes (exec + in-guest http/tcp via execd), graceful termination
// (SIGTERM in the guest, SIGKILL after grace), restartPolicy with naive
// crash backoff, hostPath/emptyDir/configMap/secret volumes (per-volume
// virtio-fs shares; configMap/secret materialized at pod start), static
// pods from a manifest dir (running before/without the apiserver) with
// mirror pods. Still missing: port-forward, projected volumes,
// authn/authz on the kubelet server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type podVM struct {
	podUID types.UID
	name   string
	ns     string

	// Static pods (file-driven, no API object of their own) get a mirror
	// pod named <name>-<node>; status writes go there when the API is up.
	static     bool
	mirrorName string

	mu       sync.Mutex
	cmd      *exec.Cmd
	ready    map[string]bool // per main-container readiness (probe-driven)
	stopping bool
	restarts int32 // VM-level restarts (in-guest container restarts live in execd)
	ip6      string
	stopOnce sync.Once

	// Digest of the last pushStatus payload (dedupe for the 5s poll).
	lastPush string

	// Last reported status, replayed onto the mirror pod when it is
	// (re)created after the fact.
	lastPhase  corev1.PodPhase
	lastICS    []corev1.ContainerStatus
	lastCS     []corev1.ContainerStatus
	lastReason string
	lastMsg    string
}

func (vm *podVM) setCmd(c *exec.Cmd) { vm.mu.Lock(); vm.cmd = c; vm.mu.Unlock() }
func (vm *podVM) getCmd() *exec.Cmd  { vm.mu.Lock(); defer vm.mu.Unlock(); return vm.cmd }

// initReady seeds per-container readiness: containers without readiness or
// startup probes are ready as soon as they run; probed ones start unready.
func (vm *podVM) initReady(pod *corev1.Pod) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.ready = map[string]bool{}
	for _, c := range pod.Spec.Containers {
		vm.ready[c.Name] = c.ReadinessProbe == nil && c.StartupProbe == nil
	}
}
func (vm *podVM) setContainerReady(name string, r bool) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.ready == nil {
		vm.ready = map[string]bool{}
	}
	changed := vm.ready[name] != r
	vm.ready[name] = r
	return changed
}
func (vm *podVM) containerReady(name string) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.ready[name]
}
func (vm *podVM) setIP(ip string)  { vm.mu.Lock(); vm.ip6 = ip; vm.mu.Unlock() }
func (vm *podVM) getIP() string    { vm.mu.Lock(); defer vm.mu.Unlock(); return vm.ip6 }
func (vm *podVM) setStopping()     { vm.mu.Lock(); vm.stopping = true; vm.mu.Unlock() }
func (vm *podVM) isStopping() bool { vm.mu.Lock(); defer vm.mu.Unlock(); return vm.stopping }
func (vm *podVM) restartCount() int32 {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.restarts
}

// killVM asks execd to SIGTERM the workload, escalating to SIGKILL in the
// guest after the grace period; as a backstop the harness process itself is
// killed a bit after that (e.g. execd unreachable during early boot).
func (a *agent) killVM(vm *podVM, grace int64, reason string) {
	log.Printf("pod %s/%s: stopping VM (%s, grace %ds)", vm.ns, vm.name, reason, grace)
	if err := a.execdRequest(vm.podUID, map[string]any{"op": "shutdown", "grace": grace},
		5*time.Second); err != nil {
		log.Printf("pod %s/%s: graceful shutdown unavailable (%v); killing VM", vm.ns, vm.name, err)
		if cmd := vm.getCmd(); cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
		return
	}
	time.AfterFunc(time.Duration(grace)*time.Second+5*time.Second, func() {
		if cmd := vm.getCmd(); cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %s", path, timeout)
}

func podGracePeriod(pod *corev1.Pod) int64 {
	if pod.DeletionGracePeriodSeconds != nil {
		return *pod.DeletionGracePeriodSeconds
	}
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return *pod.Spec.TerminationGracePeriodSeconds
	}
	return 30
}

type agent struct {
	// client becomes non-nil once the apiserver is up. Static pods run
	// before that (that's their point), so anything reachable from a static
	// pod's lifecycle must handle a nil client.
	client      atomic.Pointer[kubernetes.Clientset]
	nodeName    string
	nodeIP      string
	harness     string
	kernel      string
	rootfsBase  string
	workDir     string
	imagesDir   string
	execdPath   string
	gvproxyPath string
	vmnetHelper string
	podCIDR6    netip.Prefix
	svcCIDR6    string
	kubeletPort int
	kubeletCert string
	kubeletKey  string
	clientCA    string
	imageBlock  bool

	mu         sync.Mutex
	vms        map[types.UID]*podVM
	staticVIPs map[string]*podVM // bootstrap ClusterIPs (static pod annotation) -> backend
}

// cs returns the API client, or nil before the apiserver is up.
func (a *agent) cs() *kubernetes.Clientset { return a.client.Load() }

// staticVIP looks up a bootstrap ClusterIP declared by a static pod.
func (a *agent) staticVIP(vip string) *podVM {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.staticVIPs[vip]
}

func main() {
	var (
		nodeName      = flag.String("node-name", "macos-poc", "node name to register")
		harness       = flag.String("harness", "../harness/podvm", "path to podvm harness binary")
		kernel        = flag.String("kernel", "../_artifacts/vmlinux-nft-arm64", "guest kernel (must include nftables: the services/DNS data plane depends on it)")
		rootfsBase    = flag.String("rootfs-base", "../_artifacts/rootfs-alpine", "base rootfs dir")
		workDir       = flag.String("work-dir", "../_artifacts/pods", "per-pod state dir")
		imagesDir     = flag.String("images-dir", "../_artifacts/images", "pulled-image rootfs cache")
		execdPath     = flag.String("execd", "../_artifacts/execd", "path to the guest execd binary (linux/arm64)")
		gvproxyPath   = flag.String("gvproxy", "../_artifacts/gvproxy", "path to gvproxy (outbound pod networking; empty to disable)")
		vmnetHelper   = flag.String("vmnet-helper", "/opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper", "path to vmnet-helper (routed IPv6 pod networking; empty to disable)")
		podCIDR6      = flag.String("pod-cidr6", "fd42:6b75:6265::/64", "IPv6 ULA /64 for pod addresses")
		svcCIDR6      = flag.String("service-cidr6", "fd42:6b75:6265:1::/112", "IPv6 range for ClusterIPs")
		kubeconfig    = flag.String("kubeconfig", "../etc/kubernetes/agent.kubeconfig", "kubeconfig for the agent (generated by poc/pki; points at the apiserver static pod's 127.0.0.1:6443 hostPort forward)")
		nodeIP        = flag.String("node-ip", "192.168.127.254", "node InternalIP to register: must be reachable from the kube-apiserver pod (gvproxy's host address) for kubectl logs/exec")
		kubeletPort   = flag.Int("kubelet-port", 10250, "port for the kubelet server (kubectl logs)")
		kubeletCert   = flag.String("kubelet-cert", "../etc/kubernetes/pki/kubelet-server.crt", "kubelet server TLS cert (from poc/pki)")
		kubeletKey    = flag.String("kubelet-key", "../etc/kubernetes/pki/kubelet-server.key", "kubelet server TLS key")
		clientCA      = flag.String("client-ca", "../etc/kubernetes/pki/ca.crt", "CA bundle for verifying kubelet API clients (the apiserver)")
		manifestDir   = flag.String("manifest-dir", "../etc/kubernetes/manifests", "static pod manifest dir (watched; pods run without the apiserver — the control plane itself lives here; empty to disable)")
		imageBlock    = flag.Bool("image-block", true, "serve images as read-only ext4 block devices with a tmpfs overlay (real Linux fs semantics); false = legacy flattened-dir virtiofs root")
	)
	flag.Parse()

	for _, p := range []*string{harness, kernel, rootfsBase, workDir, kubeconfig, imagesDir, execdPath, gvproxyPath, kubeletCert, kubeletKey, clientCA} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			log.Fatalf("abs %q: %v", *p, err)
		}
		*p = abs
	}

	a := &agent{
		nodeName:    *nodeName,
		nodeIP:      *nodeIP,
		harness:     *harness,
		kernel:      *kernel,
		rootfsBase:  *rootfsBase,
		workDir:     *workDir,
		imagesDir:   *imagesDir,
		execdPath:   *execdPath,
		gvproxyPath: *gvproxyPath,
		vmnetHelper: *vmnetHelper,
		svcCIDR6:    *svcCIDR6,
		kubeletPort: *kubeletPort,
		kubeletCert: *kubeletCert,
		kubeletKey:  *kubeletKey,
		clientCA:    *clientCA,
		imageBlock:  *imageBlock,
		vms:         map[types.UID]*podVM{},
		staticVIPs:  map[string]*podVM{},
	}
	if err := os.MkdirAll(a.imagesDir, 0o755); err != nil {
		log.Fatalf("creating images dir: %v", err)
	}
	if *vmnetHelper != "" {
		prefix, err := netip.ParsePrefix(*podCIDR6)
		if err != nil || prefix.Bits() != 64 || !prefix.Addr().Is6() {
			log.Fatalf("--pod-cidr6 must be an IPv6 /64, got %q", *podCIDR6)
		}
		a.podCIDR6 = prefix
		if _, err := os.Stat(*vmnetHelper); err != nil {
			log.Fatalf("vmnet-helper not found at %s (brew install nirs/vmnet-helper/vmnet-helper, or --vmnet-helper='' to disable)", *vmnetHelper)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; log.Printf("shutting down"); cancel() }()

	// Static pods start now — before the control plane exists. That ordering
	// is the point: this is the primitive that will eventually boot the
	// control plane itself (research/static-pod-control-plane.md).
	if *manifestDir != "" {
		dir, err := filepath.Abs(*manifestDir)
		if err != nil {
			log.Fatalf("abs %q: %v", *manifestDir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("creating manifest dir: %v", err)
		}
		go a.staticPodLoop(ctx, dir)
	}

	if err := a.startKubeletServer(ctx, *kubeletPort); err != nil {
		log.Fatalf("starting kubelet server: %v", err)
	}

	// The control plane is the static pods above: etcd → apiserver (its
	// 127.0.0.1:6443 hostPort forward is what our kubeconfig points at) →
	// controller-manager and scheduler via the apiserver's bootstrap VIP.
	// Wait for it to self-assemble, then join as a node like any kubelet.
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("loading kubeconfig %s: %v (generate it with: pki --dir ../etc/kubernetes)", *kubeconfig, err)
	}
	// Kubelet-grade traffic: reconcile + per-pod status pushes would swamp
	// client-go's default QPS=5, queueing every request seconds deep — which
	// starves the (latency-sensitive) lazy service-LB resolution path.
	cfg.QPS = 50
	cfg.Burst = 100
	client := kubernetes.NewForConfigOrDie(cfg)
	log.Printf("waiting for the apiserver at %s...", cfg.Host)
	for i := 0; ; i++ {
		if _, err := client.Discovery().ServerVersion(); err == nil {
			break
		} else if i%15 == 14 {
			log.Printf("still waiting for the apiserver: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	a.client.Store(client)
	log.Printf("apiserver is up (%s)", cfg.Host)

	for {
		if err := a.registerNode(ctx); err == nil {
			break
		} else {
			log.Printf("registering node: %v (retrying)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	log.Printf("node %q registered (os=linux arch=arm64, pods run in microVMs)", a.nodeName)
	log.Printf("try: KUBECONFIG=%s kubectl get pods -A", filepath.Join(filepath.Dir(*kubeconfig), "admin.kubeconfig"))

	go a.heartbeatLoop(ctx)
	a.reconcileLoop(ctx)

	// Give VMs a moment to die with the agent.
	a.mu.Lock()
	for _, vm := range a.vms {
		if vm.cmd != nil && vm.cmd.Process != nil {
			vm.cmd.Process.Kill()
		}
	}
	a.mu.Unlock()
}

func (a *agent) registerNode(ctx context.Context) error {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.nodeName,
			Labels: map[string]string{
				"kubernetes.io/os":           "linux",
				"kubernetes.io/arch":         "arm64",
				"kubernetes.io/hostname":     a.nodeName,
				"kube-on-macos.io/runtime":   "vm",
				"kube-on-macos.io/os-host":   "darwin",
			},
		},
	}
	existing, err := a.cs().CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		existing, err = a.cs().CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	}
	if err != nil {
		return err
	}
	existing.Status = a.nodeStatus()
	_, err = a.cs().CoreV1().Nodes().UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (a *agent) nodeStatus() corev1.NodeStatus {
	now := metav1.Now()
	return corev1.NodeStatus{
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			corev1.ResourcePods:   resource.MustParse("32"),
		},
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("12Gi"),
			corev1.ResourcePods:   resource.MustParse("32"),
		},
		Conditions: []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			Reason:             "AgentReady",
			Message:            "kube-on-macos PoC agent is running",
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
		}},
		Addresses: []corev1.NodeAddress{
			// The InternalIP must be reachable from the kube-apiserver *pod*
			// (logs/exec proxying): gvproxy's host address, not 127.0.0.1.
			{Type: corev1.NodeInternalIP, Address: a.nodeIP},
			{Type: corev1.NodeHostName, Address: a.nodeName},
		},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{Port: int32(a.kubeletPort)},
		},
		NodeInfo: corev1.NodeSystemInfo{
			OperatingSystem:         "linux",
			Architecture:            "arm64",
			KubeletVersion:          "kube-on-macos-poc-v0.1",
			OSImage:                 "Linux microVM (libkrun/HVF) on macOS host",
			ContainerRuntimeVersion: "podvm://0.1",
			KernelVersion:           "6.12.28-kata",
		},
	}
}

func (a *agent) heartbeatLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			node, err := a.cs().CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			node.Status = a.nodeStatus()
			a.cs().CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			a.renewNodeLease(ctx)
		}
	}
}

// renewNodeLease keeps the node's kube-node-lease Lease fresh — the health
// signal kube-controller-manager's node-lifecycle controller watches. Without
// it the node would be marked NotReady and every pod taint-evicted.
func (a *agent) renewNodeLease(ctx context.Context) {
	leases := a.cs().CoordinationV1().Leases(corev1.NamespaceNodeLease)
	now := metav1.NewMicroTime(time.Now())
	lease, err := leases.Get(ctx, a.nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		duration := int32(40)
		_, err = leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: a.nodeName, Namespace: corev1.NamespaceNodeLease},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &a.nodeName,
				LeaseDurationSeconds: &duration,
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
	} else if err == nil {
		lease.Spec.RenewTime = &now
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
	}
	if err != nil {
		log.Printf("renewing node lease: %v", err)
	}
}

func (a *agent) reconcileLoop(ctx context.Context) {
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pods, err := a.cs().CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
				FieldSelector: "spec.nodeName=" + a.nodeName,
			})
			if err != nil {
				continue
			}
			for i := range pods.Items {
				a.reconcilePod(ctx, &pods.Items[i])
			}
		}
	}
}

func (a *agent) reconcilePod(ctx context.Context, pod *corev1.Pod) {
	if pod.Annotations[mirrorAnnotation] != "" {
		// Mirror pods reflect static pods; their lifecycle belongs to the
		// static pod manager, and starting a VM for one would double-run it.
		// Exception: an orphaned mirror (its static pod no longer exists —
		// e.g. recovered from persistent etcd after a manifest was removed)
		// has no manager, so finalize deletions here.
		if pod.DeletionTimestamp != nil {
			a.mu.Lock()
			_, live := a.vms[podStateUID(pod)]
			a.mu.Unlock()
			if !live {
				zero := int64(0)
				a.cs().CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name,
					metav1.DeleteOptions{GracePeriodSeconds: &zero})
			}
		}
		return
	}
	a.mu.Lock()
	vm, running := a.vms[pod.UID]
	a.mu.Unlock()

	if pod.DeletionTimestamp != nil {
		if !running {
			// Nothing running (never started, or already exited): finalize.
			zero := int64(0)
			a.cs().CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})
			return
		}
		vm.stopOnce.Do(func() {
			vm.setStopping()
			go a.killVM(vm, podGracePeriod(pod), "pod deleted")
		})
		return
	}

	if running || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return
	}

	// Reserve the slot before the (possibly slow, image-pulling) start so
	// the next reconcile tick doesn't start the pod twice.
	vm = &podVM{podUID: pod.UID, name: pod.Name, ns: pod.Namespace}
	a.mu.Lock()
	a.vms[pod.UID] = vm
	a.mu.Unlock()

	go a.runPod(ctx, pod, vm)
}

// runPod owns a pod's whole lifecycle: prepare rootfs, then a supervise
// loop — start VM, run probes, poll per-container status, wait. Container
// restarts happen *in-guest* (execd applies restartPolicy per container);
// the VM itself restarts only if it dies without writing a final status
// (crash) — the pod-level analogue of a node reboot.
func (a *agent) runPod(ctx context.Context, pod *corev1.Pod, vm *podVM) {
	defer func() {
		a.mu.Lock()
		delete(a.vms, pod.UID)
		a.mu.Unlock()
	}()

	fail := func(reason string, err error) {
		log.Printf("pod %s/%s: %s: %v", pod.Namespace, pod.Name, reason, err)
		a.setPhase(ctx, pod, corev1.PodFailed, reason, err.Error(), nil, nil)
	}

	if !a.imageBlock && (len(pod.Spec.Containers) > 1 || len(pod.Spec.InitContainers) > 0) {
		fail("StartError", fmt.Errorf("multi-container pods require --image-block"))
		return
	}
	vm.initReady(pod)

	// Image pull / volume failures don't fail the pod: like the kubelet,
	// keep it Pending (ErrImagePull / FailedMount) and retry with backoff
	// (a Failed pod would be replaced by its ReplicaSet, churning a doomed
	// pod forever).
	var dir string
	var bases map[string]string
	var volArgs []string
	var mountsByC map[string][]volumeMount
	for attempt := 0; ; attempt++ {
		var err error
		reason := "ErrImagePull"
		dir, bases, err = a.preparePod(pod)
		if err == nil {
			reason = "FailedMount"
			volArgs, mountsByC, err = a.resolveVolumes(ctx, pod, dir)
		}
		if err == nil {
			break
		}
		if len(pod.Spec.Containers) == 0 {
			fail("StartError", err)
			return
		}
		backoff := time.Duration(10<<min(attempt, 5)) * time.Second
		log.Printf("pod %s/%s: %s (retrying in %s): %v", pod.Namespace, pod.Name, reason, backoff, err)
		a.setPhase(ctx, pod, corev1.PodPending, "", "",
			waitingStatuses(pod.Spec.InitContainers, reason, err.Error()),
			waitingStatuses(pod.Spec.Containers, reason, err.Error()))
		deadline := time.Now().Add(backoff)
		for time.Now().Before(deadline) && !vm.isStopping() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if vm.isStopping() {
			a.finalizePod(pod, vm)
			return
		}
	}

	// VM sizing: the pod shares one VM, so sum the main containers' limits
	// (an init container never runs alongside them, so it only raises the
	// floor). Defaults: 256MiB / 1 cpu per container without limits.
	memMB, cpus := int64(0), int64(0)
	for _, c := range pod.Spec.Containers {
		if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			memMB += m.Value() / (1024 * 1024)
		} else {
			memMB += 256
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			cpus += q.Value()
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			if v := m.Value() / (1024 * 1024); v > memMB {
				memMB = v
			}
		}
	}
	if memMB < 256 {
		memMB = 256
	}
	if cpus < 1 {
		cpus = 1
	}

	for attempt := 0; ; attempt++ {
		// Start failures only consume the pod when restartPolicy is Never:
		// kubelet semantics, and load-bearing for static pods — a transient
		// host hiccup (a vmnet-helper EOF, say) must not permanently fail a
		// pod that nothing exists to replace.
		retryStart := func(err error) bool {
			policy := pod.Spec.RestartPolicy
			if policy == "" {
				policy = corev1.RestartPolicyAlways
			}
			if policy == corev1.RestartPolicyNever {
				fail("StartError", err)
				return false
			}
			backoff := time.Duration(1<<min(attempt, 5)) * time.Second
			log.Printf("pod %s/%s: StartError (retrying in %s): %v", pod.Namespace, pod.Name, backoff, err)
			a.setPhase(ctx, pod, corev1.PodPending, "", "",
				waitingStatuses(pod.Spec.InitContainers, "StartError", err.Error()),
				waitingStatuses(pod.Spec.Containers, "StartError", err.Error()))
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
			if vm.isStopping() {
				a.finalizePod(pod, vm)
				return false
			}
			return true
		}

		// Fresh boot filesystem for every VM (re)start. (Volumes live
		// outside the rootfs and persist across restarts.)
		rootfs, images, err := a.makeRootfs(pod, dir, bases, mountsByC)
		if err != nil {
			if retryStart(err) {
				continue
			}
			return
		}
		logf, err := os.OpenFile(filepath.Join(dir, "container.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			if retryStart(err) {
				continue
			}
			return
		}

		// Per-pod gvproxy: userspace NAT giving the guest outbound
		// networking (interim until the routed-IPv6 data plane).
		harnessArgs := []string{
			"--kernel", a.kernel,
			"--rootfs", rootfs,
			"--cpus", fmt.Sprintf("%d", cpus),
			"--mem", fmt.Sprintf("%d", memMB),
			"--log", filepath.Join(dir, "vmm.log"),
			"--vsock-exec", execSockPath(pod.UID),
		}
		harnessArgs = append(harnessArgs, volArgs...)
		// One EROFS block device per distinct image, in the device order the
		// spec's rootDev fields assume (--root-image i => /dev/vd{a+i}).
		for _, img := range images {
			harnessArgs = append(harnessArgs, "--root-image", img+".erofs")
		}
		var gvp *exec.Cmd
		if a.gvproxyPath != "" {
			netSock := netSockPath(pod.UID)
			os.Remove(netSock)
			gvlog, err := os.Create(filepath.Join(dir, "gvproxy.log"))
			if err != nil {
				logf.Close()
				if retryStart(err) {
					continue
				}
				return
			}
			// -ssh-port -1 disables gvproxy's default 127.0.0.1:2222
			// forward, which would collide across per-pod instances.
			gvArgs := []string{"-listen-vfkit", "unixgram://" + netSock, "-ssh-port", "-1"}
			hostPorts := collectHostPorts(pod)
			apiSock := gvproxyAPISockPath(pod.UID)
			if len(hostPorts) > 0 {
				// The control API is only opened when needed (hostPorts).
				os.Remove(apiSock)
				gvArgs = append(gvArgs, "-listen", "unix://"+apiSock)
			}
			gvp = exec.Command(a.gvproxyPath, gvArgs...)
			gvp.Stdout, gvp.Stderr = gvlog, gvlog
			if err := gvp.Start(); err != nil {
				logf.Close()
				gvlog.Close()
				if retryStart(fmt.Errorf("starting gvproxy: %w", err)) {
					continue
				}
				return
			}
			if err := waitForSocket(netSock, 5*time.Second); err != nil {
				gvp.Process.Kill()
				logf.Close()
				if retryStart(err) {
					continue
				}
				return
			}
			if len(hostPorts) > 0 {
				if err := exposeHostPorts(apiSock, hostPorts); err != nil {
					gvp.Process.Kill()
					logf.Close()
					if retryStart(fmt.Errorf("hostPort forward: %w", err)) {
						continue
					}
					return
				}
				log.Printf("pod %s/%s: hostPorts forwarded on 127.0.0.1: %v", pod.Namespace, pod.Name, hostPorts)
			}
			harnessArgs = append(harnessArgs, "--net-socket", netSock)
		}

		// Per-pod vmnet-helper: guest eth1 on the shared vmnet L2, carrying
		// the pod's routed IPv6 address.
		var vmnetCmd *exec.Cmd
		if a.vmnetHelper != "" {
			vmSock := vmnetSockPath(pod.UID)
			os.Remove(vmSock)
			vc, mac, err := startVmnetHelper(a.vmnetHelper, pod.UID, vmSock,
				filepath.Join(dir, "vmnet.log"))
			if err != nil {
				logf.Close()
				if gvp != nil {
					gvp.Process.Kill()
				}
				if retryStart(err) {
					continue
				}
				return
			}
			vmnetCmd = vc
			if err := waitForSocket(vmSock, 5*time.Second); err != nil {
				vmnetCmd.Process.Kill()
				logf.Close()
				if gvp != nil {
					gvp.Process.Kill()
				}
				if retryStart(err) {
					continue
				}
				return
			}
			harnessArgs = append(harnessArgs, "--net2-socket", vmSock, "--net2-mac", mac)
			vm.setIP(podIP6(a.podCIDR6, pod.UID).String())

			// Lazy service resolution channel (guest dials out on cache
			// miss); only meaningful with routed pod networking.
			stopSvc, err := a.startSvcListener(ctx, pod.UID)
			if err != nil {
				vmnetCmd.Process.Kill()
				logf.Close()
				if gvp != nil {
					gvp.Process.Kill()
				}
				if retryStart(err) {
					continue
				}
				return
			}
			defer stopSvc()
			harnessArgs = append(harnessArgs, "--vsock-svc", svcSockPath(pod.UID))
		}
		// Exec execd directly: it's a static binary at the rootfs root, and
		// distroless images (the control plane's) have no /bin/sh to wrap it.
		harnessArgs = append(harnessArgs, "--", "/execd")

		cmd := exec.Command(a.harness, harnessArgs...)
		cmd.Stdout = logf
		cmd.Stderr = logf
		if lvl := pod.Annotations["kube-on-macos.io/vmm-log-level"]; lvl != "" {
			cmd.Env = append(os.Environ(), "PODVM_LOG_LEVEL="+lvl)
		}
		if err := cmd.Start(); err != nil {
			logf.Close()
			if gvp != nil {
				gvp.Process.Kill()
			}
			if vmnetCmd != nil {
				vmnetCmd.Process.Kill()
			}
			if retryStart(err) {
				continue
			}
			return
		}
		vm.setCmd(cmd)
		log.Printf("pod %s/%s: microVM started (pid %d, cpus=%d, mem=%dMiB, restarts=%d)",
			pod.Namespace, pod.Name, cmd.Process.Pid, cpus, memMB, vm.restartCount())

		started := metav1.Now()
		// First status right away (execd reports per-container states), then
		// keep polling; probes push updates on readiness transitions too.
		a.pushStatus(ctx, pod, vm)

		probeStop := make(chan struct{})
		go a.runProbes(ctx, pod, vm, probeStop)
		go a.statusLoop(ctx, pod, vm, probeStop)

		err = cmd.Wait()
		close(probeStop)
		logf.Close()
		if gvp != nil {
			gvp.Process.Kill()
			gvp.Wait()
		}
		if vmnetCmd != nil {
			vmnetCmd.Process.Kill()
			vmnetCmd.Wait()
		}
		log.Printf("pod %s/%s: microVM exited", pod.Namespace, pod.Name)

		// A clean pod end (all containers done per restartPolicy, or a
		// graceful shutdown) leaves execd's final per-container status in the
		// boot share. No file means the VM died mid-flight: the pod-level
		// analogue of a node crash — restart the whole VM.
		if final, ok := readFinalStatus(dir); ok {
			ics, css, _ := a.translateStatuses(pod, final, vm)
			phase := finalPhase(ics, css)
			a.setPhase(context.Background(), pod, phase, "", "", ics, css)
			if vm.isStopping() {
				a.finalizePod(pod, vm)
			}
			return
		}

		if vm.isStopping() {
			a.setPhase(context.Background(), pod, corev1.PodFailed, "VMCrashed",
				"microVM exited without a final status", nil, nil)
			a.finalizePod(pod, vm)
			return
		}
		policy := pod.Spec.RestartPolicy
		if policy == "" {
			policy = corev1.RestartPolicyAlways
		}
		if policy == corev1.RestartPolicyNever {
			fail("VMCrashed", fmt.Errorf("microVM exited without a final status"))
			return
		}

		// Restarting the VM. Naive crash backoff, reset after a long run.
		if time.Since(started.Time) > 5*time.Minute {
			attempt = 0
		}
		backoff := time.Duration(1<<min(attempt, 5)) * time.Second
		vm.mu.Lock()
		vm.restarts++
		vm.mu.Unlock()
		a.setPhase(context.Background(), pod, corev1.PodRunning, "", "",
			waitingStatuses(pod.Spec.InitContainers, "CrashLoopBackOff", fmt.Sprintf("VM restarting in %s", backoff)),
			waitingStatuses(pod.Spec.Containers, "CrashLoopBackOff", fmt.Sprintf("VM restarting in %s", backoff)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if vm.isStopping() {
			a.finalizePod(pod, vm)
			return
		}
	}
}

// finalizePod removes the pod's API presence after its VM is gone: the pod
// object itself for API pods, the mirror pod for static pods (whose source
// of truth is the manifest file, not the API).
func (a *agent) finalizePod(pod *corev1.Pod, vm *podVM) {
	client := a.cs()
	if client == nil {
		return
	}
	name := pod.Name
	if vm.static {
		name = vm.mirrorName
	}
	zero := int64(0)
	client.CoreV1().Pods(pod.Namespace).Delete(context.Background(), name,
		metav1.DeleteOptions{GracePeriodSeconds: &zero})
}

// preparePod does the once-per-pod work: pull every container's image
// (deduped — containers sharing an image share the cached EROFS) and create
// the state dir. Returns (stateDir, image -> imageRootfsBase).
func (a *agent) preparePod(pod *corev1.Pod) (string, map[string]string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", nil, fmt.Errorf("no containers")
	}
	bases := map[string]string{}
	for _, c := range allContainers(pod) {
		if _, ok := bases[c.Image]; ok {
			continue
		}
		base, err := a.ensureImage(c.Image)
		if err != nil {
			return "", nil, fmt.Errorf("pulling image %q: %w", c.Image, err)
		}
		bases[c.Image] = base
	}

	dir := filepath.Join(a.workDir, string(pod.UID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	return dir, bases, nil
}

// allContainers returns init containers followed by main containers — the
// order device assignment and status reporting both use.
func allContainers(pod *corev1.Pod) []corev1.Container {
	return append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
}

// makeRootfs materializes the guest boot filesystem. In block mode this is
// a tiny staging dir — just execd, the spec, and mountpoint dirs — because
// each image arrives as a read-only virtio-blk EROFS that execd overlays
// (tmpfs upper) and chroots its container into. In dir mode it is the
// legacy APFS clone of the flattened image (single-container only).
// Returns the rootfs path and the images (rootfs bases) to attach as block
// devices, in /dev/vdX assignment order.
func (a *agent) makeRootfs(pod *corev1.Pod, dir string, bases map[string]string, mountsByC map[string][]volumeMount) (string, []string, error) {
	rootfs := filepath.Join(dir, "rootfs")

	if _, err := os.Stat(rootfs); err == nil {
		if err := os.RemoveAll(rootfs); err != nil {
			// The guest may have created write-protected dirs; loosen and retry.
			exec.Command("/bin/chmod", "-R", "u+rwX", rootfs).Run()
			if err := os.RemoveAll(rootfs); err != nil {
				return "", nil, fmt.Errorf("removing previous rootfs: %w", err)
			}
		}
	}
	if a.imageBlock {
		// Boot dir: execd + spec + the dirs libkrun's init expects. execd
		// creates its per-container overlay dirs itself.
		for _, d := range []string{"", "dev", "proc", "sys"} {
			if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
				return "", nil, err
			}
		}
	} else {
		// APFS clonefile copy of the base rootfs: instant, copy-on-write.
		// -p matters: without it, directories are recreated subject to the
		// umask, silently stripping e.g. /tmp's 1777 down to 1755.
		if out, err := exec.Command("/bin/cp", "-Rpc", bases[pod.Spec.Containers[0].Image], rootfs).CombinedOutput(); err != nil {
			return "", nil, fmt.Errorf("cloning rootfs: %v: %s", err, out)
		}
	}

	// The guest boots into execd, which supervises every container (on a pty
	// when the pod asks for one) and serves exec/attach over vsock. Workload
	// argv travels in a spec file: libkrun passes exec args via the kernel
	// cmdline, which splits on whitespace, so it must go out-of-band.
	if out, err := exec.Command("/bin/cp", "-c", a.execdPath, filepath.Join(rootfs, "execd")).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("installing execd: %v: %s", err, out)
	}

	// Device assignment: distinct images in first-use order (inits, then
	// mains) map to /dev/vda, vdb, ... — matching the harness's --root-image
	// order.
	var orderedImages []string
	devByBase := map[string]string{}
	containerSpec := func(c corev1.Container) (map[string]any, error) {
		base := bases[c.Image]
		argv := append(append([]string{}, c.Command...), c.Args...)
		if len(argv) == 0 {
			argv = imageArgv(base)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("container %s: no command: pod spec has none and image config has no Entrypoint/Cmd", c.Name)
		}
		cs := map[string]any{"name": c.Name, "argv": argv, "tty": c.TTY}
		if a.imageBlock {
			dev, ok := devByBase[base]
			if !ok {
				dev = fmt.Sprintf("/dev/vd%c", 'a'+len(orderedImages))
				devByBase[base] = dev
				orderedImages = append(orderedImages, base)
			}
			cs["rootDev"] = dev
		}
		// Environment: image config env, overridden by pod spec env. valueFrom
		// (fieldRef/configMap/secret) is not implemented.
		env := imageEnv(base)
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				log.Printf("pod %s/%s: env %s uses valueFrom (not implemented); skipping", pod.Namespace, pod.Name, e.Name)
				continue
			}
			env = append(env, e.Name+"="+e.Value)
		}
		if len(env) > 0 {
			cs["env"] = env
		}
		// Working directory: pod spec overrides image config. Without this,
		// entrypoints that operate on "." (e.g. redis's chown of its WORKDIR
		// /data) run against / instead.
		cwd := c.WorkingDir
		if cwd == "" {
			cwd = imageWorkingDir(base)
		}
		if cwd != "" {
			cs["cwd"] = cwd
		}
		if len(mountsByC[c.Name]) > 0 {
			cs["mounts"] = mountsByC[c.Name]
		}
		return cs, nil
	}

	policy := pod.Spec.RestartPolicy
	if policy == "" {
		policy = corev1.RestartPolicyAlways
	}
	var inits, mains []map[string]any
	for _, c := range pod.Spec.InitContainers {
		cs, err := containerSpec(c)
		if err != nil {
			return "", nil, err
		}
		inits = append(inits, cs)
	}
	for _, c := range pod.Spec.Containers {
		cs, err := containerSpec(c)
		if err != nil {
			return "", nil, err
		}
		mains = append(mains, cs)
	}
	specData := map[string]any{
		"restartPolicy": string(policy),
		"containers":    mains,
	}
	if len(inits) > 0 {
		specData["initContainers"] = inits
	}
	if a.vmnetHelper != "" {
		specData["net6"] = map[string]string{
			"ip": podIP6(a.podCIDR6, pod.UID).String() + "/64",
		}
		specData["svc"] = map[string]string{"cidr": a.svcCIDR6, "ns": pod.Namespace}
	}
	if a.gvproxyPath != "" {
		// gvproxy's virtual network: 192.168.127.0/24, gateway+DNS at .1.
		// Per-pod gvproxy instance, so the guest address can be fixed.
		specData["net"] = map[string]string{
			"ip":  "192.168.127.2/24",
			"gw":  "192.168.127.1",
			"dns": "192.168.127.1",
		}
	}
	specJSON, err := json.Marshal(specData)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(rootfs, ".podvm-spec.json"), specJSON, 0o644); err != nil {
		return "", nil, err
	}
	return rootfs, orderedImages, nil
}

// volumeMount is one entry of the guest-side mount plan (spec JSON).
type volumeMount struct {
	Tag       string `json:"tag"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// resolveVolumes turns the pod's volumes + every container's volumeMounts
// into harness --volume args (one virtio-fs device per volume, shared by all
// containers that mount it — this is how a shared emptyDir works across a
// pod) and per-container guest-side mount plans. Supported: hostPath
// (Directory / DirectoryOrCreate / "") and emptyDir (a host dir under the
// pod state dir, so it survives container restarts and dies with the pod —
// kubelet semantics). Projected volumes (the auto-injected ServiceAccount
// token) are skipped quietly; anything else is an error, matching a kubelet
// that can't set up a volume.
func (a *agent) resolveVolumes(ctx context.Context, pod *corev1.Pod, stateDir string) (harnessArgs []string, mountsByC map[string][]volumeMount, err error) {
	static := pod.Annotations[sourceAnnotation] == "file"
	byName := map[string]*corev1.Volume{}
	for i := range pod.Spec.Volumes {
		byName[pod.Spec.Volumes[i].Name] = &pod.Spec.Volumes[i]
	}
	containers := allContainers(pod)
	// A volume's virtio-fs device is read-only only if every mount of it, in
	// every container, is. configMap/secret volumes are always read-only
	// (kubelet semantics).
	anyRW := map[string]bool{}
	forceRO := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.VolumeMounts {
			if !m.ReadOnly {
				anyRW[m.Name] = true
			}
		}
	}

	// materialize resolves a volume to its host dir, once per volume.
	hostDirs := map[string]string{}
	materialize := func(vol *corev1.Volume) (string, error) {
		if d, ok := hostDirs[vol.Name]; ok {
			return d, nil
		}
		var hostDir string
		switch {
		case vol.HostPath != nil:
			hostDir = vol.HostPath.Path
			t := corev1.HostPathUnset
			if vol.HostPath.Type != nil {
				t = *vol.HostPath.Type
			}
			switch t {
			case corev1.HostPathUnset, corev1.HostPathDirectory:
				if st, err := os.Stat(hostDir); err != nil || !st.IsDir() {
					return "", fmt.Errorf("hostPath %q: not an existing directory", hostDir)
				}
			case corev1.HostPathDirectoryOrCreate:
				if err := os.MkdirAll(hostDir, 0o755); err != nil {
					return "", fmt.Errorf("hostPath %q: %w", hostDir, err)
				}
			default:
				return "", fmt.Errorf("hostPath type %q not supported (Directory/DirectoryOrCreate only)", t)
			}
		case vol.EmptyDir != nil:
			hostDir = filepath.Join(stateDir, "volumes", vol.Name)
			if err := os.MkdirAll(hostDir, 0o755); err != nil {
				return "", fmt.Errorf("emptyDir %q: %w", vol.Name, err)
			}
		case vol.ConfigMap != nil:
			if static {
				return "", fmt.Errorf("volume %q: static pods cannot reference API objects (configMap)", vol.Name)
			}
			hostDir = filepath.Join(stateDir, "volumes", vol.Name)
			if err := a.materializeConfigMap(ctx, pod.Namespace, vol.ConfigMap, hostDir); err != nil {
				return "", err
			}
			forceRO[vol.Name] = true
		case vol.Secret != nil:
			if static {
				return "", fmt.Errorf("volume %q: static pods cannot reference API objects (secret)", vol.Name)
			}
			hostDir = filepath.Join(stateDir, "volumes", vol.Name)
			if err := a.materializeSecret(ctx, pod.Namespace, vol.Secret, hostDir); err != nil {
				return "", err
			}
			forceRO[vol.Name] = true
		default:
			return "", fmt.Errorf("volume %q: unsupported volume type (hostPath, emptyDir, configMap, secret)", vol.Name)
		}
		hostDirs[vol.Name] = hostDir
		return hostDir, nil
	}

	mountsByC = map[string][]volumeMount{}
	tags := map[string]string{} // volume name -> tag
	for _, c := range containers {
		for _, m := range c.VolumeMounts {
			vol := byName[m.Name]
			if vol == nil {
				return nil, nil, fmt.Errorf("volumeMount %q: no such volume", m.Name)
			}
			if m.SubPath != "" || m.SubPathExpr != "" {
				return nil, nil, fmt.Errorf("volumeMount %q: subPath not supported", m.Name)
			}
			if vol.Projected != nil {
				// ServiceAccount token projection, injected by admission into
				// every pod. Not implemented; skipping it keeps all pods usable.
				continue
			}
			hostDir, err := materialize(vol)
			if err != nil {
				return nil, nil, err
			}
			tag, ok := tags[vol.Name]
			if !ok {
				tag = fmt.Sprintf("vol%d", len(tags))
				tags[vol.Name] = tag
				arg := tag + "=" + hostDir
				if !anyRW[vol.Name] || forceRO[vol.Name] {
					arg += ":ro" // VMM-side enforcement; guest adds MS_RDONLY per mount
				}
				harnessArgs = append(harnessArgs, "--volume", arg)
			}
			mountsByC[c.Name] = append(mountsByC[c.Name],
				volumeMount{Tag: tag, MountPath: m.MountPath, ReadOnly: m.ReadOnly || forceRO[vol.Name]})
		}
	}
	return harnessArgs, mountsByC, nil
}

func (a *agent) setPhase(ctx context.Context, pod *corev1.Pod, phase corev1.PodPhase, reason, message string, ics, css []corev1.ContainerStatus) {
	a.mu.Lock()
	vm := a.vms[pod.UID]
	a.mu.Unlock()

	name := pod.Name
	if vm != nil && vm.static {
		// Static pods have no API object of their own; status goes to the
		// mirror pod. Record it on the VM too, so a mirror created later
		// (or recreated after deletion) can be backfilled.
		name = vm.mirrorName
		vm.mu.Lock()
		vm.lastPhase, vm.lastReason, vm.lastMsg = phase, reason, message
		vm.lastICS, vm.lastCS = ics, css
		vm.mu.Unlock()
	}
	client := a.cs()
	if client == nil {
		return // pre-apiserver (static pods only); mirror sync backfills later
	}
	latest, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	now := metav1.Now()
	latest.Status.Phase = phase
	latest.Status.Reason = reason
	latest.Status.Message = message
	if latest.Status.StartTime == nil {
		latest.Status.StartTime = &now
	}
	// PodReady = running with every (main) container ready.
	ready := corev1.ConditionFalse
	if phase == corev1.PodRunning && len(css) > 0 {
		ready = corev1.ConditionTrue
		for _, cs := range css {
			if !cs.Ready {
				ready = corev1.ConditionFalse
			}
		}
	}
	if vm != nil && vm.getIP() != "" {
		latest.Status.PodIP = vm.getIP()
		latest.Status.PodIPs = []corev1.PodIP{{IP: vm.getIP()}}
	}
	latest.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: ready, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now},
	}
	if css != nil {
		latest.Status.ContainerStatuses = css
	}
	if ics != nil {
		latest.Status.InitContainerStatuses = ics
	}
	if _, err := a.cs().CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{}); err != nil {
		log.Printf("updating status for %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}
