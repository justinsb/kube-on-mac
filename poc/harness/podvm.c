/*
 * podvm — minimal pod-VM harness for the macOS kubelet PoC.
 *
 * Boots a Linux microVM via libkrun with:
 *   - an external kernel (Kata's vmlinux.container, ARM64 raw Image format)
 *   - a directory as the root filesystem, shared via virtio-fs
 *     (optionally with a DAX window, --dax-mb)
 *   - libkrun's built-in init, which execs the requested command as PID 2
 *
 * The command's stdout/stderr flow back over virtio-console to our
 * stdout/stderr, so a supervising process (the node agent) can capture logs.
 *
 * Usage:
 *   podvm --kernel VMLINUX --rootfs DIR [--cpus N] [--mem MB] [--dax-mb MB] \
 *         -- COMMAND [ARGS...]
 */

#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <libkrun.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <unistd.h>

static void usage(const char *name)
{
    fprintf(stderr,
            "Usage: %s --kernel VMLINUX --rootfs DIR [--cpus N] [--mem MB] "
            "[--dax-mb MB] [--log FILE] [--vsock-exec SOCK] -- COMMAND "
            "[ARGS...]\n",
            name);
}

static int check(int err, const char *msg)
{
    if (err) {
        errno = -err;
        perror(msg);
        return -1;
    }
    return 0;
}

int main(int argc, char *const argv[])
{
    const char *kernel_path = NULL;
    const char *rootfs = NULL;
    const char *log_path = NULL;
    const char *vsock_exec_path = NULL;
    const char *vsock_svc_path = NULL;
    const char *net_socket_path = NULL;
    const char *net2_socket_path = NULL;
    const char *net2_mac = NULL;
    /* Image block devices: --root-image is repeatable (multi-container pods
     * have one EROFS per distinct image). Order matters: the Nth --root-image
     * becomes /dev/vd{a+N} in the guest, which the pod spec refers to. */
#define MAX_ROOT_IMAGES 26
    const char *root_images[MAX_ROOT_IMAGES];
    int n_root_images = 0;
    long cpus = 1;
    long mem_mib = 256;
    long dax_mib = 0;

    /* Extra virtio-fs shares (pod volumes): --volume TAG=PATH[:ro] */
#define MAX_VOLUMES 32
    struct { const char *tag; char *path; bool ro; } volumes[MAX_VOLUMES];
    int n_volumes = 0;

    static const struct option opts[] = {
        { "kernel", required_argument, NULL, 'k' },
        { "rootfs", required_argument, NULL, 'r' },
        { "cpus", required_argument, NULL, 'c' },
        { "mem", required_argument, NULL, 'm' },
        { "dax-mb", required_argument, NULL, 'd' },
        { "log", required_argument, NULL, 'l' },
        { "vsock-exec", required_argument, NULL, 'x' },
        { "vsock-svc", required_argument, NULL, 'S' },
        { "net-socket", required_argument, NULL, 'n' },
        { "net2-socket", required_argument, NULL, 'N' },
        { "net2-mac", required_argument, NULL, 'M' },
        { "volume", required_argument, NULL, 'v' },
        { "root-image", required_argument, NULL, 'R' },
        { "help", no_argument, NULL, 'h' },
        { NULL, 0, NULL, 0 }
    };

    int c;
    while ((c = getopt_long(argc, argv, "+k:r:c:m:d:l:x:S:n:N:M:v:R:h", opts, NULL)) != -1) {
        switch (c) {
        case 'k': kernel_path = optarg; break;
        case 'r': rootfs = optarg; break;
        case 'c': cpus = atol(optarg); break;
        case 'm': mem_mib = atol(optarg); break;
        case 'd': dax_mib = atol(optarg); break;
        case 'l': log_path = optarg; break;
        case 'x': vsock_exec_path = optarg; break;
        case 'S': vsock_svc_path = optarg; break;
        case 'n': net_socket_path = optarg; break;
        case 'N': net2_socket_path = optarg; break;
        case 'M': net2_mac = optarg; break;
        case 'v': {
            if (n_volumes >= MAX_VOLUMES) {
                fprintf(stderr, "too many --volume args (max %d)\n", MAX_VOLUMES);
                return 1;
            }
            char *eq = strchr(optarg, '=');
            if (eq == NULL || eq == optarg || eq[1] == '\0') {
                fprintf(stderr, "bad --volume %s (want TAG=PATH[:ro])\n", optarg);
                return 1;
            }
            *eq = '\0';
            volumes[n_volumes].tag = optarg;
            volumes[n_volumes].path = eq + 1;
            volumes[n_volumes].ro = false;
            size_t plen = strlen(volumes[n_volumes].path);
            if (plen > 3 && strcmp(volumes[n_volumes].path + plen - 3, ":ro") == 0) {
                volumes[n_volumes].path[plen - 3] = '\0';
                volumes[n_volumes].ro = true;
            }
            n_volumes++;
            break;
        }
        case 'R':
            if (n_root_images >= MAX_ROOT_IMAGES) {
                fprintf(stderr, "too many --root-image args (max %d)\n",
                        MAX_ROOT_IMAGES);
                return 1;
            }
            root_images[n_root_images++] = optarg;
            break;
        case 'h': usage(argv[0]); return 0;
        default: usage(argv[0]); return 1;
        }
    }

    if (kernel_path == NULL || rootfs == NULL || optind >= argc) {
        usage(argv[0]);
        return 1;
    }
    char *const *guest_argv = &argv[optind];

    const char *const envp[] = {
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "HOME=/root",
        "TERM=xterm",
        NULL
    };

    /* Keep VMM/guest-kernel diagnostics out of the workload's output
     * streams: with --log they go to their own file; stdout/stderr carry
     * only what the workload writes over the virtio console. */
    int log_fd = KRUN_LOG_TARGET_DEFAULT;
    if (log_path != NULL) {
        log_fd = open(log_path, O_WRONLY | O_CREAT | O_APPEND, 0644);
        if (log_fd < 0) {
            perror(log_path);
            return 1;
        }
    }
    /* PODVM_LOG_LEVEL=debug turns on libkrun debug logging (per-pod via the
     * kube-on-macos.io/vmm-log-level annotation) — used to chase device-level
     * bugs like the vsock connect hang. */
    uint32_t log_level = KRUN_LOG_LEVEL_WARN;
    const char *level_env = getenv("PODVM_LOG_LEVEL");
    if (level_env != NULL && strcmp(level_env, "debug") == 0)
        log_level = KRUN_LOG_LEVEL_DEBUG;
    if (check(krun_init_log(log_fd, log_level, KRUN_LOG_STYLE_AUTO, 0),
              "krun_init_log"))
        return 1;

    int ctx = krun_create_ctx();
    if (ctx < 0) {
        errno = -ctx;
        perror("krun_create_ctx");
        return 1;
    }

    if (check(krun_set_vm_config(ctx, (uint8_t)cpus, (uint32_t)mem_mib),
              "krun_set_vm_config"))
        return 1;

    if (check(krun_add_virtio_console_default(ctx, STDIN_FILENO, STDOUT_FILENO,
                                              STDERR_FILENO),
              "krun_add_virtio_console_default"))
        return 1;

    /* Room for virtio-fs file handles. */
    struct rlimit rlim;
    getrlimit(RLIMIT_NOFILE, &rlim);
    rlim.rlim_cur = rlim.rlim_max;
    setrlimit(RLIMIT_NOFILE, &rlim);

    /* Root filesystem over virtio-fs; dax_mib > 0 requests a DAX window. */
    if (check(krun_add_virtiofs3(ctx, KRUN_FS_ROOT_TAG, rootfs,
                                 (uint64_t)dax_mib * 1024 * 1024, false),
              "krun_add_virtiofs3"))
        return 1;

    /* Each image as a read-only raw EROFS block device (shared across every
     * pod of the image — one file, one host page cache). execd overlays a
     * tmpfs upper per container and chroots the workload in. */
    for (int i = 0; i < n_root_images; i++) {
        char block_id[16];
        snprintf(block_id, sizeof(block_id), "root%d", i);
        if (check(krun_add_disk(ctx, block_id, root_images[i], true),
                  "krun_add_disk"))
            return 1;
    }

    /* Pod volumes: one additional virtio-fs device per volume; execd mounts
     * them by tag at the declared mountPaths. read-only is enforced VMM-side
     * here, and again with MS_RDONLY per-mount in the guest. */
    for (int i = 0; i < n_volumes; i++) {
        if (check(krun_add_virtiofs3(ctx, volumes[i].tag, volumes[i].path, 0,
                                     volumes[i].ro),
                  "krun_add_virtiofs3 (volume)"))
            return 1;
    }

    /* vsock device, no TSI: TSI needs libkrunfw's patched guest kernel, and
     * the Kata kernel is vanilla — the tsi_hijack cmdline flag would just be
     * passed through as an argument to the workload. Guest networking is out
     * of scope for this harness (the real design gives pods routed IPv6 via
     * virtio-net); vsock is here as the control-channel substrate. */
    if (check(krun_add_vsock(ctx, 0), "krun_add_vsock"))
        return 1;

    /* virtio-net backed by a gvproxy vfkit-protocol unixgram socket.
     * Adding a net device implicitly disables libkrun's TSI fallback. */
    if (net_socket_path != NULL) {
        uint8_t mac[6] = { 0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee };
        if (check(krun_add_net_unixgram(ctx, net_socket_path, -1, mac,
                                        COMPAT_NET_FEATURES, NET_FLAG_VFKIT),
                  "krun_add_net_unixgram"))
            return 1;
    }

    /* Second NIC (guest eth1): vmnet via vmnet-helper — same unixgram
     * protocol as gvproxy. The MAC is the vmnet-assigned one so the guest
     * matches what the bridge expects. */
    if (net2_socket_path != NULL) {
        uint8_t mac2[6] = { 0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xef };
        if (net2_mac != NULL &&
            sscanf(net2_mac, "%hhx:%hhx:%hhx:%hhx:%hhx:%hhx", &mac2[0],
                   &mac2[1], &mac2[2], &mac2[3], &mac2[4], &mac2[5]) != 6) {
            fprintf(stderr, "bad --net2-mac %s\n", net2_mac);
            return 1;
        }
        /* No offload features: vmnet-helper runs without checksum/TSO
         * offload by default, so the guest must compute full checksums
         * itself — otherwise TCP leaves the VM with partial checksums and
         * the peer drops it (while ICMP, checksummed in software, works). */
        if (check(krun_add_net_unixgram(ctx, net2_socket_path, -1, mac2,
                                        0, NET_FLAG_VFKIT),
                  "krun_add_net_unixgram (vmnet)"))
            return 1;
    }

    /* Expose guest vsock port 1024 (execd) as a host unix socket the node
     * agent dials for exec/attach sessions. */
    if (vsock_exec_path != NULL) {
        unlink(vsock_exec_path);
        if (check(krun_add_vsock_port2(ctx, 1024, vsock_exec_path, true),
                  "krun_add_vsock_port2"))
            return 1;
    }

    /* Guest-initiated channel (execd dials CID 2 port 1025): service
     * endpoint queries land on the agent's unix socket. */
    if (vsock_svc_path != NULL) {
        if (check(krun_add_vsock_port(ctx, 1025, vsock_svc_path),
                  "krun_add_vsock_port"))
            return 1;
    }

    if (check(krun_set_workdir(ctx, "/"), "krun_set_workdir"))
        return 1;

    if (check(krun_set_exec(ctx, guest_argv[0],
                            (const char *const *)&guest_argv[1], envp),
              "krun_set_exec"))
        return 1;

    /* External kernel: Kata vmlinux.container is an ARM64 raw boot Image.
     * NULL cmdline keeps libkrun's default, which includes init=/init.krun. */
    if (check(krun_set_kernel(ctx, kernel_path, KRUN_KERNEL_FORMAT_RAW, NULL,
                              NULL),
              "krun_set_kernel"))
        return 1;

    /* Never returns on success; process exit code = guest command's. */
    if (check(krun_start_enter(ctx), "krun_start_enter"))
        return 1;

    return 0;
}
