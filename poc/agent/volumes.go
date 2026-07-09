package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigMap and Secret volumes are materialized to a host directory under
// the pod state dir at (re)start, then shared into the guest like any other
// volume (read-only, matching kubelet). Divergences from kubelet, stated:
// no live updates after materialization (kubelet's atomic-symlink refresh —
// a restart re-materializes), and secrets land on the host filesystem
// rather than tmpfs.

// materializeConfigMap writes a ConfigMap's data as files into dir.
func (a *agent) materializeConfigMap(ctx context.Context, ns string, src *corev1.ConfigMapVolumeSource, dir string) error {
	client := a.cs()
	if client == nil {
		return fmt.Errorf("configMap %q: apiserver not available yet", src.Name)
	}
	cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, src.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && src.Optional != nil && *src.Optional {
			return freshDir(dir) // optional + missing = empty volume
		}
		return fmt.Errorf("configMap %q: %w", src.Name, err)
	}
	data := map[string][]byte{}
	for k, v := range cm.Data {
		data[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		data[k] = v
	}
	return writeVolumeKeys(dir, data, src.Items, src.DefaultMode)
}

// materializeSecret writes a Secret's data as files into dir.
func (a *agent) materializeSecret(ctx context.Context, ns string, src *corev1.SecretVolumeSource, dir string) error {
	client := a.cs()
	if client == nil {
		return fmt.Errorf("secret %q: apiserver not available yet", src.SecretName)
	}
	sec, err := client.CoreV1().Secrets(ns).Get(ctx, src.SecretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && src.Optional != nil && *src.Optional {
			return freshDir(dir)
		}
		return fmt.Errorf("secret %q: %w", src.SecretName, err)
	}
	return writeVolumeKeys(dir, sec.Data, src.Items, src.DefaultMode)
}

// writeVolumeKeys materializes data keys as files, honoring the optional
// items projection (key -> relative path, per-item mode) and defaultMode.
// The dir is rebuilt from scratch: each pod (re)start sees current content.
func writeVolumeKeys(dir string, data map[string][]byte, items []corev1.KeyToPath, defaultMode *int32) error {
	if err := freshDir(dir); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if defaultMode != nil {
		mode = os.FileMode(*defaultMode)
	}
	write := func(relPath string, content []byte, m os.FileMode) error {
		target := filepath.Join(dir, filepath.Clean("/"+relPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, m)
	}
	if len(items) == 0 {
		for k, v := range data {
			if err := write(k, v, mode); err != nil {
				return err
			}
		}
		return nil
	}
	for _, item := range items {
		v, ok := data[item.Key]
		if !ok {
			return fmt.Errorf("key %q not found", item.Key)
		}
		m := mode
		if item.Mode != nil {
			m = os.FileMode(*item.Mode)
		}
		if err := write(item.Path, v, m); err != nil {
			return err
		}
	}
	return nil
}

func freshDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}
