package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ensureImage pulls (once) and flattens an OCI image for linux/arm64 into a
// cached rootfs directory, returning its path. The image config is saved
// alongside as <dir>.config.json so pods without an explicit command can use
// the image's Entrypoint/Cmd.
func (a *agent) ensureImage(image string) (string, error) {
	sum := sha256.Sum256([]byte(image))
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.ToLower(image))
	dir := filepath.Join(a.imagesDir, hex.EncodeToString(sum[:8])+"-"+safe)

	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}

	log.Printf("pulling image %s (linux/arm64)...", image)
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", fmt.Errorf("parsing image reference: %w", err)
	}
	img, err := remote.Image(ref, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64"}))
	if err != nil {
		return "", fmt.Errorf("fetching image: %w", err)
	}
	// Pulling by digest (or a single-arch tag) bypasses platform selection
	// and silently yields whatever the manifest is; an amd64 rootfs can't run
	// in these VMs, so fail with the real reason instead of exec/extract
	// weirdness later.
	if cf, err := img.ConfigFile(); err == nil {
		if cf.Architecture != "arm64" || cf.OS != "linux" {
			return "", fmt.Errorf("image is %s/%s; this node runs only linux/arm64 microVMs (no emulation)", cf.OS, cf.Architecture)
		}
	}

	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := untar(mutate.Extract(img), tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("extracting image: %w", err)
	}
	// World-writable sticky temp dirs, whether or not the image's tar
	// carried explicit entries for them.
	for _, d := range []string{"tmp", "var/tmp"} {
		p := filepath.Join(tmp, d)
		os.MkdirAll(p, 0o755)
		os.Chmod(p, 0o777|os.ModeSticky)
	}

	if cf, err := img.ConfigFile(); err == nil {
		if data, err := json.Marshal(cf); err == nil {
			os.WriteFile(dir+".config.json", data, 0o644)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	log.Printf("image %s ready at %s", image, dir)
	return dir, nil
}

// imageArgv returns Entrypoint+Cmd from the cached image config.
func imageArgv(rootfsBase string) []string {
	data, err := os.ReadFile(rootfsBase + ".config.json")
	if err != nil {
		return nil
	}
	var cf v1.ConfigFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil
	}
	return append(append([]string{}, cf.Config.Entrypoint...), cf.Config.Cmd...)
}

// imageEnv returns the image config's Env from the cached image config.
func imageEnv(rootfsBase string) []string {
	data, err := os.ReadFile(rootfsBase + ".config.json")
	if err != nil {
		return nil
	}
	var cf v1.ConfigFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil
	}
	return cf.Config.Env
}

// imageWorkingDir returns the image config's WorkingDir.
func imageWorkingDir(rootfsBase string) string {
	data, err := os.ReadFile(rootfsBase + ".config.json")
	if err != nil {
		return ""
	}
	var cf v1.ConfigFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return ""
	}
	return cf.Config.WorkingDir
}

// untar extracts a flattened image tar (whiteouts already applied by
// mutate.Extract). Runs unprivileged: ownership is not preserved, device
// nodes are skipped.
func untar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			continue
		}
		// FileInfo().Mode() maps tar's setuid/setgid/sticky bits onto
		// os.FileMode flags; raw hdr.Mode would silently drop them (e.g.
		// /tmp ending up 0755 instead of 1777, breaking apt's _apt user).
		mode := hdr.FileInfo().Mode()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			os.Chmod(target, mode|0o700) // ensure we can keep writing into it
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src := filepath.Join(dest, filepath.Clean("/"+hdr.Linkname))
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return err
			}
		default:
			// char/block/fifo etc: skip (unprivileged extract; the guest
			// gets /dev from its own kernel).
		}
	}
}
