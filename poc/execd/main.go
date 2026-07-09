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
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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

	// probe
	Ptype   string `json:"ptype,omitempty"` // "tcp" or "http"
	Port    int    `json:"port,omitempty"`
	Path    string `json:"path,omitempty"`
	Scheme  string `json:"scheme,omitempty"`
	Timeout int    `json:"timeout,omitempty"` // seconds

	// shutdown
	Grace int `json:"grace,omitempty"` // seconds between SIGTERM and SIGKILL
}

type spec struct {
	Argv   []string    `json:"argv"`
	Env    []string    `json:"env,omitempty"` // image config env + pod spec env
	Cwd    string      `json:"cwd,omitempty"`
	TTY    bool        `json:"tty"`
	Net    *netSpec    `json:"net,omitempty"`
	Net6   *net6Spec   `json:"net6,omitempty"`
	Svc    *svcSpec    `json:"svc,omitempty"`
	Mounts []mountSpec `json:"mounts,omitempty"` // pod volumes (virtio-fs tags)
}

type mountSpec struct {
	Tag       string `json:"tag"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// mountVolumes attaches the pod's volumes (extra virtio-fs devices added by
// the harness) at their declared mountPaths. Failures are fatal: a workload
// that expects a volume must not run without it (kubelet leaves such pods
// stuck in ContainerCreating; our equivalent is the VM exiting).
func mountVolumes(mounts []mountSpec) {
	for _, m := range mounts {
		if err := os.MkdirAll(m.MountPath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "execd: volume %s: mkdir %s: %v\n", m.Tag, m.MountPath, err)
			log.Fatalf("volume %s: mkdir %s: %v", m.Tag, m.MountPath, err)
		}
		var flags uintptr
		if m.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(m.Tag, m.MountPath, "virtiofs", flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "execd: volume %s: mount %s: %v\n", m.Tag, m.MountPath, err)
			log.Fatalf("volume %s: mount %s: %v", m.Tag, m.MountPath, err)
		}
		log.Printf("volume %s mounted at %s (ro=%v)", m.Tag, m.MountPath, m.ReadOnly)
	}
}

type svcSpec struct {
	CIDR      string `json:"cidr"`         // ClusterIP range, e.g. fd42:6b75:6265:1::/112
	Namespace string `json:"ns,omitempty"` // pod namespace (DNS search path)
}

type net6Spec struct {
	IP string `json:"ip"` // CIDR, e.g. fd42:6b75:6265::42/64 (on eth1)
}

// configureNet6 puts the pod's routed IPv6 address on eth1 (the vmnet
// interface). The /64 is on-link; no routes needed for v1.
func configureNet6(ns *net6Spec) error {
	eth1, err := netlink.LinkByName("eth1")
	if err != nil {
		return fmt.Errorf("no eth1: %w", err)
	}
	addr, err := netlink.ParseAddr(ns.IP)
	if err != nil {
		return fmt.Errorf("parsing ip %q: %w", ns.IP, err)
	}
	// NODAD: a tentative (DAD-in-progress) address may not be used as a
	// source, so for the pod's first ~2s the kernel picked ::1 for outbound
	// flows — poisoning any TCP connection made at boot for its entire
	// life (retransmits keep the source). The ULA is derived from the pod
	// UID on a private bridge; duplicate detection buys nothing.
	addr.Flags = unix.IFA_F_NODAD
	if err := netlink.AddrAdd(eth1, addr); err != nil {
		return fmt.Errorf("adding address: %w", err)
	}
	if err := netlink.LinkSetUp(eth1); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	return nil
}

type netSpec struct {
	IP  string `json:"ip"`  // CIDR, e.g. 192.168.127.2/24
	GW  string `json:"gw"`  // e.g. 192.168.127.1
	DNS string `json:"dns"` // e.g. 192.168.127.1
}

// configureNet brings up lo and eth0 with a static address via netlink —
// images can't be assumed to ship iproute2, so we do it ourselves.
func configureNet(ns *netSpec) error {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		netlink.LinkSetUp(lo)
	}
	eth0, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("no eth0: %w", err)
	}
	addr, err := netlink.ParseAddr(ns.IP)
	if err != nil {
		return fmt.Errorf("parsing ip %q: %w", ns.IP, err)
	}
	if err := netlink.AddrAdd(eth0, addr); err != nil {
		return fmt.Errorf("adding address: %w", err)
	}
	if err := netlink.LinkSetUp(eth0); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	gw := net.ParseIP(ns.GW)
	if gw == nil {
		return fmt.Errorf("bad gateway %q", ns.GW)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: eth0.Attrs().Index,
		Gw:        gw,
	}); err != nil {
		return fmt.Errorf("adding default route: %w", err)
	}
	if ns.DNS != "" {
		os.WriteFile("/etc/resolv.conf",
			[]byte("nameserver "+ns.DNS+"\n"), 0o644)
	}
	return nil
}

func defaultEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=xterm",
	}
}

// workloadEnv is the pod's environment: defaults overridden by the spec env
// (image config env + pod spec env, merged host-side). Exec sessions get the
// same view, matching kubelet. workloadCwd likewise.
var (
	workloadEnv = defaultEnv
	workloadCwd string
)

func mergeEnv(base, override []string) []string {
	idx := map[string]int{}
	var out []string
	add := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			return
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		add(kv)
	}
	for _, kv := range override {
		add(kv)
	}
	return out
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
	cmd      *exec.Cmd
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
	log.SetFlags(log.LstdFlags)
	// Keep execd's own chatter out of the workload's stdout/stderr (the
	// virtio console feeds container.log). The rootfs is a virtiofs share,
	// so this file is visible host-side at pods/<uid>/rootfs/.execd.log.
	if f, err := os.OpenFile("/.execd.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(f)
	}

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

	if sp.Net != nil {
		if err := configureNet(sp.Net); err != nil {
			log.Printf("configuring network: %v (continuing without)", err)
		} else {
			log.Printf("network up: %s via %s", sp.Net.IP, sp.Net.GW)
		}
	}
	if sp.Net6 != nil {
		if err := configureNet6(sp.Net6); err != nil {
			log.Printf("configuring ipv6: %v (continuing without)", err)
		} else {
			log.Printf("ipv6 up: %s on eth1", sp.Net6.IP)
		}
	}
	if sp.Svc != nil {
		// Synchronous on purpose: rules must exist before the workload's
		// first connect, or early flows are never DNAT'd (see services.go).
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("service LB panicked: %v (services unavailable, workload unaffected)", r)
				}
			}()
			if err := setupServiceLB(sp.Svc.CIDR); err != nil {
				log.Printf("service LB setup failed: %v (services unavailable)", err)
			}
		}()
		// Cluster DNS rides the same lazy channel. Bind before the workload
		// starts so names resolve from its very first syscall; on failure the
		// gvproxy resolv.conf from configureNet stays (external DNS only).
		upstream := ""
		if sp.Net != nil && sp.Net.DNS != "" {
			upstream = net.JoinHostPort(sp.Net.DNS, "53")
		}
		if err := startDNSProxy(upstream); err != nil {
			log.Printf("dns proxy failed: %v (cluster DNS unavailable)", err)
		} else {
			ns := sp.Svc.Namespace
			if ns == "" {
				ns = "default"
			}
			search := fmt.Sprintf("%s.svc.cluster.local svc.cluster.local cluster.local", ns)
			os.WriteFile("/etc/resolv.conf",
				[]byte("nameserver 127.0.0.1\nsearch "+search+"\noptions ndots:5\n"), 0o644)
			log.Printf("cluster dns up: 127.0.0.1:53 (search %s, upstream %s)", search, upstream)
		}
	}
	if len(sp.Env) > 0 {
		merged := mergeEnv(defaultEnv(), sp.Env)
		workloadEnv = func() []string { return merged }
	}
	mountVolumes(sp.Mounts)

	wl := &workload{}
	cmd := exec.Command(sp.Argv[0], sp.Argv[1:]...)
	cmd.Env = workloadEnv()
	if sp.Cwd != "" {
		os.MkdirAll(sp.Cwd, 0o755) // images may declare a WORKDIR the tar lacks
		cmd.Dir = sp.Cwd
		workloadCwd = sp.Cwd
	}
	wl.cmd = cmd

	// The output copiers must finish before cmd.Wait(): Wait closes the
	// pipes on process exit, and a fast-exiting workload can be gone before
	// the copy goroutines are ever scheduled — silently losing all output.
	// (Observed in practice once extra virtiofs devices shifted scheduling.)
	var copies sync.WaitGroup
	if sp.TTY {
		f, err := pty.Start(cmd)
		if err != nil {
			log.Fatalf("starting workload on pty: %v", err)
		}
		wl.ptyFile = f
		wl.stdinW = f
		copies.Add(1)
		go func() { defer copies.Done(); io.Copy(wl, f) }()
	} else {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		stdin, _ := cmd.StdinPipe()
		wl.stdinW = stdin
		if err := cmd.Start(); err != nil {
			log.Fatalf("starting workload: %v", err)
		}
		copies.Add(2)
		go func() { defer copies.Done(); io.Copy(wl, stdout) }()
		go func() { defer copies.Done(); io.Copy(os.Stderr, stderr) }()
	}

	go serveVsock(wl)

	copies.Wait()
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
	case "probe":
		handleProbe(fc, req)
	case "shutdown":
		handleShutdown(fc, req, wl)
	}
}

// handleProbe implements tcpSocket/httpGet probes from inside the pod's
// network view — the equivalent of kubelet probing the pod IP.
func handleProbe(fc *framedConn, req request) {
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}
	addr := fmt.Sprintf("127.0.0.1:%d", req.Port)
	fail := func(msg string) {
		fc.writeFrame(frameExit, []byte(fmt.Sprintf(`{"code":1,"error":%q}`, msg)))
	}
	switch req.Ptype {
	case "tcp":
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			fail(err.Error())
			return
		}
		c.Close()
	case "http":
		scheme := req.Scheme
		if scheme == "" {
			scheme = "http"
		}
		path := req.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		client := &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		resp, err := client.Get(fmt.Sprintf("%s://%s%s", scheme, addr, path))
		if err != nil {
			fail(err.Error())
			return
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			fail(fmt.Sprintf("HTTP probe returned status %d", resp.StatusCode))
			return
		}
	default:
		fail("unknown probe type " + req.Ptype)
		return
	}
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
}

// handleShutdown delivers SIGTERM to the workload and escalates to SIGKILL
// after the grace period. The VM exits when the workload does.
func handleShutdown(fc *framedConn, req request, wl *workload) {
	wl.mu.Lock()
	cmd := wl.cmd
	wl.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		fc.writeFrame(frameExit, []byte(`{"code":1,"error":"no workload"}`))
		return
	}
	grace := time.Duration(req.Grace) * time.Second
	log.Printf("shutdown requested (grace %s)", grace)
	cmd.Process.Signal(syscall.SIGTERM)
	time.AfterFunc(grace, func() {
		cmd.Process.Kill()
	})
	fc.writeFrame(frameExit, []byte(`{"code":0}`))
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
	cmd.Env = workloadEnv()
	cmd.Dir = workloadCwd

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
