#!/usr/bin/env python3
"""Pod launch benchmark: create N pods (prepulled image), time to all Ready."""
import json, subprocess, sys, time, statistics

KUBECONFIG = "/Users/justinsb/projects/kube-on-macos/poc/etc/kubernetes/admin.kubeconfig"

def kubectl(*args, input=None, check=True):
    return subprocess.run(["kubectl", "--kubeconfig", KUBECONFIG, *args],
                          input=input, capture_output=True, text=True, check=check)

def pod_yaml(i):
    return f"""apiVersion: v1
kind: Pod
metadata:
  name: bench-{i}
  labels: {{bench: "true"}}
spec:
  containers:
  - name: main
    image: busybox:1.28
    command: ["sleep", "3600"]
    resources:
      requests: {{memory: 32Mi, cpu: 10m}}
      limits: {{memory: 128Mi}}
---
"""

def ready_count():
    out = kubectl("get", "pods", "-l", "bench=true", "-o", "json", check=False).stdout
    if not out:
        return 0, 0
    pods = json.loads(out)["items"]
    ready = 0
    for p in pods:
        for c in p.get("status", {}).get("conditions", []):
            if c["type"] == "Ready" and c["status"] == "True":
                ready += 1
    return ready, len(pods)

def bench(n, timeout=900):
    manifest = "".join(pod_yaml(i) for i in range(n))
    t0 = time.time()
    kubectl("apply", "-f", "-", input=manifest)
    t_apply = time.time() - t0
    while True:
        r, total = ready_count()
        if r >= n:
            break
        if time.time() - t0 > timeout:
            print(f"TIMEOUT: {r}/{n} ready after {timeout}s", flush=True)
            break
        time.sleep(0.2)
    t_all = time.time() - t0

    # Per-pod: creationTimestamp -> containerStatuses[0].state.running.startedAt
    # (1s granularity: both are RFC3339 seconds).
    out = kubectl("get", "pods", "-l", "bench=true", "-o", "json").stdout
    lat = []
    for p in json.loads(out)["items"]:
        try:
            import datetime as dt
            created = dt.datetime.fromisoformat(p["metadata"]["creationTimestamp"].replace("Z", "+00:00"))
            started = dt.datetime.fromisoformat(
                p["status"]["containerStatuses"][0]["state"]["running"]["startedAt"].replace("Z", "+00:00"))
            lat.append((started - created).total_seconds())
        except (KeyError, IndexError):
            pass
    print(f"N={n}: apply={t_apply:.1f}s  all-ready={t_all:.1f}s  "
          f"throughput={n/t_all:.1f} pods/s", flush=True)
    if lat:
        lat.sort()
        print(f"      per-pod create->running: min={lat[0]:.0f}s "
              f"median={statistics.median(lat):.0f}s p90={lat[int(len(lat)*0.9)-1 if n>1 else 0]:.0f}s max={lat[-1]:.0f}s", flush=True)
    return t_all

def cleanup():
    kubectl("delete", "pods", "-l", "bench=true", "--wait=false", check=False)
    t0 = time.time()
    while time.time() - t0 < 300:
        _, total = ready_count()
        if total == 0:
            print(f"cleanup: {time.time()-t0:.1f}s", flush=True)
            return
        time.sleep(1)
    print("cleanup TIMEOUT", flush=True)

if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "clean":
        cleanup()
    else:
        bench(int(mode))
