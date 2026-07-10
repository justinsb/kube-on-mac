package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
)

// The vsock control channel: exec/attach/probe/status/shutdown, all
// container-aware ("container" in the request; empty = first container).

func serveVsock(sup *supervisor) {
	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		log.Printf("vsock listen: %v (exec/attach unavailable)", err)
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("vsock accept: %v", err)
			return
		}
		go handleConn(&framedConn{c: conn}, sup)
	}
}

func handleConn(fc *framedConn, sup *supervisor) {
	defer fc.c.Close()
	typ, payload, err := fc.readFrame()
	if err != nil || typ != frameRequest {
		return
	}
	var req request
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}
	switch req.Op {
	case "exec":
		handleExec(fc, req, sup)
	case "attach":
		handleAttach(fc, req, sup)
	case "probe":
		handleProbe(fc, req)
	case "status":
		handleStatus(fc, sup)
	case "kill":
		handleKill(fc, req, sup)
	case "portforward":
		handlePortForward(fc, req)
	case "shutdown":
		handleShutdown(fc, req, sup)
	}
}

// handlePortForward bridges one forwarded connection to a port inside the
// pod, dialed on loopback — the kubelet dials inside the pod's netns, so
// apps bound only to 127.0.0.1 are reachable, matching real port-forward
// semantics (and unlike going via the pod IP).
func handlePortForward(fc *framedConn, req request) {
	if req.Port == 0 {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"no port"}`))
		return
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port), 5*time.Second)
	if err != nil {
		fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":1,"error":%q}`, err.Error())))
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(&frameWriter{fc, frameStdout}, conn)
		close(done)
	}()
	go func() {
		for {
			typ, payload, err := fc.readFrame()
			if err != nil {
				conn.Close() // client went away; unblock the copy above
				return
			}
			switch typ {
			case frameStdin:
				if _, err := conn.Write(payload); err != nil {
					return
				}
			case frameStdinClose:
				if tc, ok := conn.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
			}
		}
	}()
	<-done
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
}

// handleKill restarts one container: SIGTERM (SIGKILL after grace); its
// supervisor sees the exit and applies restartPolicy. Used by the host for
// liveness/startup probe failures — kubelet restarts the container, not the
// pod.
func handleKill(fc *framedConn, req request, sup *supervisor) {
	c := sup.byName(req.Container)
	if c == nil {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"no such container"}`))
		return
	}
	grace := req.Grace
	if grace <= 0 {
		grace = 30
	}
	log.Printf("container %s: kill requested (grace %ds)", c.spec.Name, grace)
	c.terminate(grace)
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
}

func handleStatus(fc *framedConn, sup *supervisor) {
	data, err := json.Marshal(sup.statuses())
	if err != nil {
		return
	}
	fc.writeFrame(frameStdout, data)
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
}

// handleProbe implements tcpSocket/httpGet probes from inside the pod's
// network view — the equivalent of kubelet probing the pod IP. Probes are
// network-level, so no container targeting is needed.
func handleProbe(fc *framedConn, req request) {
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}
	ok := false
	switch req.Ptype {
	case "tcp":
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port), timeout)
		if err == nil {
			conn.Close()
			ok = true
		}
	case "http":
		scheme := req.Scheme
		if scheme == "" {
			scheme = "http"
		}
		client := &http.Client{Timeout: timeout}
		if scheme == "https" {
			// Probe semantics per the API: certificate verification is skipped.
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
		}
		url := fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, req.Port, req.Path)
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			ok = resp.StatusCode >= 200 && resp.StatusCode < 400
		}
	}
	code := 1
	if ok {
		code = 0
	}
	fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":%d}`, code)))
}

func handleShutdown(fc *framedConn, req request, sup *supervisor) {
	grace := req.Grace
	if grace <= 0 {
		grace = 30
	}
	log.Printf("shutdown requested (grace %ds)", grace)
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
	sup.shutdownAll(grace)
}

func handleAttach(fc *framedConn, req request, sup *supervisor) {
	c := sup.byName(req.Container)
	if c == nil {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"no such container"}`))
		return
	}
	c.mu.Lock()
	if c.attached != nil {
		c.mu.Unlock()
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"already attached"}`))
		return
	}
	c.attached = fc
	stdin := c.stdinW
	ptyF := c.ptyFile
	c.mu.Unlock()
	defer c.setAttached(nil)

	for {
		typ, payload, err := fc.readFrame()
		if err != nil {
			return
		}
		switch typ {
		case frameStdin:
			if stdin != nil {
				stdin.Write(payload)
			}
		case frameStdinClose:
			// For an attached session, closing stdin does not kill the
			// workload; just stop forwarding.
		case frameResize:
			var sz struct{ Cols, Rows uint16 }
			if json.Unmarshal(payload, &sz) == nil && ptyF != nil {
				pty.Setsize(ptyF, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows})
			}
		}
	}
}

func handleExec(fc *framedConn, req request, sup *supervisor) {
	if len(req.Argv) == 0 {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"empty argv"}`))
		return
	}
	c := sup.byName(req.Container)
	if c == nil {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"no such container"}`))
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = mergeEnv(defaultEnv(), c.spec.Env)
	cmd.Dir = c.spec.Cwd
	if cmd.Dir == "" && c.root != "" {
		cmd.Dir = "/" // avoid inheriting execd's pre-chroot cwd
	}
	if c.root != "" {
		path, err := lookPathInRoot(c.root, cmd.Env, req.Argv[0])
		if err != nil {
			fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":126,"error":%q}`, err.Error())))
			return
		}
		cmd.Path = path
		cmd.Err = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: c.root}
	}

	var ptyF *os.File
	var stdinW io.WriteCloser
	done := make(chan struct{})

	if req.TTY {
		var err error
		ptyF, err = pty.Start(cmd)
		if err != nil {
			fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":126,"error":%q}`, err.Error())))
			return
		}
		if req.Cols > 0 {
			pty.Setsize(ptyF, &pty.Winsize{Cols: req.Cols, Rows: req.Rows})
		}
		stdinW = ptyF
		go func() {
			io.Copy(&frameWriter{fc, frameStdout}, ptyF)
			close(done)
		}()
	} else {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		stdin, _ := cmd.StdinPipe()
		stdinW = stdin
		if err := cmd.Start(); err != nil {
			fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":126,"error":%q}`, err.Error())))
			return
		}
		var n int
		waitFor := make(chan struct{}, 2)
		go func() { io.Copy(&frameWriter{fc, frameStdout}, stdout); waitFor <- struct{}{} }()
		go func() { io.Copy(&frameWriter{fc, frameStderr}, stderr); waitFor <- struct{}{} }()
		go func() {
			for n = 0; n < 2; n++ {
				<-waitFor
			}
			close(done)
		}()
	}

	go func() {
		for {
			typ, payload, err := fc.readFrame()
			if err != nil {
				return
			}
			switch typ {
			case frameStdin:
				stdinW.Write(payload)
			case frameStdinClose:
				stdinW.Close()
			case frameResize:
				var sz struct{ Cols, Rows uint16 }
				if json.Unmarshal(payload, &sz) == nil && ptyF != nil {
					pty.Setsize(ptyF, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows})
				}
			}
		}
	}()

	<-done
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = 1
	}
	fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":%d}`, code)))
}
