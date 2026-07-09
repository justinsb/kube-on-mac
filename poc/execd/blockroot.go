package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Block-image container roots: each container's image arrives as its own
// read-only virtio-blk EROFS (containers sharing an image share the disk).
// Per container, execd assembles
//
//	/dev/vdX (erofs, ro)      ->  /lower-vdX      (shared per image)
//	tmpfs                     ->  /rw-<name>      (upper+work)
//	overlay(lower,upper)      ->  /roots/<name>   (the chroot)
//
// plus the kernel filesystems the workload expects (proc, sysfs, devtmpfs,
// devpts) and a pod-shared /dev/shm — one tmpfs bind-mounted into every
// container, because containers in a pod share IPC.

var (
	mountedLowers = map[string]string{} // device -> lower dir
	builtRoots    []string             // every container root (for /etc writes)
	podShmReady   bool
)

// ensureDevMounted mounts an image device read-only, once.
func ensureDevMounted(dev string) (string, error) {
	if p, ok := mountedLowers[dev]; ok {
		return p, nil
	}
	if len(mountedLowers) == 0 {
		// libkrun's init normally provides /dev; make sure device nodes are
		// visible (EBUSY = already mounted, fine).
		if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil && err != syscall.EBUSY {
			log.Printf("mounting /dev: %v (continuing)", err)
		}
	}
	lower := "/lower-" + filepath.Base(dev)
	if err := os.MkdirAll(lower, 0o755); err != nil {
		return "", err
	}
	if err := syscall.Mount(dev, lower, "erofs", syscall.MS_RDONLY, ""); err != nil {
		// ext4 fallback covers images converted before the EROFS writer.
		if err2 := syscall.Mount(dev, lower, "ext4", syscall.MS_RDONLY, ""); err2 != nil {
			return "", fmt.Errorf("mounting image %s: erofs: %v; ext4: %w", dev, err, err2)
		}
	}
	mountedLowers[dev] = lower
	return lower, nil
}

// buildContainerRoot assembles one container's writable root.
func buildContainerRoot(name, dev string) (string, error) {
	lower, err := ensureDevMounted(dev)
	if err != nil {
		return "", err
	}
	rw := "/rw-" + name
	root := "/roots/" + name
	for _, d := range []string{rw, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	if err := syscall.Mount("tmpfs", rw, "tmpfs", 0, ""); err != nil {
		return "", fmt.Errorf("upper tmpfs: %w", err)
	}
	for _, d := range []string{rw + "/upper", rw + "/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s/upper,workdir=%s/work", lower, rw, rw)
	if err := syscall.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return "", fmt.Errorf("overlay: %w", err)
	}

	type m struct{ src, dst, fstype, opts string }
	for _, mnt := range []m{
		{"proc", root + "/proc", "proc", ""},
		{"sysfs", root + "/sys", "sysfs", ""},
		{"devtmpfs", root + "/dev", "devtmpfs", ""},
		{"devpts", root + "/dev/pts", "devpts", "mode=620,ptmxmode=666"},
	} {
		os.MkdirAll(mnt.dst, 0o755)
		if err := syscall.Mount(mnt.src, mnt.dst, mnt.fstype, 0, mnt.opts); err != nil {
			log.Printf("mounting %s: %v (continuing)", mnt.dst, err)
		}
	}
	// Pod-shared /dev/shm: one tmpfs for the whole pod (shared IPC).
	if !podShmReady {
		os.MkdirAll("/podshm", 0o755)
		if err := syscall.Mount("tmpfs", "/podshm", "tmpfs", 0, "mode=1777"); err == nil {
			podShmReady = true
		}
	}
	if podShmReady {
		os.MkdirAll(root+"/dev/shm", 0o755)
		if err := syscall.Mount("/podshm", root+"/dev/shm", "", syscall.MS_BIND, ""); err != nil {
			log.Printf("binding /dev/shm: %v (continuing)", err)
		}
	}
	// A usable /tmp even if the image tar lacked one.
	if st, err := os.Stat(root + "/tmp"); err != nil || !st.IsDir() {
		os.MkdirAll(root+"/tmp", 0o755)
	}
	os.Chmod(root+"/tmp", 0o777|os.ModeSticky)
	os.MkdirAll(root+"/etc", 0o755)

	builtRoots = append(builtRoots, root)
	log.Printf("container root ready: %s -> %s", dev, root)
	return root, nil
}

// writeEtc writes an /etc file into every container root (and the boot
// root, covering legacy dir mode where the boot root IS the container).
func writeEtc(name string, content []byte) {
	os.WriteFile("/etc/"+name, content, 0o644)
	for _, root := range builtRoots {
		os.WriteFile(root+"/etc/"+name, content, 0o644)
	}
}

// lookPathInRoot resolves a bare command name against PATH *inside* the
// chroot (exec.Command's own LookPath would search execd's boot root, where
// the image's binaries don't exist). Returns a chroot-relative absolute path.
func lookPathInRoot(root string, env []string, name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathEnv := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathEnv = v
		}
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		resolved, err := resolveInRoot(root, candidate, 0)
		if err != nil {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, resolved)); err == nil &&
			!st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH inside image", name)
}

// resolveInRoot walks p component-wise under root, re-rooting symlink
// targets (an absolute symlink like /bin/sh -> /bin/busybox must resolve
// inside the chroot, not in execd's boot filesystem).
func resolveInRoot(root, p string, depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("too many symlinks resolving %q", p)
	}
	cur := "/"
	comps := strings.Split(strings.Trim(filepath.Clean("/"+p), "/"), "/")
	for i, comp := range comps {
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(filepath.Join(root, cur))
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filepath.Join(root, cur))
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			rest := filepath.Join(append([]string{target}, comps[i+1:]...)...)
			return resolveInRoot(root, rest, depth+1)
		}
	}
	return cur, nil
}
