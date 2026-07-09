package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// runProbes runs startup/readiness/liveness probes for every container in
// the pod VM until stop closes. HTTP/TCP probes execute inside the guest via
// execd (the in-guest equivalent of kubelet probing the pod IP — the pod's
// containers share one network namespace, so these are pod-level); exec
// probes run through the normal exec channel, targeted at their container.
func (a *agent) runProbes(ctx context.Context, pod *corev1.Pod, vm *podVM, stop <-chan struct{}) {
	for i := range pod.Spec.Containers {
		a.runContainerProbes(ctx, pod, vm, pod.Spec.Containers[i], stop)
	}
}

func (a *agent) runContainerProbes(ctx context.Context, pod *corev1.Pod, vm *podVM, c corev1.Container, stop <-chan struct{}) {
	// startupDone gates readiness and liveness, per kubelet semantics.
	startupDone := make(chan struct{})
	if c.StartupProbe == nil {
		close(startupDone)
	} else {
		go a.probeLoop(ctx, pod, vm, c, stop, nil, c.StartupProbe, "startup",
			func(healthy bool) {
				if healthy {
					select {
					case <-startupDone:
					default:
						close(startupDone)
						log.Printf("pod %s/%s: container %s startup probe succeeded", pod.Namespace, pod.Name, c.Name)
						if c.ReadinessProbe == nil && vm.setContainerReady(c.Name, true) {
							a.pushStatus(ctx, pod, vm)
						}
					}
				} else {
					a.killContainer(vm, c.Name, podGracePeriod(pod), "startup probe failed")
				}
			})
	}

	if c.ReadinessProbe != nil {
		go a.probeLoop(ctx, pod, vm, c, stop, startupDone, c.ReadinessProbe, "readiness",
			func(healthy bool) {
				if vm.setContainerReady(c.Name, healthy) {
					log.Printf("pod %s/%s: container %s readiness -> %v", pod.Namespace, pod.Name, c.Name, healthy)
					a.pushStatus(ctx, pod, vm)
				}
			})
	}

	if c.LivenessProbe != nil {
		go a.probeLoop(ctx, pod, vm, c, stop, startupDone, c.LivenessProbe, "liveness",
			func(healthy bool) {
				if !healthy {
					a.killContainer(vm, c.Name, podGracePeriod(pod), "liveness probe failed")
				}
			})
	}
}

// killContainer restarts one container in-guest: execd SIGTERMs it (SIGKILL
// after grace) and its supervisor restarts it per restartPolicy — kubelet
// semantics, without touching the pod's other containers. If execd is
// unreachable the whole VM goes, as the only remaining lever.
func (a *agent) killContainer(vm *podVM, container string, grace int64, reason string) {
	log.Printf("pod %s/%s: killing container %s (%s, grace %ds)", vm.ns, vm.name, container, reason, grace)
	if err := a.execdRequest(vm.podUID, map[string]any{
		"op": "kill", "container": container, "grace": grace,
	}, 5*time.Second); err != nil {
		log.Printf("pod %s/%s: container kill unavailable (%v)", vm.ns, vm.name, err)
		a.killVM(vm, grace, reason)
	}
}

// probeLoop runs one probe on its schedule, invoking report on threshold
// transitions: report(true) once the success threshold is met, report(false)
// once the failure threshold is met. For startup/liveness, report(false) is
// terminal (the container gets killed and restarted; its next incarnation
// gets fresh probe state because runProbes outlives it only via this loop —
// the loop keeps probing the replacement); for readiness it toggles.
func (a *agent) probeLoop(ctx context.Context, pod *corev1.Pod, vm *podVM, c corev1.Container,
	stop <-chan struct{}, gate <-chan struct{}, p *corev1.Probe, kind string,
	report func(healthy bool)) {

	period := time.Duration(p.PeriodSeconds) * time.Second
	if period <= 0 {
		period = 10 * time.Second
	}
	failureThreshold := int(p.FailureThreshold)
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	successThreshold := int(p.SuccessThreshold)
	if successThreshold <= 0 {
		successThreshold = 1
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}

	if p.InitialDelaySeconds > 0 {
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(p.InitialDelaySeconds) * time.Second):
		}
	}
	if gate != nil {
		select {
		case <-stop:
			return
		case <-gate:
		}
	}

	var successes, failures int
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		err := a.doProbe(ctx, vm, c.Name, p, timeout)
		if err == nil {
			failures = 0
			successes++
			if successes == successThreshold {
				report(true)
			}
		} else {
			successes = 0
			failures++
			if failures == failureThreshold {
				log.Printf("pod %s/%s: container %s %s probe failed %d times: %v",
					pod.Namespace, pod.Name, c.Name, kind, failures, err)
				report(false)
				if kind != "readiness" {
					// The container is being restarted; give it a fresh run.
					successes, failures = 0, 0
				}
			}
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func (a *agent) doProbe(ctx context.Context, vm *podVM, container string, p *corev1.Probe, timeout time.Duration) error {
	switch {
	case p.Exec != nil:
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return a.session(pctx, vm.podUID,
			map[string]any{"op": "exec", "container": container, "argv": p.Exec.Command, "tty": false},
			nil, nopWriteCloser{io.Discard}, nopWriteCloser{io.Discard}, nil)
	case p.HTTPGet != nil:
		port := p.HTTPGet.Port.IntValue()
		if port == 0 {
			return fmt.Errorf("named probe ports not supported")
		}
		return a.execdRequest(vm.podUID, map[string]any{
			"op": "probe", "ptype": "http", "port": port,
			"path":    p.HTTPGet.Path,
			"scheme":  strings.ToLower(string(p.HTTPGet.Scheme)),
			"timeout": int(timeout / time.Second),
		}, timeout)
	case p.TCPSocket != nil:
		port := p.TCPSocket.Port.IntValue()
		if port == 0 {
			return fmt.Errorf("named probe ports not supported")
		}
		return a.execdRequest(vm.podUID, map[string]any{
			"op": "probe", "ptype": "tcp", "port": port,
			"timeout": int(timeout / time.Second),
		}, timeout)
	default:
		return fmt.Errorf("no probe handler specified")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
