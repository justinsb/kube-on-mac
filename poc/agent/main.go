// PoC node agent for kube-on-macos.
//
// This is milestone-0 of "reimplement the kubelet API contract for macOS":
// it boots a real kube-apiserver+etcd locally (via envtest binaries),
// registers a Node, binds pending pods to it, and reconciles each bound pod
// into a Linux microVM launched through the podvm harness (libkrun on
// Hypervisor.framework). Pod phase/status is reported back honestly.
//
// Deliberately NOT the kubelet contract yet: no :10250 server (exec/logs/
// port-forward), no probes, no volumes beyond the rootfs, image field is
// ignored (all pods run the shared Alpine rootfs), no pod networking.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
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
	cmd     *exec.Cmd
	podUID  types.UID
	name    string
	ns      string
	done    chan struct{}
	exitErr error
}

type agent struct {
	client     *kubernetes.Clientset
	nodeName   string
	harness    string
	kernel     string
	rootfsBase string
	workDir    string

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
		assetsDir     = flag.String("assets", "", "envtest binary assets dir (kube-apiserver, etcd)")
		kubeconfigOut = flag.String("kubeconfig-out", "../_artifacts/kubeconfig", "where to write admin kubeconfig")
	)
	flag.Parse()

	for _, p := range []*string{harness, kernel, rootfsBase, workDir, kubeconfigOut} {
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
		Append("disable-admission-plugins", "ServiceAccount")

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
		client:     kubernetes.NewForConfigOrDie(cfg),
		nodeName:   *nodeName,
		harness:    *harness,
		kernel:     *kernel,
		rootfsBase: *rootfsBase,
		workDir:    *workDir,
		vms:        map[types.UID]*podVM{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; log.Printf("shutting down"); cancel() }()

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
		if vm.cmd.Process != nil {
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
				"kubernetes.io/os":          "linux",
				"kubernetes.io/arch":        "arm64",
				"kubernetes.io/hostname":    a.nodeName,
				"kube-on-macos.io/runtime":  "vm",
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
		if running && vm.cmd.Process != nil {
			log.Printf("pod %s/%s deleted; killing VM", pod.Namespace, pod.Name)
			vm.cmd.Process.Signal(syscall.SIGTERM)
		}
		zero := int64(0)
		a.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name,
			metav1.DeleteOptions{GracePeriodSeconds: &zero})
		a.mu.Lock()
		delete(a.vms, pod.UID)
		a.mu.Unlock()
		return
	}

	if running || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return
	}

	if err := a.startPod(ctx, pod); err != nil {
		log.Printf("starting pod %s/%s: %v", pod.Namespace, pod.Name, err)
		a.setPhase(ctx, pod, corev1.PodFailed, "StartError", err.Error(), nil)
	}
}

func (a *agent) startPod(ctx context.Context, pod *corev1.Pod) error {
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("no containers")
	}
	c := pod.Spec.Containers[0]

	dir := filepath.Join(a.workDir, string(pod.UID))
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// APFS clonefile copy of the base rootfs: instant, copy-on-write.
	if _, err := os.Stat(rootfs); os.IsNotExist(err) {
		if out, err := exec.Command("/bin/cp", "-Rc", a.rootfsBase, rootfs).CombinedOutput(); err != nil {
			return fmt.Errorf("cloning rootfs: %v: %s", err, out)
		}
	}

	// The workload command travels as a script file in the guest rootfs:
	// libkrun passes exec args via the kernel cmdline, which splits on
	// whitespace, so argv goes out-of-band.
	argv := append(append([]string{}, c.Command...), c.Args...)
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-c", "echo kube-on-macos PoC: no command specified; sleep 30"}
	}
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\nexec")
	for _, arg := range argv {
		sb.WriteString(" '")
		sb.WriteString(strings.ReplaceAll(arg, "'", `'\''`))
		sb.WriteString("'")
	}
	sb.WriteString("\n")
	if err := os.WriteFile(filepath.Join(rootfs, "entry.sh"), []byte(sb.String()), 0o755); err != nil {
		return err
	}

	memMB := int64(256)
	if m, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		memMB = m.Value() / (1024 * 1024)
	}
	cpus := int64(1)
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		cpus = q.Value()
	}

	logf, err := os.Create(filepath.Join(dir, "container.log"))
	if err != nil {
		return err
	}

	cmd := exec.Command(a.harness,
		"--kernel", a.kernel,
		"--rootfs", rootfs,
		"--cpus", fmt.Sprintf("%d", cpus),
		"--mem", fmt.Sprintf("%d", memMB),
		"--", "/bin/sh", "/entry.sh")
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}

	vm := &podVM{cmd: cmd, podUID: pod.UID, name: pod.Name, ns: pod.Namespace, done: make(chan struct{})}
	a.mu.Lock()
	a.vms[pod.UID] = vm
	a.mu.Unlock()
	log.Printf("pod %s/%s: microVM started (pid %d, cpus=%d, mem=%dMiB)",
		pod.Namespace, pod.Name, cmd.Process.Pid, cpus, memMB)

	started := metav1.Now()
	a.setPhase(ctx, pod, corev1.PodRunning, "", "", &corev1.ContainerStatus{
		Name:  c.Name,
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: started},
		},
		Image:   c.Image,
		ImageID: "podvm://shared-alpine-rootfs",
	})

	go func() {
		defer logf.Close()
		err := cmd.Wait()
		close(vm.done)
		exitCode := int32(0)
		phase := corev1.PodSucceeded
		csReason := "Completed"
		if err != nil {
			phase = corev1.PodFailed
			csReason = "Error"
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = int32(ee.ExitCode())
			}
		}
		log.Printf("pod %s/%s: microVM exited (code %d)", pod.Namespace, pod.Name, exitCode)
		a.setPhase(context.Background(), pod, phase, "", "", &corev1.ContainerStatus{
			Name:  c.Name,
			Ready: false,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   exitCode,
					Reason:     csReason,
					StartedAt:  started,
					FinishedAt: metav1.Now(),
				},
			},
			Image:   c.Image,
			ImageID: "podvm://shared-alpine-rootfs",
		})
		a.mu.Lock()
		delete(a.vms, pod.UID)
		a.mu.Unlock()
	}()
	return nil
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
		ready = corev1.ConditionTrue
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
