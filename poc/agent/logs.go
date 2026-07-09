package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// startKubeletServer serves the slice of the kubelet API the apiserver
// proxies to (/containerLogs, /exec, /attach). TLS both ways, from the
// declarative PKI: it serves pki/kubelet-server.crt (which the apiserver
// verifies via --kubelet-certificate-authority), and it requires a
// CA-signed client certificate, accepting only the apiserver's kubelet
// client identity (or a system:masters cert). This replaces both the
// ephemeral self-signed cert and the everyone-welcome policy. Delegated
// TokenReview/SubjectAccessReview would be the fuller kubelet contract.
func (a *agent) startKubeletServer(ctx context.Context, port int) error {
	cert, err := tls.LoadX509KeyPair(a.kubeletCert, a.kubeletKey)
	if err != nil {
		return fmt.Errorf("loading kubelet serving cert (run pki?): %w", err)
	}
	caPEM, err := os.ReadFile(a.clientCA)
	if err != nil {
		return fmt.Errorf("loading client CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("no certificates in %s", a.clientCA)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/containerLogs/", a.handleContainerLogs)
	mux.HandleFunc("/exec/", a.handleExec)
	mux.HandleFunc("/attach/", a.handleAttach)

	srv := &http.Server{
		Handler: authorizeKubeletClient(mux),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    caPool,
		},
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("kubelet server: %v", err)
		}
	}()
	log.Printf("kubelet server listening on https://127.0.0.1:%d (client-cert authn)", port)
	return nil
}

// authorizeKubeletClient is the authz half: the TLS layer has verified the
// client cert chains to our CA; only the apiserver's kubelet-client
// identity (or a superuser cert) may use the kubelet API.
func authorizeKubeletClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		peer := r.TLS.PeerCertificates[0]
		authorized := peer.Subject.CommonName == "kube-apiserver-kubelet-client"
		for _, org := range peer.Subject.Organization {
			if org == "system:masters" {
				authorized = true
			}
		}
		if !authorized {
			http.Error(w, fmt.Sprintf("subject %q is not authorized to use the kubelet API", peer.Subject.CommonName), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GET /containerLogs/{namespace}/{pod}/{container}?follow=true&tailLines=N
func (a *agent) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/containerLogs/"), "/"), "/")
	if len(parts) != 3 {
		http.Error(w, "expected /containerLogs/{namespace}/{pod}/{container}", http.StatusNotFound)
		return
	}
	ns, podName, container := parts[0], parts[1], parts[2]

	client := a.cs()
	if client == nil {
		http.Error(w, "apiserver not available yet", http.StatusServiceUnavailable)
		return
	}
	pod, err := client.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	uid := podStateUID(pod)
	// execd writes per-container logs into the boot share; the merged
	// console stream (container.log) remains as a fallback for legacy
	// single-container dir-mode pods.
	logPath := filepath.Join(a.workDir, string(uid), "rootfs", "logs", container+".log")
	if _, err := os.Stat(logPath); err != nil {
		logPath = filepath.Join(a.workDir, string(uid), "container.log")
	}
	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("no logs for pod %s/%s: %v", ns, podName, err), http.StatusNotFound)
		return
	}
	defer f.Close()

	q := r.URL.Query()
	follow := q.Get("follow") == "true"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)

	if n, err := strconv.Atoi(q.Get("tailLines")); err == nil && n >= 0 {
		if err := seekToLastLines(f, n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			if !follow {
				return
			}
			// Keep following until the VM is gone and we've drained.
			if !a.vmRunning(uid) {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		if readErr != nil {
			return
		}
	}
}

func (a *agent) vmRunning(uid types.UID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.vms[uid]
	return ok
}

// seekToLastLines positions f so that only the final n lines remain to be
// read. PoC logs are small; read the whole file.
func seekToLastLines(f *os.File, n int) error {
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = f.Seek(0, io.SeekEnd)
		return err
	}
	lines := bytes.Count(data, []byte("\n"))
	skip := lines - n
	if skip <= 0 {
		_, err = f.Seek(0, io.SeekStart)
		return err
	}
	off := 0
	for i := 0; i < skip; i++ {
		idx := bytes.IndexByte(data[off:], '\n')
		off += idx + 1
	}
	_, err = f.Seek(int64(off), io.SeekStart)
	return err
}

