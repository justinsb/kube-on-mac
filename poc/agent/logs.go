package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
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
// proxies to: for now just /containerLogs, which is what `kubectl logs`
// uses. HTTPS with an ephemeral self-signed cert — the apiserver does not
// verify the kubelet's serving cert unless --kubelet-certificate-authority
// is set. No authn/authz (PoC; the real agent must do delegated
// TokenReview/SubjectAccessReview here).
func (a *agent) startKubeletServer(ctx context.Context, port int) error {
	cert, err := selfSignedCert(a.nodeName)
	if err != nil {
		return fmt.Errorf("generating serving cert: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/containerLogs/", a.handleContainerLogs)

	srv := &http.Server{
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
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
	log.Printf("kubelet server (logs) listening on https://127.0.0.1:%d", port)
	return nil
}

// GET /containerLogs/{namespace}/{pod}/{container}?follow=true&tailLines=N
func (a *agent) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/containerLogs/"), "/"), "/")
	if len(parts) != 3 {
		http.Error(w, "expected /containerLogs/{namespace}/{pod}/{container}", http.StatusNotFound)
		return
	}
	ns, podName := parts[0], parts[1]

	pod, err := a.client.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	logPath := filepath.Join(a.workDir, string(pod.UID), "container.log")
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
			if !a.vmRunning(pod.UID) {
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

func selfSignedCert(nodeName string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{nodeName},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}
