package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

// resolveService maps a ClusterIP:port to ready endpoint pod IPs, answered
// entirely from the watch caches — kube-proxy's diet: the Service for the
// port mapping, its EndpointSlices (written by kube-controller-manager from
// the readiness we report) for the backends. Zero API requests at query
// time: this path sits under a guest-side deadline and must never queue
// behind control-loop traffic (research/client-side-rate-limiting.md).
func (a *agent) resolveService(ctx context.Context, q svcQuery) svcAnswer {
	// Bootstrap ClusterIPs (static pod annotation) resolve from the agent's
	// own table, apiserver or not — this is how control-plane clients will
	// reach the apiserver VIP before (and while) the apiserver serves. Ports
	// pass through 1:1.
	if q.VIP != "" {
		if vm := a.staticVIP(q.VIP); vm != nil {
			ip := vm.getIP()
			if ip == "" || vm.isStopping() {
				return svcAnswer{Error: "bootstrap VIP backend not ready", TTLSeconds: 1}
			}
			log.Printf("static vip query [%s]:%d -> %s/%s (%s)", q.VIP, q.Port, vm.ns, vm.name, ip)
			return svcAnswer{Endpoints: []string{ip}, TargetPort: q.Port, TTLSeconds: 5}
		}
	}
	if a.cs() == nil {
		// Static pods run before the apiserver; there are no services yet.
		return svcAnswer{Error: "apiserver not available yet", TTLSeconds: 1}
	}
	if !a.cachesSynced.Load() {
		return svcAnswer{Error: "watch caches not synced yet", TTLSeconds: 1}
	}
	if q.Name != "" {
		return a.resolveServiceName(q)
	}
	proto := strings.ToUpper(q.Proto)
	if proto == "" {
		proto = "TCP"
	}
	svcs, err := a.svcLister.List(labels.Everything())
	if err != nil {
		return svcAnswer{Error: err.Error()}
	}
	for _, svc := range svcs {
		if !clusterIPMatches(svc, q.VIP) {
			continue
		}
		portName, found := "", false
		for _, p := range svc.Spec.Ports {
			if int(p.Port) == q.Port && (string(p.Protocol) == proto || p.Protocol == "") {
				portName = p.Name
				found = true
				break
			}
		}
		if !found {
			return svcAnswer{Error: "no matching service port"}
		}
		eps, targetPort, err := a.endpointsFor(svc, portName, proto)
		if err != nil {
			return svcAnswer{Error: err.Error(), TTLSeconds: 1}
		}
		log.Printf("service query [%s]:%d/%s -> %s/%s targetPort=%d endpoints=%v",
			q.VIP, q.Port, proto, svc.Namespace, svc.Name, targetPort, eps)
		ttl := 30
		if len(eps) == 0 {
			ttl = 5 // recover quickly once a backend becomes ready
		}
		return svcAnswer{Endpoints: eps, TargetPort: targetPort, TTLSeconds: ttl}
	}
	return svcAnswer{Error: "no service with ClusterIP " + q.VIP}
}

// endpointsFor reads a service's EndpointSlices from the watch cache. The
// slices carry ready-ness (maintained by the endpointslice controller from
// pod conditions) and *resolved* port numbers — named targetPorts work for
// free, per-slice.
func (a *agent) endpointsFor(svc *corev1.Service, portName, proto string) ([]string, int, error) {
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	slices, err := a.epsLister.EndpointSlices(svc.Namespace).List(sel)
	if err != nil {
		return nil, 0, err
	}
	var eps []string
	targetPort := 0
	for _, slice := range slices {
		if slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue // pod addresses are IPv6 ULAs; ignore other families
		}
		slicePort := 0
		for _, p := range slice.Ports {
			name := ""
			if p.Name != nil {
				name = *p.Name
			}
			if name != portName {
				continue
			}
			if p.Protocol != nil && !strings.EqualFold(string(*p.Protocol), proto) {
				continue
			}
			if p.Port != nil {
				slicePort = int(*p.Port)
			}
			break
		}
		if slicePort == 0 {
			continue
		}
		targetPort = slicePort
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if len(ep.Addresses) > 0 {
				eps = append(eps, ep.Addresses[0])
			}
		}
	}
	if targetPort == 0 {
		return nil, 0, fmt.Errorf("no endpointslice yet for %s/%s port %q", svc.Namespace, svc.Name, portName)
	}
	return eps, targetPort, nil
}

// resolveServiceName is the DNS half of the lazy protocol: service name ->
// ClusterIPs (execd serves them as A/AAAA records in the guest), from the
// watch cache.
func (a *agent) resolveServiceName(q svcQuery) svcAnswer {
	ns := q.Namespace
	if ns == "" {
		ns = "default"
	}
	svc, err := a.svcLister.Services(ns).Get(q.Name)
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
