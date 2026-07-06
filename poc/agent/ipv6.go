package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// podIP6 derives a stable IPv6 address for a pod from the node's /64 and a
// hash of the pod UID. Deterministic across agent restarts; collisions are
// 2^-64-unlikely and would only matter for concurrently-running pods.
func podIP6(prefix netip.Prefix, uid types.UID) netip.Addr {
	h := sha256.Sum256([]byte(uid))
	host := binary.BigEndian.Uint64(h[:8])
	if host < 2 {
		host += 2 // avoid ::0 (subnet router anycast) and ::1 (host)
	}
	b := prefix.Addr().As16()
	binary.BigEndian.PutUint64(b[8:], host)
	return netip.AddrFrom16(b)
}

// vmnetSockPath is the vmnet-helper unixgram socket for a pod VM (short
// path: sun_path limit).
func vmnetSockPath(uid types.UID) string {
	short := string(uid)
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join("/tmp", "podvm-"+short+".vm")
}

// startVmnetHelper launches a per-pod vmnet-helper on the shared network and
// returns the process and the vmnet-assigned MAC (parsed from the JSON it
// prints on stdout). interface-id = pod UID gives a stable MAC per pod.
func startVmnetHelper(helperPath string, uid types.UID, sock, logPath string) (*exec.Cmd, string, error) {
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.Command(helperPath,
		"--socket", sock,
		"--interface-id", string(uid),
		"--operation-mode", "shared")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logf.Close()
		return nil, "", err
	}
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, "", fmt.Errorf("starting vmnet-helper: %w", err)
	}

	type ifaceInfo struct {
		MAC string `json:"vmnet_mac_address"`
	}
	infoc := make(chan ifaceInfo, 1)
	errc := make(chan error, 1)
	go func() {
		defer logf.Close()
		var info ifaceInfo
		if err := json.NewDecoder(stdout).Decode(&info); err != nil {
			errc <- fmt.Errorf("reading vmnet-helper interface info: %w", err)
			return
		}
		infoc <- info
	}()
	select {
	case info := <-infoc:
		if info.MAC == "" {
			cmd.Process.Kill()
			return nil, "", fmt.Errorf("vmnet-helper reported no MAC")
		}
		return cmd, info.MAC, nil
	case err := <-errc:
		cmd.Process.Kill()
		return nil, "", err
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		return nil, "", fmt.Errorf("timeout waiting for vmnet-helper interface info")
	}
}
