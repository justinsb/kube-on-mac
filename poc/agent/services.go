package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// The lazy service protocol (see research/services.md): execd dials the
// per-pod svc socket (guest vsock port 1025) when a pod opens a flow to a
// ClusterIP it hasn't seen, and asks for endpoints. JSON lines.

type svcQuery struct {
	VIP   string `json:"vip,omitempty"`
	Port  int    `json:"port,omitempty"`
	Proto string `json:"proto,omitempty"` // "tcp" or "udp"

	// DNS-style lookup instead: service name -> ClusterIPs.
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type svcAnswer struct {
	Endpoints  []string `json:"endpoints"` // pod IPv6 addresses
	TargetPort int      `json:"targetPort"`
	ClusterIPs []string `json:"clusterIPs,omitempty"`
	TTLSeconds int      `json:"ttl"`
	Error      string   `json:"error,omitempty"`
}

// svcSockPath is the host unix socket bridging guest-initiated vsock
// connections (port 1025) for a pod VM.
func svcSockPath(uid types.UID) string {
	short := string(uid)
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join("/tmp", "podvm-"+short+".svc")
}

// startSvcListener serves endpoint queries for one pod VM. libkrun connects
// to this unix socket whenever the guest dials vsock port 1025.
func (a *agent) startSvcListener(ctx context.Context, uid types.UID) (func(), error) {
	sock := svcSockPath(uid)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go a.serveSvcConn(ctx, conn)
		}
	}()
	stop := func() { ln.Close(); os.Remove(sock) }
	return stop, nil
}

func (a *agent) serveSvcConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var q svcQuery
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			enc.Encode(svcAnswer{Error: "bad query: " + err.Error()})
			continue
		}
		enc.Encode(a.resolveService(ctx, q))
	}
}

// resolveService maps a ClusterIP:port to ready endpoint pod IPs, computed
// directly: Service selector -> Running+Ready pods. (kube-controller-manager
// now writes real EndpointSlices; switching this to consume them instead of
// re-deriving from selectors is a possible simplification.)
func (a *agent) resolveService(ctx context.Context, q svcQuery) svcAnswer {
	if a.cs() == nil {
		// Static pods run before the apiserver; there are no services yet.
		return svcAnswer{Error: "apiserver not available yet", TTLSeconds: 1}
	}
	if q.Name != "" {
		return a.resolveServiceName(ctx, q)
	}
	proto := strings.ToUpper(q.Proto)
	if proto == "" {
		proto = "TCP"
	}
	svcs, err := a.cs().CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return svcAnswer{Error: err.Error()}
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if !clusterIPMatches(svc, q.VIP) {
			continue
		}
		var targetPort int
		found := false
		for _, p := range svc.Spec.Ports {
			if int(p.Port) == q.Port && (string(p.Protocol) == proto || p.Protocol == "") {
				targetPort = p.TargetPort.IntValue()
				if targetPort == 0 {
					if p.TargetPort.String() == "" || p.TargetPort.String() == "0" {
						targetPort = q.Port // unset targetPort defaults to port
					} else {
						return svcAnswer{Error: "named targetPorts not supported in PoC"}
					}
				}
				found = true
				break
			}
		}
		if !found {
			return svcAnswer{Error: "no matching service port"}
		}
		if len(svc.Spec.Selector) == 0 {
			return svcAnswer{Error: "selector-less services not supported in PoC"}
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		pods, err := a.cs().CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: sel.String(),
		})
		if err != nil {
			return svcAnswer{Error: err.Error()}
		}
		var eps []string
		for j := range pods.Items {
			pod := &pods.Items[j]
			if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
				continue
			}
			if !podReady(pod) {
				continue
			}
			eps = append(eps, pod.Status.PodIP)
		}
		log.Printf("service query [%s]:%d/%s -> %s/%s targetPort=%d endpoints=%v",
			q.VIP, q.Port, proto, svc.Namespace, svc.Name, targetPort, eps)
		return svcAnswer{Endpoints: eps, TargetPort: targetPort, TTLSeconds: 30}
	}
	return svcAnswer{Error: "no service with ClusterIP " + q.VIP}
}

// resolveServiceName is the DNS half of the lazy protocol: service name ->
// ClusterIPs (execd serves them as A/AAAA records in the guest).
func (a *agent) resolveServiceName(ctx context.Context, q svcQuery) svcAnswer {
	ns := q.Namespace
	if ns == "" {
		ns = "default"
	}
	svc, err := a.cs().CoreV1().Services(ns).Get(ctx, q.Name, metav1.GetOptions{})
	if err != nil {
		return svcAnswer{Error: err.Error(), TTLSeconds: 5}
	}
	ips := svc.Spec.ClusterIPs
	if len(ips) == 0 && svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != corev1.ClusterIPNone {
		ips = []string{svc.Spec.ClusterIP}
	}
	log.Printf("dns query %s.%s -> %v", q.Name, ns, ips)
	return svcAnswer{ClusterIPs: ips, TTLSeconds: 5}
}

func clusterIPMatches(svc *corev1.Service, vip string) bool {
	if svc.Spec.ClusterIP == vip {
		return true
	}
	for _, ip := range svc.Spec.ClusterIPs {
		if ip == vip {
			return true
		}
	}
	return false
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
