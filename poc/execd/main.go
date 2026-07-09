// execd is the in-guest supervisor + exec daemon for the kube-on-macos PoC.
//
// It runs as the process libkrun's init execs, so the VM lives exactly as
// long as execd. It:
//
//   - reads /.podvm-spec.json (containers, network, volumes),
//   - assembles each container's root (EROFS lower + tmpfs upper overlay)
//     and supervises every container independently — restarts per
//     restartPolicy happen in-guest; the VM exits only when the pod is done,
//   - writes per-container logs to /logs/<name>.log in the boot virtiofs
//     share (host-visible) and a merged stream to the console,
//   - listens on vsock port 1024 for exec/attach/probe/status/shutdown,
//   - exits with the pod's aggregate exit code, writing /status.json first.
//
// Wire protocol (both directions, after the JSON request frame):
//
//	frame := type(1 byte) | length(4 bytes BE) | payload
//	host→guest: 0=stdin, 4=resize JSON {"cols":c,"rows":r}, 5=stdin close
//	guest→host: 1=stdout, 2=stderr, 3=exit JSON {"code":n}
//
// The first host→guest frame is type 6: a JSON request, e.g.
// {"op":"exec","container":"web","argv":[...],"tty":bool} or {"op":"status"}.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
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
	Op        string   `json:"op"`
	Container string   `json:"container,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	TTY       bool     `json:"tty,omitempty"`
	Cols      uint16   `json:"cols,omitempty"`
	Rows      uint16   `json:"rows,omitempty"`

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
	RestartPolicy  string          `json:"restartPolicy,omitempty"` // Always|OnFailure|Never
	InitContainers []containerSpec `json:"initContainers,omitempty"`
	Containers     []containerSpec `json:"containers"`
	Net            *netSpec        `json:"net,omitempty"`
	Net6           *net6Spec       `json:"net6,omitempty"`
	Svc            *svcSpec        `json:"svc,omitempty"`
}

type containerSpec struct {
	Name    string      `json:"name"`
	Argv    []string    `json:"argv"`
	Env     []string    `json:"env,omitempty"` // image config env + pod spec env
	Cwd     string      `json:"cwd,omitempty"`
	TTY     bool        `json:"tty,omitempty"`
	RootDev string      `json:"rootDev,omitempty"` // /dev/vdX; "" = legacy boot root
	Mounts  []mountSpec `json:"mounts,omitempty"`  // this container's volume mounts
}

type mountSpec struct {
	Tag       string `json:"tag"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type svcSpec struct {
	CIDR      string `json:"cidr"`
	Namespace string `json:"ns,omitempty"`
}

type net6Spec struct {
	IP string `json:"ip"`
}

type netSpec struct {
	IP  string `json:"ip"`
	GW  string `json:"gw"`
	DNS string `json:"dns"`
}

func defaultEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=xterm",
	}
}

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

// mountVolumes attaches a container's volumes (extra virtio-fs devices) at
// their mountPaths inside the container root. Failures are fatal: a
// workload that expects a volume must not run without it.
func mountVolumes(root string, mounts []mountSpec) {
	for _, m := range mounts {
		target := root + m.MountPath
		if err := os.MkdirAll(target, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "execd: volume %s: mkdir %s: %v\n", m.Tag, target, err)
			log.Fatalf("volume %s: mkdir %s: %v", m.Tag, target, err)
		}
		var flags uintptr
		if m.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(m.Tag, target, "virtiofs", flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "execd: volume %s: mount %s: %v\n", m.Tag, target, err)
			log.Fatalf("volume %s: mount %s: %v", m.Tag, target, err)
		}
		log.Printf("volume %s mounted at %s (ro=%v)", m.Tag, target, m.ReadOnly)
	}
}

func main() {
	log.SetPrefix("execd: ")
	log.SetFlags(log.LstdFlags)
	// Keep execd's own chatter out of the console (which feeds
	// container.log); the boot rootfs is a virtiofs share, so this file is
	// visible host-side at pods/<uid>/rootfs/.execd.log.
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
	if len(sp.Containers) == 0 {
		log.Fatalf("no containers in spec")
	}
	if sp.RestartPolicy == "" {
		sp.RestartPolicy = "Always"
	}

	// Container roots first: everything else (resolv.conf, volume mounts)
	// writes into them.
	os.MkdirAll("/logs", 0o755)
	sup := &supervisor{policy: sp.RestartPolicy}
	build := func(cs containerSpec) *container {
		c := &container{spec: cs, state: "Waiting"}
		if cs.RootDev != "" {
			root, err := buildContainerRoot(cs.Name, cs.RootDev)
			if err != nil {
				fmt.Fprintf(os.Stderr, "execd: container %s: %v\n", cs.Name, err)
				log.Fatalf("container %s root: %v", cs.Name, err)
			}
			c.root = root
		}
		if f, err := os.OpenFile("/logs/"+cs.Name+".log",
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			c.logF = f
		}
		return c
	}
	for _, cs := range sp.InitContainers {
		sup.inits = append(sup.inits, build(cs))
	}
	for _, cs := range sp.Containers {
		sup.mains = append(sup.mains, build(cs))
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
		// Synchronous on purpose: rules must exist before any container's
		// first connect, or early flows are never DNAT'd (see services.go).
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("service LB panicked: %v (services unavailable)", r)
				}
			}()
			if err := setupServiceLB(sp.Svc.CIDR); err != nil {
				log.Printf("service LB setup failed: %v (services unavailable)", err)
			}
		}()
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
			writeEtc("resolv.conf",
				[]byte("nameserver 127.0.0.1\nsearch "+search+"\noptions ndots:5\n"))
			log.Printf("cluster dns up: 127.0.0.1:53 (search %s, upstream %s)", search, upstream)
		}
	} else if sp.Net != nil && sp.Net.DNS != "" {
		writeEtc("resolv.conf", []byte("nameserver "+sp.Net.DNS+"\n"))
	}

	for _, c := range append(append([]*container{}, sup.inits...), sup.mains...) {
		mountVolumes(c.root, c.spec.Mounts)
	}

	go serveVsock(sup)

	code := sup.run()
	sup.writeFinalStatus()
	log.Printf("pod done (exit %d)", code)
	os.Exit(code)
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
