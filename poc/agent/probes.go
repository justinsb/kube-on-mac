package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runProbes runs the container's startup/readiness/liveness probes against
// the pod VM until stop closes. HTTP/TCP probes execute inside the guest
// via execd (the in-guest equivalent of kubelet probing the pod IP); exec
// probes run through the normal exec channel.
func (a *agent) runProbes(ctx context.Context, pod *corev1.Pod, vm *podVM, stop <-chan struct{}) {
	c := pod.Spec.Containers[0]

	// startupDone gates readiness and liveness, per kubelet semantics.
	startupDone := make(chan struct{})
	if c.StartupProbe == nil {
		close(startupDone)
	} else {
		go a.probeLoop(ctx, pod, vm, stop, nil, c.StartupProbe, "startup",
			func(healthy bool) {
				if healthy {
					select {
					case <-startupDone:
					default:
						close(startupDone)
						log.Printf("pod %s/%s: startup probe succeeded", pod.Namespace, pod.Name)
						if c.ReadinessProbe == nil && vm.setReady(true) {
							a.pushReadiness(ctx, pod, vm, c)
						}
					}
				} else {
					a.killVM(vm, podGracePeriod(pod), "startup probe failed")
				}
			})
	}

	if c.ReadinessProbe != nil {
		go a.probeLoop(ctx, pod, vm, stop, startupDone, c.ReadinessProbe, "readiness",
			func(healthy bool) {
				if vm.setReady(healthy) {
					log.Printf("pod %s/%s: readiness -> %v", pod.Namespace, pod.Name, healthy)
					a.pushReadiness(ctx, pod, vm, c)
				}
			})
	}

	if c.LivenessProbe != nil {
		go a.probeLoop(ctx, pod, vm, stop, startupDone, c.LivenessProbe, "liveness",
			func(healthy bool) {
				if !healthy {
					a.killVM(vm, podGracePeriod(pod), "liveness probe failed")
				}
			})
	}
}

// probeLoop runs one probe on its schedule, invoking report on threshold
// transitions: report(true) once the success threshold is met, report(false)
// once the failure threshold is met. For startup/liveness, report(false) is
// terminal (the VM gets killed); for readiness it toggles.
func (a *agent) probeLoop(ctx context.Context, pod *corev1.Pod, vm *podVM,
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
		err := a.doProbe(ctx, vm, p, timeout)
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
				log.Printf("pod %s/%s: %s probe failed %d times: %v",
					pod.Namespace, pod.Name, kind, failures, err)
				report(false)
				if kind != "readiness" {
					return // startup/liveness failure is terminal for this VM
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

func (a *agent) doProbe(ctx context.Context, vm *podVM, p *corev1.Probe, timeout time.Duration) error {
	switch {
	case p.Exec != nil:
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return a.session(pctx, vm.podUID,
			map[string]any{"op": "exec", "argv": p.Exec.Command, "tty": false},
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

// pushReadiness republishes Running status with the current ready state.
func (a *agent) pushReadiness(ctx context.Context, pod *corev1.Pod, vm *podVM, c corev1.Container) {
	a.setPhase(ctx, pod, corev1.PodRunning, "", "", &corev1.ContainerStatus{
		Name:         c.Name,
		Ready:        vm.getReady(),
		RestartCount: vm.restartCount(),
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
		},
		Image: c.Image,
	})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
