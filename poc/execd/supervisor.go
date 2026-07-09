package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Multi-container supervision: the pod is one VM, each container is a
// supervised process with its own image root (EROFS lower + tmpfs upper
// overlay, chroot), env, cwd, and log stream. Containers share the kernel —
// so localhost, IPC, and /dev/shm behave like a Kubernetes pod by
// construction. execd restarts containers individually per restartPolicy
// (a crashing sidecar must not restart its siblings), and the VM exits only
// when the pod is done: every container terminated with no restart due.
//
// Per-container logs are written to /logs/<name>.log in the boot virtiofs
// share, so the host reads them directly (kubectl logs -c). The merged
// stream still goes to the console (container.log) for debugging.

type container struct {
	spec containerSpec
	root string // chroot path; "" in legacy dir mode

	mu       sync.Mutex
	cmd      *exec.Cmd
	ptyFile  *os.File
	stdinW   io.WriteCloser
	attached *framedConn
	logF     *os.File

	state      string // Waiting | Running | Terminated
	exitCode   int
	restarts   int
	startedAt  time.Time
	finishedAt time.Time
	reason     string
}

type containerStatus struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	ExitCode   int    `json:"exitCode"`
	Restarts   int    `json:"restarts"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Init       bool   `json:"init,omitempty"`
}

func (c *container) status(init bool) containerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := containerStatus{
		Name: c.spec.Name, State: c.state, ExitCode: c.exitCode,
		Restarts: c.restarts, Reason: c.reason, Init: init,
	}
	if !c.startedAt.IsZero() {
		s.StartedAt = c.startedAt.Unix()
	}
	if !c.finishedAt.IsZero() {
		s.FinishedAt = c.finishedAt.Unix()
	}
	return s
}

func (c *container) setAttached(fc *framedConn) {
	c.mu.Lock()
	c.attached = fc
	c.mu.Unlock()
}

// Write mirrors container output to its log file, the console (merged
// view), and any attached session.
func (c *container) Write(p []byte) (int, error) {
	if c.logF != nil {
		c.logF.Write(p)
	}
	os.Stdout.Write(p)
	c.mu.Lock()
	fc := c.attached
	c.mu.Unlock()
	if fc != nil {
		fc.writeFrame(frameStdout, p)
	}
	return len(p), nil
}

// startOnce launches the container process and waits for it to exit.
func (c *container) startOnce() (int, error) {
	cs := c.spec
	cmd := exec.Command(cs.Argv[0], cs.Argv[1:]...)
	cmd.Env = mergeEnv(defaultEnv(), cs.Env)
	if c.root != "" {
		path, err := lookPathInRoot(c.root, cmd.Env, cs.Argv[0])
		if err != nil {
			return 128, err
		}
		cmd.Path = path
		cmd.Err = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: c.root}
	}
	if cs.Cwd != "" {
		os.MkdirAll(c.root+cs.Cwd, 0o755) // images may declare a WORKDIR the tar lacks
		cmd.Dir = cs.Cwd
	} else if c.root != "" {
		// Without an explicit chdir the child keeps execd's pre-chroot cwd,
		// which doesn't exist inside the chroot (getcwd() fails).
		cmd.Dir = "/"
	}

	// The output copiers must finish before cmd.Wait(): Wait closes the
	// pipes on process exit, and a fast-exiting workload can be gone before
	// the copy goroutines are ever scheduled — losing all output.
	var copies sync.WaitGroup
	if cs.TTY {
		f, err := pty.Start(cmd)
		if err != nil {
			return 128, err
		}
		c.mu.Lock()
		c.cmd, c.ptyFile, c.stdinW = cmd, f, f
		c.mu.Unlock()
		copies.Add(1)
		go func() { defer copies.Done(); io.Copy(c, f) }()
	} else {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		stdin, _ := cmd.StdinPipe()
		if err := cmd.Start(); err != nil {
			return 128, err
		}
		c.mu.Lock()
		c.cmd, c.stdinW = cmd, stdin
		c.mu.Unlock()
		copies.Add(2)
		go func() { defer copies.Done(); io.Copy(c, stdout) }()
		go func() { defer copies.Done(); io.Copy(c, stderr) }()
	}
	c.mu.Lock()
	c.state = "Running"
	c.startedAt = time.Now()
	c.mu.Unlock()

	copies.Wait()
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = 1
	}
	return code, nil
}

// terminate delivers SIGTERM to the container's current process, escalating
// to SIGKILL after the grace period if that same incarnation still runs.
func (c *container) terminate(grace int) {
	c.mu.Lock()
	cmd := c.cmd
	running := c.state == "Running"
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil || !running {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	time.AfterFunc(time.Duration(grace)*time.Second, func() {
		c.mu.Lock()
		same := c.cmd == cmd && c.state == "Running"
		c.mu.Unlock()
		if same {
			cmd.Process.Kill()
		}
	})
}

// shouldRestart applies restartPolicy.
func shouldRestart(policy string, exitCode int) bool {
	switch policy {
	case "Never":
		return false
	case "OnFailure":
		return exitCode != 0
	default: // Always
		return true
	}
}

// supervise runs a container until it is terminally done per restartPolicy.
// Restart divergence, stated: the writable overlay upper is NOT wiped on
// in-guest restarts (kubelet gives a fresh layer; ours persists until the
// VM itself restarts).
func (c *container) supervise(policy string, stop func() bool) {
	for attempt := 0; ; attempt++ {
		code, err := c.startOnce()
		now := time.Now()
		c.mu.Lock()
		ran := now.Sub(c.startedAt)
		c.exitCode = code
		c.finishedAt = now
		if err != nil {
			c.reason = err.Error()
			log.Printf("container %s: start failed: %v", c.spec.Name, err)
		} else {
			c.reason = ""
		}
		c.mu.Unlock()
		log.Printf("container %s: exited (code %d)", c.spec.Name, code)

		if stop() || !shouldRestart(policy, code) {
			c.mu.Lock()
			c.state = "Terminated"
			c.mu.Unlock()
			return
		}
		if ran > 5*time.Minute {
			attempt = 0
		}
		backoff := time.Duration(1<<min(attempt, 5)) * time.Second
		c.mu.Lock()
		c.state = "Waiting"
		c.reason = fmt.Sprintf("CrashLoopBackOff: restarting in %s", backoff)
		c.restarts++
		c.mu.Unlock()
		time.Sleep(backoff)
		if stop() {
			c.mu.Lock()
			c.state = "Terminated"
			c.mu.Unlock()
			return
		}
	}
}

type supervisor struct {
	mu       sync.Mutex
	policy   string
	inits    []*container
	mains    []*container
	stopping bool
}

func (s *supervisor) byName(name string) *container {
	if name == "" && len(s.mains) > 0 {
		return s.mains[0]
	}
	for _, c := range append(append([]*container{}, s.mains...), s.inits...) {
		if c.spec.Name == name {
			return c
		}
	}
	return nil
}

func (s *supervisor) statuses() []containerStatus {
	var out []containerStatus
	for _, c := range s.inits {
		out = append(out, c.status(true))
	}
	for _, c := range s.mains {
		out = append(out, c.status(false))
	}
	return out
}

// run executes init containers sequentially, then supervises the main
// containers until the pod completes. Returns the pod exit code.
func (s *supervisor) run() int {
	stop := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopping
	}
	for _, c := range s.inits {
		log.Printf("init container %s: starting", c.spec.Name)
		c.supervise(initPolicy(s.policy), stop)
		c.mu.Lock()
		code := c.exitCode
		c.mu.Unlock()
		if code != 0 {
			log.Printf("init container %s: failed (code %d); pod fails", c.spec.Name, code)
			return code
		}
	}

	var wg sync.WaitGroup
	for _, c := range s.mains {
		wg.Add(1)
		go func(c *container) {
			defer wg.Done()
			log.Printf("container %s: starting", c.spec.Name)
			c.supervise(s.policy, stop)
		}(c)
	}
	wg.Wait()

	code := 0
	for _, c := range s.mains {
		c.mu.Lock()
		if c.exitCode != 0 {
			code = c.exitCode
		}
		c.mu.Unlock()
	}
	return code
}

// initPolicy: init containers with pod policy Always retry OnFailure
// (kubelet semantics — Always would loop a succeeded init forever).
func initPolicy(pod string) string {
	if pod == "Always" {
		return "OnFailure"
	}
	return pod
}

// shutdownAll delivers SIGTERM to every running container and escalates to
// SIGKILL after the grace period.
func (s *supervisor) shutdownAll(grace int) {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	all := append(append([]*container{}, s.mains...), s.inits...)
	for _, c := range all {
		c.mu.Lock()
		if c.cmd != nil && c.cmd.Process != nil && c.state == "Running" {
			c.cmd.Process.Signal(syscall.SIGTERM)
		}
		c.mu.Unlock()
	}
	time.AfterFunc(time.Duration(grace)*time.Second, func() {
		for _, c := range all {
			c.mu.Lock()
			if c.cmd != nil && c.cmd.Process != nil && c.state == "Running" {
				c.cmd.Process.Kill()
			}
			c.mu.Unlock()
		}
	})
}

// writeFinalStatus persists the pod outcome into the boot share so the
// agent can read it after the VM is gone.
func (s *supervisor) writeFinalStatus() {
	data, err := json.Marshal(s.statuses())
	if err != nil {
		return
	}
	os.WriteFile("/status.json", data, 0o644)
}
