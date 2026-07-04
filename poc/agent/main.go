// PoC node agent for kube-on-macos.
//
// This is milestone-0 of "reimplement the kubelet API contract for macOS":
// it boots a real kube-apiserver+etcd locally (via envtest binaries),
// registers a Node, binds pending pods to it, and reconciles each bound pod
// into a Linux microVM launched through the podvm harness (libkrun on
// Hypervisor.framework). Pod phase/status is reported back honestly.
//
// Implemented so far: real image pulls (flattened, cached), kubectl
// logs/exec/attach via the :10250 kubelet server, startup/readiness/liveness
// probes (exec + in-guest http/tcp via execd), graceful termination
// (SIGTERM in the guest, SIGKILL after grace), restartPolicy with naive
// crash backoff. Still missing: port-forward, volumes beyond the rootfs,
// pod networking, authn/authz on the kubelet server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type podVM struct {
	podUID types.UID
	name   string
	ns     string

	mu       sync.Mutex
	cmd      *exec.Cmd
	ready    bool
	stopping bool
	restarts int32
	stopOnce sync.Once
}

func (vm *podVM) setCmd(c *exec.Cmd) { vm.mu.Lock(); vm.cmd = c; vm.mu.Unlock() }
func (vm *podVM) getCmd() *exec.Cmd  { vm.mu.Lock(); defer vm.mu.Unlock(); return vm.cmd }
func (vm *podVM) setReady(r bool) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	changed := vm.ready != r
	vm.ready = r
	return changed
}
func (vm *podVM) getReady() bool   { vm.mu.Lock(); defer vm.mu.Unlock(); return vm.ready }
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
	client      *kubernetes.Clientset
	nodeName    string
	harness     string
	kernel      string
	rootfsBase  string
	workDir     string
	imagesDir   string
	execdPath   string
	kubeletPort int

	mu  sync.Mutex
	vms map[types.UID]*podVM
}

func main() {
	var (
		nodeName      = flag.String("node-name", "macos-poc", "node name to register")
		harness       = flag.String("harness", "../harness/podvm", "path to podvm harness binary")
		kernel        = flag.String("kernel", "../_artifacts/vmlinux-arm64", "guest kernel")
		rootfsBase    = flag.String("rootfs-base", "../_artifacts/rootfs-alpine", "base rootfs dir")
		workDir       = flag.String("work-dir", "../_artifacts/pods", "per-pod state dir")
		imagesDir     = flag.String("images-dir", "../_artifacts/images", "pulled-image rootfs cache")
		execdPath     = flag.String("execd", "../_artifacts/execd", "path to the guest execd binary (linux/arm64)")
		assetsDir     = flag.String("assets", "", "envtest binary assets dir (kube-apiserver, etcd)")
		kubeconfigOut = flag.String("kubeconfig-out", "../_artifacts/kubeconfig", "where to write admin kubeconfig")
		kubeletPort   = flag.Int("kubelet-port", 10250, "port for the kubelet server (kubectl logs)")
	)
	flag.Parse()

	for _, p := range []*string{harness, kernel, rootfsBase, workDir, kubeconfigOut, imagesDir, execdPath} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			log.Fatalf("abs %q: %v", *p, err)
		}
		*p = abs
	}

	env := &envtest.Environment{}
	if *assetsDir != "" {
		env.BinaryAssetsDirectory = *assetsDir
	}
	// No controller-manager runs here, so the ServiceAccount admission
	// plugin would reject pods (no auto-created default SA). Disable it.
	env.ControlPlane.GetAPIServer().Configure().
		Append("disable-admission-plugins", "ServiceAccount").
		// The apiserver prefers the node's Hostname address for kubelet
		// connections (logs/exec); "macos-poc" doesn't resolve, so use the
		// InternalIP we register (127.0.0.1).
		Append("kubelet-preferred-address-types", "InternalIP")

	log.Printf("starting kube-apiserver + etcd (envtest)...")
	cfg, err := env.Start()
	if err != nil {
		log.Fatalf("starting control plane: %v", err)
	}
	defer env.Stop()

	user, err := env.AddUser(envtest.User{Name: "poc-admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		log.Fatalf("adding admin user: %v", err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		log.Fatalf("rendering kubeconfig: %v", err)
	}
	if err := os.WriteFile(*kubeconfigOut, kc, 0o600); err != nil {
		log.Fatalf("writing kubeconfig: %v", err)
	}
	log.Printf("kubeconfig written to %s", *kubeconfigOut)
	log.Printf("try: KUBECONFIG=%s kubectl get nodes", *kubeconfigOut)

	a := &agent{
		client:      kubernetes.NewForConfigOrDie(cfg),
		nodeName:    *nodeName,
		harness:     *harness,
		kernel:      *kernel,
		rootfsBase:  *rootfsBase,
		workDir:     *workDir,
		imagesDir:   *imagesDir,
		execdPath:   *execdPath,
		kubeletPort: *kubeletPort,
		vms:         map[types.UID]*podVM{},
	}
	if err := os.MkdirAll(a.imagesDir, 0o755); err != nil {
		log.Fatalf("creating images dir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; log.Printf("shutting down"); cancel() }()

	if err := a.startKubeletServer(ctx, *kubeletPort); err != nil {
		log.Fatalf("starting kubelet server: %v", err)
	}

	if err := a.registerNode(ctx); err != nil {
		log.Fatalf("registering node: %v", err)
	}
	log.Printf("node %q registered (os=linux arch=arm64, pods run in microVMs)", a.nodeName)

	go a.heartbeatLoop(ctx)
	go a.schedulerLoop(ctx)
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
				"node.kubernetes.io/os-host": "darwin",
			},
		},
	}
	existing, err := a.client.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		existing, err = a.client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	}
	if err != nil {
		return err
	}
	existing.Status = a.nodeStatus()
	_, err = a.client.CoreV1().Nodes().UpdateStatus(ctx, existing, metav1.UpdateOptions{})
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
			{Type: corev1.NodeInternalIP, Address: "127.0.0.1"},
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
			node, err := a.client.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			node.Status = a.nodeStatus()
			a.client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		}
	}
}

// schedulerLoop is a stand-in for kube-scheduler: bind any unassigned pod to
// this node so plain `kubectl apply` works.
func (a *agent) schedulerLoop(ctx context.Context) {
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pods, err := a.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
				FieldSelector: "spec.nodeName=",
			})
			if err != nil {
				continue
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				b := &corev1.Binding{
					ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
					Target:     corev1.ObjectReference{Kind: "Node", Name: a.nodeName},
				}
				if err := a.client.CoreV1().Pods(pod.Namespace).Bind(ctx, b, metav1.CreateOptions{}); err == nil {
					log.Printf("bound pod %s/%s to %s", pod.Namespace, pod.Name, a.nodeName)
				}
			}
		}
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
			pods, err := a.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
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
	a.mu.Lock()
	vm, running := a.vms[pod.UID]
	a.mu.Unlock()

	if pod.DeletionTimestamp != nil {
		if !running {
			// Nothing running (never started, or already exited): finalize.
			zero := int64(0)
			a.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name,
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
// loop — start VM, run probes, wait, restart per restartPolicy.
func (a *agent) runPod(ctx context.Context, pod *corev1.Pod, vm *podVM) {
	defer func() {
		a.mu.Lock()
		delete(a.vms, pod.UID)
		a.mu.Unlock()
	}()

	fail := func(reason string, err error) {
		log.Printf("pod %s/%s: %s: %v", pod.Namespace, pod.Name, reason, err)
		a.setPhase(ctx, pod, corev1.PodFailed, reason, err.Error(), nil)
	}

	dir, rootfs, err := a.preparePod(pod)
	if err != nil {
		fail("StartError", err)
		return
	}
	c := pod.Spec.Containers[0]

	memMB := int64(256)
	if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		memMB = m.Value() / (1024 * 1024)
	}
	cpus := int64(1)
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		cpus = q.Value()
	}

	for attempt := 0; ; attempt++ {
		logf, err := os.OpenFile(filepath.Join(dir, "container.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fail("StartError", err)
			return
		}
		cmd := exec.Command(a.harness,
			"--kernel", a.kernel,
			"--rootfs", rootfs,
			"--cpus", fmt.Sprintf("%d", cpus),
			"--mem", fmt.Sprintf("%d", memMB),
			"--log", filepath.Join(dir, "vmm.log"),
			"--vsock-exec", execSockPath(pod.UID),
			"--", "/bin/sh", "/entry.sh")
		cmd.Stdout = logf
		cmd.Stderr = logf
		if err := cmd.Start(); err != nil {
			logf.Close()
			fail("StartError", err)
			return
		}
		vm.setCmd(cmd)
		log.Printf("pod %s/%s: microVM started (pid %d, cpus=%d, mem=%dMiB, restarts=%d)",
			pod.Namespace, pod.Name, cmd.Process.Pid, cpus, memMB, vm.restartCount())

		started := metav1.Now()
		// Ready immediately only if there's no readiness (or startup) probe.
		vm.setReady(c.ReadinessProbe == nil && c.StartupProbe == nil)
		a.setPhase(ctx, pod, corev1.PodRunning, "", "", &corev1.ContainerStatus{
			Name:         c.Name,
			Ready:        vm.getReady(),
			RestartCount: vm.restartCount(),
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: started},
			},
			Image: c.Image,
		})

		probeStop := make(chan struct{})
		go a.runProbes(ctx, pod, vm, probeStop)

		err = cmd.Wait()
		close(probeStop)
		logf.Close()

		exitCode := int32(0)
		csReason := "Completed"
		if err != nil {
			csReason = "Error"
			exitCode = 1
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = int32(ee.ExitCode())
			}
		}
		finished := metav1.Now()
		terminated := &corev1.ContainerStatus{
			Name:         c.Name,
			Ready:        false,
			RestartCount: vm.restartCount(),
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   exitCode,
					Reason:     csReason,
					StartedAt:  started,
					FinishedAt: finished,
				},
			},
			Image: c.Image,
		}
		log.Printf("pod %s/%s: microVM exited (code %d)", pod.Namespace, pod.Name, exitCode)

		if vm.isStopping() {
			// Deletion in progress: report final state and finalize.
			phase := corev1.PodSucceeded
			if exitCode != 0 {
				phase = corev1.PodFailed
			}
			a.setPhase(context.Background(), pod, phase, "", "", terminated)
			zero := int64(0)
			a.client.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})
			return
		}

		policy := pod.Spec.RestartPolicy
		if policy == "" {
			policy = corev1.RestartPolicyAlways
		}
		if policy == corev1.RestartPolicyNever ||
			(policy == corev1.RestartPolicyOnFailure && exitCode == 0) {
			phase := corev1.PodSucceeded
			if exitCode != 0 {
				phase = corev1.PodFailed
			}
			a.setPhase(context.Background(), pod, phase, "", "", terminated)
			return
		}

		// Restarting. Naive crash backoff, reset after a long healthy run.
		if finished.Sub(started.Time) > 5*time.Minute {
			attempt = 0
		}
		backoff := time.Duration(1<<min(attempt, 5)) * time.Second
		vm.mu.Lock()
		vm.restarts++
		vm.mu.Unlock()
		terminated.State = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CrashLoopBackOff",
				Message: fmt.Sprintf("restarting in %s", backoff),
			},
		}
		terminated.RestartCount = vm.restartCount()
		a.setPhase(context.Background(), pod, corev1.PodRunning, "", "", terminated)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if vm.isStopping() {
			zero := int64(0)
			a.client.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: &zero})
			return
		}
	}
}

// preparePod pulls the image and materializes the pod's rootfs. Note: the
// rootfs is reused across container restarts (real kubelet gives restarted
// containers a fresh filesystem).
func (a *agent) preparePod(pod *corev1.Pod) (string, string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", "", fmt.Errorf("no containers")
	}
	c := pod.Spec.Containers[0]

	// Pull the image (cached after first use).
	rootfsBase, err := a.ensureImage(c.Image)
	if err != nil {
		return "", "", fmt.Errorf("pulling image %q: %w", c.Image, err)
	}

	dir := filepath.Join(a.workDir, string(pod.UID))
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	// APFS clonefile copy of the base rootfs: instant, copy-on-write.
	if _, err := os.Stat(rootfs); os.IsNotExist(err) {
		if out, err := exec.Command("/bin/cp", "-Rc", rootfsBase, rootfs).CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("cloning rootfs: %v: %s", err, out)
		}
	}

	// The guest boots into execd, which supervises the workload (on a pty
	// when the pod asks for one) and serves exec/attach over vsock. Workload
	// argv travels in a spec file: libkrun passes exec args via the kernel
	// cmdline, which splits on whitespace, so it must go out-of-band.
	if out, err := exec.Command("/bin/cp", "-c", a.execdPath, filepath.Join(rootfs, "execd")).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("installing execd: %v: %s", err, out)
	}
	argv := append(append([]string{}, c.Command...), c.Args...)
	if len(argv) == 0 {
		argv = imageArgv(rootfsBase)
	}
	if len(argv) == 0 {
		return "", "", fmt.Errorf("no command: pod spec has none and image config has no Entrypoint/Cmd")
	}
	specJSON, err := json.Marshal(map[string]any{"argv": argv, "tty": c.TTY})
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(rootfs, ".podvm-spec.json"), specJSON, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(rootfs, "entry.sh"), []byte("#!/bin/sh\nexec /execd\n"), 0o755); err != nil {
		return "", "", err
	}
	return dir, rootfs, nil
}

func (a *agent) setPhase(ctx context.Context, pod *corev1.Pod, phase corev1.PodPhase, reason, message string, cs *corev1.ContainerStatus) {
	latest, err := a.client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
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
	ready := corev1.ConditionFalse
	if phase == corev1.PodRunning {
		a.mu.Lock()
		vm := a.vms[pod.UID]
		a.mu.Unlock()
		if vm == nil || vm.getReady() {
			ready = corev1.ConditionTrue
		}
	}
	latest.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: ready, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now},
	}
	if cs != nil {
		latest.Status.ContainerStatuses = []corev1.ContainerStatus{*cs}
	}
	if _, err := a.client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{}); err != nil {
		log.Printf("updating status for %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}
