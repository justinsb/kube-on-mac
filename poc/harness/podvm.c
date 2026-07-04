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
            "[--dax-mb MB] -- COMMAND [ARGS...]\n",
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
    long cpus = 1;
    long mem_mib = 256;
    long dax_mib = 0;

    static const struct option opts[] = {
        { "kernel", required_argument, NULL, 'k' },
        { "rootfs", required_argument, NULL, 'r' },
        { "cpus", required_argument, NULL, 'c' },
        { "mem", required_argument, NULL, 'm' },
        { "dax-mb", required_argument, NULL, 'd' },
        { "help", no_argument, NULL, 'h' },
        { NULL, 0, NULL, 0 }
    };

    int c;
    while ((c = getopt_long(argc, argv, "+k:r:c:m:d:h", opts, NULL)) != -1) {
        switch (c) {
        case 'k': kernel_path = optarg; break;
        case 'r': rootfs = optarg; break;
        case 'c': cpus = atol(optarg); break;
        case 'm': mem_mib = atol(optarg); break;
        case 'd': dax_mib = atol(optarg); break;
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

    if (check(krun_init_log(KRUN_LOG_TARGET_DEFAULT, KRUN_LOG_LEVEL_WARN,
                            KRUN_LOG_STYLE_AUTO, 0),
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

    /* vsock device, no TSI: TSI needs libkrunfw's patched guest kernel, and
     * the Kata kernel is vanilla — the tsi_hijack cmdline flag would just be
     * passed through as an argument to the workload. Guest networking is out
     * of scope for this harness (the real design gives pods routed IPv6 via
     * virtio-net); vsock is here as the control-channel substrate. */
    if (check(krun_add_vsock(ctx, 0), "krun_add_vsock"))
        return 1;

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
