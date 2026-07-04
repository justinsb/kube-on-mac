package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/tools/remotecommand"
	remotecommandserver "k8s.io/kubelet/pkg/cri/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"
)

// Frame protocol shared with execd (see poc/execd/main.go).
const (
	frameStdin      = 0
	frameStdout     = 1
	frameStderr     = 2
	frameExit       = 3
	frameResize     = 4
	frameStdinClose = 5
	frameRequest    = 6
)

type execdConn struct {
	mu sync.Mutex
	c  net.Conn
}

func (f *execdConn) writeFrame(typ byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := f.c.Write(hdr[:]); err != nil {
		return err
	}
	_, err := f.c.Write(payload)
	return err
}

func (f *execdConn) readFrame() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.c, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	payload := make([]byte, n)
	if _, err := io.ReadFull(f.c, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// execSockPath returns the host unix socket bridging to the pod VM's execd.
// It lives under /tmp because sun_path is limited to ~104 bytes on macOS —
// the per-pod state dir is far too deep.
func execSockPath(uid types.UID) string {
	short := string(uid)
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join("/tmp", "podvm-"+short+".sock")
}

// dialExecd connects to the pod VM's exec socket, retrying briefly: the
// socket exists from VM creation but the guest daemon needs a moment to
// start listening.
func (a *agent) dialExecd(uid types.UID) (*execdConn, error) {
	sock := execSockPath(uid)
	var lastErr error
	for i := 0; i < 20; i++ {
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			return &execdConn{c: c}, nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("dialing execd: %w", lastErr)
}

// session pumps one exec/attach session between the streaming server's
// io streams and execd's frame protocol.
func (a *agent) session(ctx context.Context, uid types.UID, req map[string]any,
	in io.Reader, out, errw io.WriteCloser, resize <-chan remotecommand.TerminalSize) error {

	fc, err := a.dialExecd(uid)
	if err != nil {
		return err
	}
	defer fc.c.Close()

	reqData, _ := json.Marshal(req)
	if err := fc.writeFrame(frameRequest, reqData); err != nil {
		return err
	}

	// Cancel the connection when the client goes away.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			fc.c.Close()
		case <-done:
		}
	}()

	if resize != nil {
		go func() {
			for sz := range resize {
				data, _ := json.Marshal(map[string]uint16{"Cols": sz.Width, "Rows": sz.Height})
				if fc.writeFrame(frameResize, data) != nil {
					return
				}
			}
		}()
	}

	if in != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := in.Read(buf)
				if n > 0 {
					if fc.writeFrame(frameStdin, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					fc.writeFrame(frameStdinClose, nil)
					return
				}
			}
		}()
	}

	for {
		typ, payload, err := fc.readFrame()
		if err != nil {
			// Connection dropped (e.g. VM exited while attached): treat as
			// a clean end of session.
			return nil
		}
		switch typ {
		case frameStdout:
			if out != nil {
				out.Write(payload)
			}
		case frameStderr:
			if errw != nil {
				errw.Write(payload)
			}
		case frameExit:
			var e struct {
				Code  int    `json:"code"`
				Error string `json:"error"`
			}
			json.Unmarshal(payload, &e)
			if e.Code == 0 {
				return nil
			}
			msg := e.Error
			if msg == "" {
				msg = fmt.Sprintf("command terminated with exit code %d", e.Code)
			}
			return utilexec.CodeExitError{Err: fmt.Errorf("%s", msg), Code: e.Code}
		}
	}
}

// ExecInContainer implements the kubelet streaming Executor interface.
func (a *agent) ExecInContainer(ctx context.Context, name string, uid types.UID, container string,
	cmd []string, in io.Reader, out, errw io.WriteCloser, tty bool,
	resize <-chan remotecommand.TerminalSize, timeout time.Duration) error {
	return a.session(ctx, uid, map[string]any{"op": "exec", "argv": cmd, "tty": tty}, in, out, errw, resize)
}

// AttachContainer implements the kubelet streaming Attacher interface.
func (a *agent) AttachContainer(ctx context.Context, name string, uid types.UID, container string,
	in io.Reader, out, errw io.WriteCloser, tty bool,
	resize <-chan remotecommand.TerminalSize) error {
	return a.session(ctx, uid, map[string]any{"op": "attach"}, in, out, errw, resize)
}

// splitStreamPath parses /exec/{ns}/{pod}/{container} (or /attach/...).
func splitStreamPath(path, prefix string) (ns, pod, container string, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func (a *agent) handleExec(w http.ResponseWriter, r *http.Request) {
	ns, podName, container, ok := splitStreamPath(r.URL.Path, "/exec/")
	if !ok {
		http.Error(w, "expected /exec/{namespace}/{pod}/{container}", http.StatusNotFound)
		return
	}
	pod, err := a.client.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	streamOpts, err := remotecommandserver.NewOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := r.URL.Query()["command"]
	remotecommandserver.ServeExec(w, r, a, podName, pod.UID, container, cmd, streamOpts,
		time.Hour, 30*time.Second, remotecommandconsts.SupportedStreamingProtocols)
}

func (a *agent) handleAttach(w http.ResponseWriter, r *http.Request) {
	ns, podName, container, ok := splitStreamPath(r.URL.Path, "/attach/")
	if !ok {
		http.Error(w, "expected /attach/{namespace}/{pod}/{container}", http.StatusNotFound)
		return
	}
	pod, err := a.client.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	streamOpts, err := remotecommandserver.NewOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	remotecommandserver.ServeAttach(w, r, a, podName, pod.UID, container, streamOpts,
		time.Hour, 30*time.Second, remotecommandconsts.SupportedStreamingProtocols)
}
