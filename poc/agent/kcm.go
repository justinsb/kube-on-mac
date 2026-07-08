package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// startControllerManager runs the real kube-controller-manager against the
// envtest apiserver. envtest ships only apiserver+etcd; KCM is pure Go and
// cross-builds for darwin/arm64 from the kubernetes tree (see README). This
// replaced a PoC deployment controller (see docs/walkthrough.md for that
// arc): with the real thing we get ReplicaSets, rolling updates, endpoints,
// garbage collection, namespace deletion, and ServiceAccount creation for
// free — and the agent goes back to being just a kubelet stand-in.
//
// The service-account token controller signs with the same key envtest gave
// the apiserver (sa-signer.key in the apiserver's cert dir).
func startControllerManager(ctx context.Context, path, kubeconfig, certDir, logPath string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("kube-controller-manager not found at %s (build it from kubernetes@v1.33.0: go build ./cmd/kube-controller-manager)", path)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	args := []string{
		"--kubeconfig", kubeconfig,
		"--service-account-private-key-file", filepath.Join(certDir, "sa-signer.key"),
		"--root-ca-file", filepath.Join(certDir, "apiserver.crt"),
		"--leader-elect=false",
	}
	startOnce := func() (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd, nil
	}
	cmd, err := startOnce()
	if err != nil {
		logf.Close()
		return err
	}
	log.Printf("kube-controller-manager started (pid %d, log %s)", cmd.Process.Pid, logPath)
	go func() {
		for {
			err := cmd.Wait()
			if ctx.Err() != nil {
				return
			}
			log.Printf("kube-controller-manager exited (%v); restarting in 3s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			var serr error
			if cmd, serr = startOnce(); serr != nil {
				log.Printf("restarting kube-controller-manager: %v (giving up)", serr)
				return
			}
			log.Printf("kube-controller-manager restarted (pid %d)", cmd.Process.Pid)
		}
	}()
	return nil
}
