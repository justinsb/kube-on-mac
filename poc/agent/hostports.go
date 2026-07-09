package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// hostPort support: containers[].ports[].hostPort is honored by asking the
// pod's gvproxy (its control API listens on a per-pod unix socket) to
// forward 127.0.0.1:<hostPort> on the macOS host to the guest. This is how
// the host reaches node-published pods — e.g. the future apiserver static
// pod at 127.0.0.1:6443 — with no sudo and no dependency on pod IPs: the
// forward is re-created with the pod on every restart.
//
// Divergence: forwards bind 127.0.0.1 (not hostIP/0.0.0.0) — this is a
// single-user dev machine and nothing should expose pods on the LAN.

func gvproxyAPISockPath(uid types.UID) string {
	short := string(uid)
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join("/tmp", "podvm-"+short+".gvapi")
}

func collectHostPorts(pod *corev1.Pod) [][2]int32 {
	var out [][2]int32
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort != 0 && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
				cp := p.ContainerPort
				if cp == 0 {
					cp = p.HostPort
				}
				out = append(out, [2]int32{p.HostPort, cp})
			}
		}
	}
	return out
}

// exposeHostPorts posts forwarder rules to a pod's gvproxy control API.
// The guest side of every pod's gvproxy network is fixed (192.168.127.2).
func exposeHostPorts(apiSock string, ports [][2]int32) error {
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 5 * time.Second,
	}
	for _, p := range ports {
		body := fmt.Sprintf(`{"local":"127.0.0.1:%d","remote":"192.168.127.2:%d","protocol":"tcp"}`, p[0], p[1])
		var lastErr error
		// gvproxy's API becomes ready shortly after its sockets appear.
		for attempt := 0; attempt < 20; attempt++ {
			resp, err := httpc.Post("http://gvproxy/services/forwarder/expose",
				"application/json", strings.NewReader(body))
			if err == nil {
				msg, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					lastErr = nil
					break
				}
				lastErr = fmt.Errorf("expose %d->%d: %s: %s", p[0], p[1], resp.Status, strings.TrimSpace(string(msg)))
			} else {
				lastErr = err
			}
			time.Sleep(250 * time.Millisecond)
		}
		if lastErr != nil {
			return lastErr
		}
	}
	return nil
}
