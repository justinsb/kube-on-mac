// execd is the in-guest supervisor + exec daemon for the kube-on-macos PoC.
//
// It runs as the process libkrun's init execs (via /entry.sh), so the VM
// lives exactly as long as execd. It:
//
//   - reads /.podvm-spec.json (workload argv, tty flag),
//   - starts the workload — on a pty if the pod requested tty — mirroring
//     its output to execd's own stdout (the virtio console → container.log
//     on the host),
//   - listens on vsock port 1024 for exec/attach sessions from the host
//     agent,
//   - exits with the workload's exit code, which shuts down the VM.
//
// Wire protocol (both directions, after the JSON request frame):
//
//	frame := type(1 byte) | length(4 bytes BE) | payload
//	host→guest: 0=stdin, 4=resize JSON {"cols":c,"rows":r}, 5=stdin close
//	guest→host: 1=stdout, 2=stderr, 3=exit JSON {"code":n}
//
// The first host→guest frame is type 6: a JSON request, either
// {"op":"exec","argv":[...],"tty":bool} or {"op":"attach"}.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
)

const (
	frameStdin      = 0
	frameStdout     = 1
	frameStderr     = 2
	frameExit       = 3
	frameResize     = 4
	frameStdinClose = 5
	frameRequest    = 6

	vsockPort = 1024
)

type request struct {
	Op   string   `json:"op"`
	Argv []string `json:"argv,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
}

type spec struct {
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty"`
}

func defaultEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=xterm",
	}
}

// framedConn serializes frame writes (multiple copiers share one conn).
type framedConn struct {
	mu sync.Mutex
	c  io.ReadWriteCloser
}

func (f *framedConn) writeFrame(typ byte, payload []byte) error {
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

func (f *framedConn) readFrame() (byte, []byte, error) {
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

// frameWriter adapts a stream id on a framedConn to io.Writer.
type frameWriter struct {
	fc  *framedConn
	typ byte
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if err := w.fc.writeFrame(w.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// workload is the supervised main process.
type workload struct {
	mu       sync.Mutex
	ptyFile  *os.File // non-nil when running on a pty
	stdinW   io.WriteCloser
	attached *framedConn // at most one attach session
}

func (wl *workload) setAttached(fc *framedConn) {
	wl.mu.Lock()
	wl.attached = fc
	wl.mu.Unlock()
}

// Write mirrors workload output to the console and any attached session.
func (wl *workload) Write(p []byte) (int, error) {
	os.Stdout.Write(p)
	wl.mu.Lock()
	fc := wl.attached
	wl.mu.Unlock()
	if fc != nil {
		fc.writeFrame(frameStdout, p)
	}
	return len(p), nil
}

func main() {
	log.SetPrefix("execd: ")
	log.SetFlags(0)

	data, err := os.ReadFile("/.podvm-spec.json")
	if err != nil {
		log.Fatalf("reading spec: %v", err)
	}
	var sp spec
	if err := json.Unmarshal(data, &sp); err != nil {
		log.Fatalf("parsing spec: %v", err)
	}
	if len(sp.Argv) == 0 {
		log.Fatalf("empty argv in spec")
	}

	wl := &workload{}
	cmd := exec.Command(sp.Argv[0], sp.Argv[1:]...)
	cmd.Env = defaultEnv()

	if sp.TTY {
		f, err := pty.Start(cmd)
		if err != nil {
			log.Fatalf("starting workload on pty: %v", err)
		}
		wl.ptyFile = f
		wl.stdinW = f
		go io.Copy(wl, f)
	} else {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		stdin, _ := cmd.StdinPipe()
		wl.stdinW = stdin
		if err := cmd.Start(); err != nil {
			log.Fatalf("starting workload: %v", err)
		}
		go io.Copy(wl, stdout)
		go io.Copy(os.Stderr, stderr)
	}

	go serveVsock(wl)

	err = cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = 1
	}
	os.Exit(code)
}

func serveVsock(wl *workload) {
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
		go handleConn(&framedConn{c: conn}, wl)
	}
}

func handleConn(fc *framedConn, wl *workload) {
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
		handleExec(fc, req)
	case "attach":
		handleAttach(fc, wl)
	}
}

func handleAttach(fc *framedConn, wl *workload) {
	wl.setAttached(fc)
	defer wl.setAttached(nil)
	for {
		typ, payload, err := fc.readFrame()
		if err != nil {
			return
		}
		switch typ {
		case frameStdin:
			if wl.stdinW != nil {
				wl.stdinW.Write(payload)
			}
		case frameStdinClose:
			// For an attached session, closing stdin does not kill the
			// workload; just stop forwarding.
		case frameResize:
			var sz struct{ Cols, Rows uint16 }
			if json.Unmarshal(payload, &sz) == nil && wl.ptyFile != nil {
				pty.Setsize(wl.ptyFile, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows})
			}
		}
	}
}

func handleExec(fc *framedConn, req request) {
	if len(req.Argv) == 0 {
		fc.writeFrame(frameExit, []byte(`{"code":126,"error":"empty argv"}`))
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = defaultEnv()

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
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); io.Copy(&frameWriter{fc, frameStdout}, stdout) }()
		go func() { defer wg.Done(); io.Copy(&frameWriter{fc, frameStderr}, stderr) }()
		go func() { wg.Wait(); close(done) }()
	}

	// Reader loop: stdin/resize from host until the connection drops.
	connDead := make(chan struct{})
	go func() {
		defer close(connDead)
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

	err := cmd.Wait()
	<-done // drain output before reporting exit
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = 126
	}
	fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":%d}`, code)))
	if ptyF != nil {
		ptyF.Close()
	}
	_ = connDead
}
