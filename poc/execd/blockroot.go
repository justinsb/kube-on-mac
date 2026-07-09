package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Block-image roots: the container image arrives as a read-only virtio-blk
// ext4 (a real Linux filesystem — case-sensitive, faithful ownership, real
// xattrs — unlike a virtiofs view of APFS). execd assembles
//
//	/dev/vda (ext4, ro)  ->  /lower
//	tmpfs                ->  /rw   (upper+work)
//	overlay(lower,upper) ->  /newroot
//
// and chroots the workload (and exec sessions) into /newroot. The tmpfs
// upper gives every container start a fresh writable layer, which is the
// same contract the per-restart rootfs re-clone provided. execd itself
// stays in the tiny virtiofs boot root, next to its vsock/console plumbing.

// rootPrefix is "" in legacy dir mode, "/newroot" in block mode.
var rootPrefix string

func setupBlockRoot(dev string) error {
	// libkrun's init normally provides /dev; make sure it exists so the
	// block device node is visible (EBUSY = already mounted, fine).
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil && err != syscall.EBUSY {
		log.Printf("mounting /dev: %v (continuing)", err)
	}
	if err := syscall.Mount(dev, "/lower", "erofs", syscall.MS_RDONLY, ""); err != nil {
		// ext4 fallback covers any image converted before the EROFS writer.
		if err2 := syscall.Mount(dev, "/lower", "ext4", syscall.MS_RDONLY, ""); err2 != nil {
			return fmt.Errorf("mounting image %s: erofs: %v; ext4: %w", dev, err, err2)
		}
	}
	if err := syscall.Mount("tmpfs", "/rw", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mounting upper tmpfs: %w", err)
	}
	for _, d := range []string{"/rw/upper", "/rw/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=/lower,upperdir=/rw/upper,workdir=/rw/work"
	if err := syscall.Mount("overlay", "/newroot", "overlay", 0, opts); err != nil {
		return fmt.Errorf("mounting overlay: %w", err)
	}

	// The kernel-backed filesystems the workload expects inside its root.
	type m struct{ src, dst, fstype, opts string }
	for _, mnt := range []m{
		{"proc", "/newroot/proc", "proc", ""},
		{"sysfs", "/newroot/sys", "sysfs", ""},
		{"devtmpfs", "/newroot/dev", "devtmpfs", ""},
		{"devpts", "/newroot/dev/pts", "devpts", "mode=620,ptmxmode=666"},
		{"tmpfs", "/newroot/dev/shm", "tmpfs", "mode=1777"},
	} {
		os.MkdirAll(mnt.dst, 0o755)
		if err := syscall.Mount(mnt.src, mnt.dst, mnt.fstype, 0, mnt.opts); err != nil {
			log.Printf("mounting %s: %v (continuing)", mnt.dst, err)
		}
	}
	// A usable /tmp even if the image tar lacked one.
	if st, err := os.Stat("/newroot/tmp"); err != nil || !st.IsDir() {
		os.MkdirAll("/newroot/tmp", 0o755)
	}
	os.Chmod("/newroot/tmp", 0o777|os.ModeSticky)
	os.MkdirAll("/newroot/etc", 0o755)

	rootPrefix = "/newroot"
	log.Printf("block root ready: %s -> overlay at /newroot", dev)
	return nil
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

// etcPath maps a guest /etc file to the workload's view of it.
func etcPath(name string) string { return rootPrefix + "/etc/" + name }
