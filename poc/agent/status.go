package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Per-container status flows out of the guest two ways: while the VM runs,
// the agent polls execd's "status" op; when the VM exits cleanly, execd's
// final answer is /status.json in the boot share (readable after the guest
// is gone). Both carry the same records.

// guestContainerStatus mirrors execd's containerStatus (poc/execd).
type guestContainerStatus struct {
	Name       string `json:"name"`
	State      string `json:"state"` // Waiting | Running | Terminated
	ExitCode   int32  `json:"exitCode"`
	Restarts   int32  `json:"restarts"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Init       bool   `json:"init,omitempty"`
}

// execdStatus fetches live per-container statuses from the guest.
func (a *agent) execdStatus(uid types.UID) ([]guestContainerStatus, error) {
	fc, err := a.dialExecd(uid)
	if err != nil {
		return nil, err
	}
	defer fc.c.Close()
	fc.c.SetDeadline(time.Now().Add(5 * time.Second))

	reqData, _ := json.Marshal(map[string]any{"op": "status"})
	if err := fc.writeFrame(frameRequest, reqData); err != nil {
		return nil, err
	}
	var body []byte
	for {
		typ, payload, err := fc.readFrame()
		if err != nil {
			return nil, err
		}
		switch typ {
		case frameStdout:
			body = append(body, payload...)
		case frameExit:
			var sts []guestContainerStatus
			if err := json.Unmarshal(body, &sts); err != nil {
				return nil, fmt.Errorf("parsing status: %w", err)
			}
			return sts, nil
		}
	}
}

// readFinalStatus reads execd's exit report (written into the boot virtiofs
// share just before the VM exits). ok=false means the VM died mid-flight.
func readFinalStatus(stateDir string) ([]guestContainerStatus, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, "rootfs", "status.json"))
	if err != nil {
		return nil, false
	}
	var sts []guestContainerStatus
	if err := json.Unmarshal(data, &sts); err != nil {
		return nil, false
	}
	return sts, true
}

// statusLoop keeps the API's per-container view in sync with the guest
// while the VM runs.
func (a *agent) statusLoop(ctx context.Context, pod *corev1.Pod, vm *podVM, stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-tick.C:
			a.pushStatus(ctx, pod, vm)
		}
	}
}

// pushStatus polls the guest once and publishes phase + container statuses —
// but only when something changed since the last push, so the 5s poll doesn't
// turn into a constant stream of no-op UpdateStatus calls.
func (a *agent) pushStatus(ctx context.Context, pod *corev1.Pod, vm *podVM) {
	sts, err := a.execdStatus(vm.podUID)
	if err != nil {
		return // guest not up (yet / anymore); the next tick or exit path reports
	}
	ics, css, phase := a.translateStatuses(pod, sts, vm)
	digest, _ := json.Marshal(map[string]any{"p": phase, "i": ics, "c": css})
	vm.mu.Lock()
	same := vm.lastPush == string(digest)
	vm.lastPush = string(digest)
	vm.mu.Unlock()
	if same {
		return
	}
	a.setPhase(ctx, pod, phase, "", "", ics, css)
}

// translateStatuses maps guest statuses onto API container statuses, in pod
// spec order, and derives the phase: Pending until every init container has
// succeeded, Running after.
func (a *agent) translateStatuses(pod *corev1.Pod, sts []guestContainerStatus, vm *podVM) (ics, css []corev1.ContainerStatus, phase corev1.PodPhase) {
	byName := map[string]*guestContainerStatus{}
	for i := range sts {
		byName[sts[i].Name] = &sts[i]
	}
	conv := func(c corev1.Container, init bool) corev1.ContainerStatus {
		cs := corev1.ContainerStatus{Name: c.Name, Image: c.Image}
		st := byName[c.Name]
		if st == nil {
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			}
			return cs
		}
		cs.RestartCount = st.Restarts
		switch st.State {
		case "Running":
			cs.State = corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.Unix(st.StartedAt, 0)},
			}
			cs.Ready = !init && vm.containerReady(c.Name)
		case "Terminated":
			reason := "Completed"
			if st.ExitCode != 0 {
				reason = "Error"
			}
			cs.State = corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   st.ExitCode,
					Reason:     reason,
					Message:    st.Reason,
					StartedAt:  metav1.Unix(st.StartedAt, 0),
					FinishedAt: metav1.Unix(st.FinishedAt, 0),
				},
			}
			cs.Ready = init && st.ExitCode == 0
		default: // Waiting
			reason := "ContainerCreating"
			if strings.HasPrefix(st.Reason, "CrashLoopBackOff") {
				reason = "CrashLoopBackOff"
			}
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: st.Reason},
			}
		}
		return cs
	}
	for _, c := range pod.Spec.InitContainers {
		ics = append(ics, conv(c, true))
	}
	for _, c := range pod.Spec.Containers {
		css = append(css, conv(c, false))
	}
	phase = corev1.PodRunning
	for _, cs := range ics {
		if cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
			phase = corev1.PodPending
		}
	}
	return ics, css, phase
}

// finalPhase derives the terminal pod phase from final container statuses:
// Succeeded only if the inits and every main container exited 0.
func finalPhase(ics, css []corev1.ContainerStatus) corev1.PodPhase {
	for _, cs := range append(append([]corev1.ContainerStatus{}, ics...), css...) {
		if cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
			return corev1.PodFailed
		}
	}
	return corev1.PodSucceeded
}

// waitingStatuses builds a uniform Waiting status for every container —
// used for pod-level conditions (image pull, mount, VM start/crash).
func waitingStatuses(cs []corev1.Container, reason, message string) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for _, c := range cs {
		out = append(out, corev1.ContainerStatus{
			Name:  c.Name,
			Image: c.Image,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
			},
		})
	}
	return out
}
