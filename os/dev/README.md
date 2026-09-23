# SaviorOS dev image and QEMU tests

The dev image is SaviorOS built from Ubuntu 24.04 packages instead of
Buildroot (DESIGN 14 "Dev image"). It uses the Ubuntu generic kernel,
`busybox-static`, the same `os/rootfs-overlay` and `os/image/mkimage.sh` as
the production images, and the `savior` binary from this checkout. You can
build it in about a minute on any Ubuntu 24.04 host (a container is fine), and
the QEMU harness boots it from every medium without KVM.

## Requirements

```sh
sudo apt-get install -y \
  busybox-static kmod zstd xz-utils cpio squashfs-tools \
  grub-common grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin \
  mtools dosfstools xorriso \
  qemu-system-x86 ovmf \
  dropbear-bin dnsmasq-base e2fsprogs jq curl
```

- Go 1.24 is needed unless you pass `--savior` (`GOTOOLCHAIN=local` is used).
- `dropbear-bin`, `dnsmasq-base` and `e2fsprogs` are the optional "extras".
  They are copied from the host together with their shared libraries.
- `jq` is needed only by the swarm test. `curl` is optional (a faster hive API
  check). `python3` is optional: without it the harness talks to the QEMU
  monitor through `nc -U`.
- The kernel packages are fetched with `apt-get download`, so the host needs
  apt access to the Ubuntu archive (`noble-updates`).

## Build

```sh
os/dev/build.sh                        # everything, into build/dev
os/dev/build.sh --savior /path/savior  # use a prebuilt linux/amd64 binary
os/dev/build.sh --no-extras --no-media # busybox only, no boot media
os/dev/build.sh --conf my.conf         # bake a config (see "Baked config")
```

| Option | Default | Meaning |
|---|---|---|
| `--out DIR` | `build/dev` | output directory |
| `--savior PATH` | build from this checkout | the `savior` binary to install |
| `--kernel-version V` | `6.8.0-142-generic` | Ubuntu kernel ABI; any version still in the archive |
| `--cache DIR` | `build/cache` | downloaded `.deb`s and extracted kernels (about 330 MB per kernel) |
| `--no-extras` | | leave out dropbear, dnsmasq and e2fsprogs `mke2fs` |
| `--no-media` | | stop after `vmlinuz` + `initrd` |
| `--conf FILE` | | passed to `mkimage.sh --conf` |
| `--formats LIST` | `img,iso,netboot` | passed to `mkimage.sh` |

Output:

```
build/dev/vmlinuz              Ubuntu kernel
build/dev/initrd               newc cpio, xz --check=crc32 -9
build/dev/savior               the savior binary in the image (also runs on the host: savior ctl)
build/dev/media/savior.img     USB image: BIOS + UEFI (x64 and ia32)
build/dev/media/savior.iso     CD image (BIOS + UEFI, hybrid)
build/dev/media/netboot/       PXE tree (TFTP root)
```

What `build.sh` does:

1. It downloads `linux-image-unsigned-V`, `linux-modules-V` and
   `linux-modules-extra-V` into the cache, extracts them once and runs
   `depmod` there.
2. It picks the modules named in [`modules.txt`](modules.txt) and adds their
   dependency closure from `modules.dep`.
   - A required module that the kernel has neither as a module nor built in
     fails the build.
   - `optional:` lines are skipped when absent, and `a|b` takes the first name
     that exists.
   - It decompresses the `.ko.zst` files and runs `depmod -b` on the result.
   - Modules and an (empty) `firmware/` go into `/lib/modloop.sqfs`
     (`mksquashfs -comp xz`). `S00mounts` loop-mounts that file, as it will in
     the Buildroot image.
3. It builds the rootfs:
   - `/bin/busybox` with a symlink for every applet;
   - the overlay, with explicit modes (`/etc/shadow` 0600). The overlay's
     `/init` re-roots the initramfs, then starts BusyBox init. The kernel's
     initial rootfs cannot be `pivot_root`ed, so `/init` bind-mounts it onto
     itself and chroots into that mount first. Without this, every sandboxed
     task fails with `pivot_root: invalid argument`;
   - the extras and their libraries (`ldd`);
   - `/usr/bin/savior` and `/etc/savior-release`;
   - `/dev/console` and `/dev/null`. These are written directly as cpio
     entries, so the build needs no `mknod`.
4. It prints sizes and checks the DESIGN 13.2 x86_64 budget (compressed
   initrd ≤ 32 MiB, unpacked rootfs without the modloop ≤ 64 MiB). Going over
   budget only prints a warning in the dev image.
5. It runs `os/image/mkimage.sh` to make the boot media.

Typical sizes with 6.8.0-142: initrd 17 MiB, unpacked rootfs 26 MiB
(savior 10.8 MiB; dnsmasq and its libraries about 8 MiB), modloop 8.6 MiB
(111 modules), USB image 105 MiB, ISO 43 MiB.

## QEMU tests

```sh
os/dev/qemu-test.sh all             # all boot tests (about 8 minutes)
os/dev/qemu-test.sh boot-bios pxe-uefi
os/dev/qemu-test.sh --swarm all     # also the multi-VM swarm and hive-pxe tests
os/dev/qemu-test.sh --expect-display screen
```

The harness uses TCG only, so no KVM is needed. A boot to the status line
takes 30-60 s, and a BIOS PXE boot can take a few minutes while GRUB fetches
the payload over TFTP. Test VMs have 512 MiB of RAM (`--mem`) and one CPU.

| Test | What boots | Main checks |
|---|---|---|
| `boot-bios` | `savior.img` as a USB stick on EHCI, SeaBIOS | config from the stick (a test `swarm_key` is written into its `savior.conf` with mtools), DHCP address, console started, `/dev/fb0`, GRUB took `$root` from the core image prefix `(,msdos1)`, `/boot-options.cfg` is sourced, no respawn flood |
| `boot-uefi` | the same stick, OVMF | the same checks |
| `boot-iso-bios` | `savior.iso` as an IDE CD-ROM, SeaBIOS | config medium is `/dev/sr0` (matched by the ISO volume UUID) |
| `boot-iso-uefi` | the ISO under OVMF, plus a plain FAT stick holding only `savior.conf` | a CD-booted machine takes the stick's config |
| `pxe-bios`, `pxe-uefi` | `netboot/` over QEMU's built-in TFTP (`core.0` / `core.efi`) | `savior.media=none`; the test key goes on the kernel command line (test only) |
| `screen` | the stick with `-vga std` | `/dev/fb0`, a screendump saved as a PPM through the QEMU monitor; with `--expect-display`, at least 1% of the screen is not black |
| `baked-conf` | a stick made with `mkimage.sh --conf`, with `savior.conf` deleted from it | the key comes from `/etc/savior/baked.conf` (second initrd `boot/savior-conf.cpio`) |
| `swarm` | see below | |
| `hive-pxe` | a hive VM with `netboot = yes` and `dhcp_server = yes`, then two diskless VMs (SeaBIOS, OVMF) on a private LAN | each client PXE-boots from the hive (S65netboot: dnsmasq DHCP with the boot file chosen by client architecture, GRUB images over TFTP, kernel and initrd over HTTP from the hive's port 7702). Its node joins keyless, is pending, and `savior ctl approve` brings it online |

Every check prints `PASS:` or `FAIL:`, and a summary with timings comes
last. The exit status is non-zero if anything failed. The logs go to
`build/dev/logs/`:

- `<test>.log`: the harness transcript, including the QEMU command lines and
  `savior ctl` output;
- `<test>.serial`: the guest serial console (`<test>-<vm>.serial` for the swarm);
- `*.ppm`: screendumps.

`--keep` keeps the work directory with the stick copies under
`build/dev/qemu-work/`. QEMU processes are killed on exit, including on
Ctrl-C.

### How the harness sees inside the guest

At the end of boot, `/usr/libexec/savior/boot-report`, started in the
background by `rcS`, writes one line to the serial console:

```
SAVIOR-BOOT: rcS done up=14.96 cfg=yes media=/dev/sda1 key=yes fb=yes net=10.0.2.15 console=yes node=yes ver=dev-1a2b3c4 t=net:14.86,fb:13.78,console:13.79
```

It waits up to 40 s for an IPv4 address, `/dev/fb0` and the first start of
the tty1 console, then prints the line. The fields are:

- `key`: whether a swarm key is set. The key itself is never printed.
- `console` and `node`: `/usr/libexec/savior/run` has started them.
- `t`: the uptime at which each condition was first seen.

`rcS` also prints `SAVIOR-BOOT: rcS start` once `/proc` and `/dev` exist.

With `savior_dumplog=1` on the kernel command line, the boot log
(`/var/log/savior-init.log`) and syslog (`logread`) are copied to the serial
console as `SAVIOR-LOG:` / `SAVIOR-SYSLOG:` lines. The stick tests set it
through `/boot-options.cfg` (`set savior_args="savior_dumplog=1"`). The PXE
tests set it in the netboot `grub.cfg`.

Serial output only happens when the kernel command line has `console=ttyS0`,
as every SaviorOS menu entry does.

### Swarm test

`qemu-test.sh swarm`, or `--swarm all`, runs a whole swarm on one host:

- **LAN:** a private QEMU socket-multicast LAN
  (`-netdev socket,mcast=230.x.y.z:port,localaddr=127.0.0.1`) with a random
  group and port per run. Every VM has its own MAC and `-uuid`.
- **hive VM** (1 GiB):
  - `roles = hive,compute`, `net = static`, `ip = 10.77.0.1/24`,
    `dhcp_server = yes`, and an `admin_token` from the test;
  - a second NIC with QEMU user networking forwards
    `127.0.0.1:<random port>` on the host to the hive's port 7700. With
    `net = static`, extra wired NICs use DHCP;
  - its stick copy has 512 MiB more space, so `savior storage init-data`
    creates SAVIOR-DATA.
- **c1, c2:** `roles = compute`, DHCP from the hive.
- **disp:** `roles = display`.

The steps:

1. All VMs boot and the nodes get `10.77.0.x` leases from the hive.
2. The hive API answers on the forwarded port.
3. `savior ctl` (the `build/dev/savior` binary, run on the host with
   `--config` in the work directory, `--hive 127.0.0.1:<port>` and
   `--token <admin token>`) sees 4 online nodes.
4. `savior ctl submit job.json --fetch DIR` runs 4 tasks. Each writes
   `task <i> of 4` into `out.txt`, and all four outputs must arrive with the
   right contents.
5. `savior ctl display <disp> text --text "HELLO SWARM"`. A screendump of the
   display VM must show non-black pixels and differ from the one taken before.
6. `savior ctl identify --all` succeeds.
7. **Duplicate ID:** a fifth VM boots with c1's MAC and SMBIOS UUID, so it
   derives the same node ID, but with its own static address. Its console must
   report the `duplicate` state, and the hive must still list 4 online nodes.

The swarm test needs working `savior hive` and `savior node`. With
placeholder builds it stops at step 2.

## Running the image by hand

```sh
# USB stick, BIOS (copy it first if you want to edit savior.conf)
qemu-system-x86_64 -m 512 -vga std -serial stdio \
  -drive if=none,id=stick,format=raw,file=build/dev/media/savior.img \
  -device usb-ehci,id=ehci -device usb-storage,bus=ehci.0,drive=stick,bootindex=0 \
  -netdev user,id=n0 -device e1000,netdev=n0

# UEFI: add -bios /usr/share/ovmf/OVMF.fd
# CD:   -drive if=none,id=cd,media=cdrom,file=build/dev/media/savior.iso -device ide-cd,drive=cd
# PXE:  -netdev user,id=n0,tftp=build/dev/media/netboot,bootfile=boot/grub/i386-pc/core.0 \
#       -device e1000,netdev=n0,bootindex=0
# Direct kernel boot (no GRUB):
qemu-system-x86_64 -m 512 -serial stdio -kernel build/dev/vmlinuz -initrd build/dev/initrd \
  -append "console=tty0 console=ttyS0,115200 savior.media=none savior.swarm_key=testtesttesttest"
```

Edit the stick's config without mounting it:

```sh
MTOOLS_SKIP_CHECK=1 mtype -i savior.img@@1M ::/savior.conf
MTOOLS_SKIP_CHECK=1 mcopy -o -i savior.img@@1M my.conf ::/savior.conf
```

For a root shell, pick "rescue shell" in the boot menu (Alt+F2), or set
`console_shell = yes`. For SSH, set `ssh_key`, which uses dropbear from the
extras.

## Baked config

`build.sh --conf FILE`, or `mkimage.sh --conf FILE`, does two things. It
copies FILE to the stick as `savior.conf`, where it can be edited. It also
bakes FILE into `boot/savior-conf.cpio`, which holds
`etc/savior/baked.conf` with mode 0600.

Every USB and ISO menu entry loads that cpio as a second initrd, so the
configuration also works when the stick's `savior.conf` is missing or
unreadable (DESIGN 5.1, source 3). The netboot tree never gets the cpio,
because TFTP has no access control.

## Differences from the production (Buildroot) image

- The kernel is Ubuntu's generic kernel. It has `CONFIG_USER_NS=y`, and
  `S10system` sets `user.max_user_namespaces=0` instead.
- Storage drivers such as `usb-storage`, `isofs`, `uas` and `nls_utf8` are
  modules, loaded by the `S05mdev` coldplug from the modloop.
- There is no firmware, no `wpa_supplicant`, no `ntpd` and no `zcip`
  (Ubuntu's busybox lacks them), and dropbear and dnsmasq come from the host.
- The image is x86_64 only. The i686 payload and the 386 binaries come with
  the Buildroot build.
