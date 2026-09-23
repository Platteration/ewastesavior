# os/buildroot: SaviorOS production images

This directory is a Buildroot `BR2_EXTERNAL` tree (name `SAVIOR`). It builds
the two SaviorOS payloads described in `docs/DESIGN.md` 13.2 and 14. Each
payload is a kernel plus an xz initramfs:

| payload | CPUs | kernel | savior binaries |
|---|---|---|---|
| `x86_64` | any 64-bit x86 | 6.12 LTS, generic x86-64, `EFI_MIXED` | `/usr/bin/savior` (`GOAMD64=v1`) |
| `i686` | Pentium Pro and later, no PAE or SSE2 needed | 6.12 LTS, `M686`, `HIGHMEM4G`, no PAE | `/usr/bin/savior-sse2`, `/usr/bin/savior-softfloat` (S00mounts keeps one) |

Boot media (USB image, ISO, netboot tree) are not built by Buildroot. They
come from `os/image/mkimage.sh` with the host's GRUB, the same way as for
the dev image.

## Building

From the repository root, on Linux:

```sh
make image ARCH=x86_64      # or ARCH=i686
make universal-image        # both, then one stick/ISO that picks the payload by CPU
```

`make image` runs these steps:

1. It downloads the pinned Buildroot release (`BUILDROOT_VERSION` in the
   Makefile, currently 2025.02.5, an LTS point release) into
   `build/br-cache/dl/` and verifies it with `scripts/fetch-buildroot.sh`
   (see "Pinning Buildroot" below).
2. It builds the savior binaries with `make build-linux`.
3. It loads `savior_<arch>_defconfig` into `build/br-<arch>/` and runs
   `scripts/check-defconfig.sh`. After `olddefconfig`, every symbol in the
   defconfig must still have the value the defconfig gives it. A misspelt or
   renamed option fails the build instead of silently disappearing.
4. It builds with Buildroot. The post-build and post-image scripts are
   described below.
5. It runs `scripts/check-kconfig.sh`, which checks the kernel `.config`
   against `board/savior/linux/required.txt`.
6. It runs `os/image/mkimage.sh`, which writes
   `build/images/<arch>/savior-<arch>.{img,iso}` and `netboot/`.

A first build takes one to two hours: the musl toolchain, the kernel, and
the packages. Later builds reuse `build/br-<arch>/`, and `BR2_CCACHE` is
enabled with the default `~/.buildroot-ccache`. `make clean` keeps the
Buildroot trees, and `make distclean` removes them.

You can also drive Buildroot directly. In that case, post-image.sh also
builds boot media into `images/media/` when the host has GRUB
(`SAVIOR_MKIMAGE=yes|no|auto`):

```sh
make -C build/buildroot-2025.02.5 O=$PWD/build/br-x86_64 BR2_EXTERNAL=$PWD/os/buildroot savior_x86_64_defconfig
SAVIOR_BIN_DIR=$PWD/build/linux-amd64 make -C build/buildroot-2025.02.5 O=$PWD/build/br-x86_64
```

## Layout

```
external.desc, external.mk, Config.in    BR2_EXTERNAL glue ("SaviorOS" menu)
configs/savior_x86_64_defconfig          x86_64 payload
configs/savior_i686_defconfig            i686 payload (differs only in arch, fragment, script args)
buildroot.hash                           pinned SHA-256 of Buildroot tarballs
package/savior-firmware/                 firmware allowlist installer (see below)
board/savior/
  linux/common.config                    both arches: base, cgroups/namespaces, zram, squashfs,
                                         filesystems, built-in boot storage, ACPI/sensors, EFI
  linux/x86_64.config, linux/i686.config per-arch CPU, memory model and cpufreq
  linux/display.config                   simpledrm/efifb/vesafb built in, KMS drivers as modules
  linux/net.config, linux/wifi.config    NIC, USB-net and Wi-Fi drivers (modules)
  linux/hardening.config                 no user namespaces, no bpf(), no userfaultfd(), ...
  linux/required.txt                     normative kernel symbol list (DESIGN 14)
  busybox.fragment                       applets the init scripts and task scripts rely on
  firmware.list                          firmware allowlist (DESIGN 13.2)
  install-firmware.sh                    installs / prunes firmware by that list
  post-build.sh, post-image.sh           see below
```

Kernel: the pinned 6.12.y LTS release (`BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE`),
built from the arch defconfig plus the fragments above, in the order common,
arch, display, net, wifi, hardening. The toolchain uses the headers of the
same kernel (`BR2_KERNEL_HEADERS_AS_KERNEL`). Anything needed before the
modloop is mounted is built in: initramfs, squashfs, loop, USB/SATA/PATA
storage, FAT/ISO9660/ext4, the serial and VT consoles, and firmware
framebuffers. Hardware drivers are modules. mdev loads them by modalias from
the modloop.

The rootfs overlay is `os/rootfs-overlay` (`BR2_ROOTFS_OVERLAY`). It is shared
with the dev image and provides inittab, rcS/rcK, the `S??` init scripts and
`/usr/libexec/savior`.

## post-build.sh

Buildroot runs it on every `make` with `TARGET_DIR` and the arch. It is
idempotent and does the following:

* It installs the prebuilt savior binaries from `$SAVIOR_BIN_DIR` (the
  Makefile passes `build/linux-amd64` or `build/linux-386`). It first checks
  that each one is a static ELF for the right architecture, and that the Go
  build info says `GOAMD64=v1`, `GO386=sse2` or `GO386=softfloat`.
* It writes `/etc/savior-release` (`VERSION=`, `ARCH=`, `BUILD_UNIX=`,
  `KERNEL=`, `BUILDROOT=`) and `/usr/lib/os-release`.
* It deletes every `/etc/init.d/S*` that the overlay doesn't ship. That
  covers Buildroot's syslogd, klogd, mdev, network, dropbear, dnsmasq and
  seedrng scripts. It then asserts that the remaining scripts, `inittab`,
  `rcS` and `rcK` are byte-identical to the overlay, and warns about
  differences from the DESIGN 13.3 table.
* It asserts that root has no usable password.
* It strips documentation, headers and static libraries, plus the e2fsprogs
  tools a node never runs (it keeps `mke2fs` and `e2fsck`).
* It prunes the firmware to `firmware.list` (keeping `regulatory.db`) as it
  moves it into the modloop, so a changed list also applies to incremental
  rebuilds.
* It moves `/lib/modules` and `/lib/firmware` into `/lib/modloop.sqfs`: xz,
  BCJ x86, 1 MiB blocks, all files owned by root, reproducible with
  `SOURCE_DATE_EPOCH`. The squashfs has two top-level directories, `modules/`
  and `firmware/`, and the empty `/lib/modules` and `/lib/firmware` are left
  as mountpoints. S00mounts loop-mounts it on `/run/modloop` and bind-mounts
  the two directories.
* It checks that the programs the init scripts call exist. These include
  mdev, udhcpc, udhcpd, ntpd, zcip, findfs, blkid, mkfs.vfat, fdisk,
  start-stop-daemon, logger, setsid, setpriv, mke2fs, dropbear, dnsmasq,
  wpa_supplicant and iw. It also checks for the CA bundle and that there are
  no setuid files.

## post-image.sh

* It recompresses the initrd with xz, a CRC32 check (the kernel rejects
  CRC64), and the smallest LZMA2 dictionary that spans the largest savior
  binary (8 to 64 MiB). The kernel vmallocs the whole dictionary while it
  unpacks the initramfs, and Buildroot's `xz -9` would ask for 64 MiB. A
  smaller dictionary leaves the size unchanged because the two 386 builds
  still deduplicate. It also checks that the cpio contains `/init`,
  `/sbin/init`, the modloop and the release file, and no setuid or setgid
  files.
* It enforces the RAM budget (DESIGN 13.2) and fails the build when the
  payload is over:

  | | compressed initrd (incl. modloop) | unpacked rootfs (excl. modloop) |
  |---|---|---|
  | i686 | 24 MiB | 48 MiB |
  | x86_64 | 32 MiB | 64 MiB |

* It writes `images/payload/<arch>/` with `vmlinuz`, `initrd`,
  `kernel.config`, `savior-release`, `budget.txt` and `SHA256SUMS`.
  `make universal-media`, CI and `scripts/qemu-smoke.sh --payload-dir` use
  this directory.

## Firmware

`package/savior-firmware` uses the same linux-firmware tarball and download
directory as Buildroot's `linux-firmware` package, so the firmware version
follows the pinned Buildroot release. It installs only the files that match
`board/savior/firmware.list`:

* Intel e100 and iwlwifi. For iwlwifi that means iwlegacy 3945/4965, the dvm
  1000 to 6000 series, and mvm 3160/3168/7260/7265, newest API only.
* Realtek `rtl_nic`, plus rtlwifi 8192c.
* Atheros carl9170 and ath9k_htc.
* Ralink rt2x00.
* Broadcom brcmsmac.
* radeon R100 to Southern Islands, excluding CIK parts and the UVD/VCE
  engines.

Old names that upstream now lists as WHENCE `Link:` entries still work. This
approach avoids depending on the per-driver `BR2_PACKAGE_LINUX_FIRMWARE_*`
options, which change between releases. If a required pattern matches
nothing, the build fails, so upstream renames are noticed. ipw2x00 and b43
firmware cannot be redistributed; users put it in `/firmware/` on the stick.

## Pinning Buildroot

`scripts/fetch-buildroot.sh` checks the downloaded tarball in this order:

1. The SHA-256 in `buildroot.hash`, if that file has one. A mismatch is fatal.
2. Otherwise the SHA-256 in the release's `.sign` manifest from
   buildroot.org. If `BUILDROOT_GPG=1` and the Buildroot release key is in the
   keyring, it also checks the signature with gpg.

In the second case it prints the line to add to `buildroot.hash`. Tag builds
in CI set `BUILDROOT_REQUIRE_PIN=1`, so pin the hash before tagging a
release. The images workflow's job summary prints the hash.

## Checks and tests

* `scripts/check-defconfig.sh`: the BR2 symbols survive `olddefconfig`.
* `scripts/check-kconfig.sh`: the kernel `.config` satisfies `required.txt`.
  Sections are `[all]`, `[x86_64]` and `[i686]`. Values can be `=y`, `=m`,
  `=y|m`, exact strings, or `# CONFIG_X is not set`.
* `scripts/test-scripts.sh` (`make test-scripts`): self-tests for the
  scripts above, plus install-firmware, fetch-buildroot and post-build
  against a fake Buildroot tree.
* `scripts/qemu-smoke.sh`: boots a payload directory, USB image or ISO under
  SeaBIOS, OVMF x64 or OVMF ia32, and waits for the boot marker on the serial
  console. CI boots the i686 image on `-cpu pentium3,-pae -m 256` under TCG,
  because under KVM an SSE2 instruction would still run.
* `.github/workflows/images.yml` runs all of this for both architectures,
  builds the universal image, and publishes a GitHub release on `v*` tags.

## Changing things

* **Kernel bump.** Change `BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE` in both
  defconfigs, and the `BR2_PACKAGE_HOST_LINUX_HEADERS_CUSTOM_6_x` series if
  the major version changes. `check-kconfig` then tells you about renamed
  symbols (for example `PAGE_TABLE_ISOLATION` became
  `MITIGATION_PAGE_TABLE_ISOLATION` in 6.9).
* **New driver.** Add it as `=m` to the matching fragment, add its firmware
  to `firmware.list`, and add it to `required.txt` if the DESIGN requires it.
* **New package.** Add it to both defconfigs. If it installs an init script,
  post-build deletes it, because only the overlay's `S??` scripts run.
