#!/bin/sh
# post-image.sh - Buildroot post-image script for SaviorOS (DESIGN 13.2, 14).
#
# Buildroot runs it as:  post-image.sh BINARIES_DIR ARCH
# (ARCH from BR2_ROOTFS_POST_SCRIPT_ARGS), with TARGET_DIR, BUILD_DIR, HOST_DIR
# and BR2_CONFIG in the environment.
#
# It:
#   1. recompresses the initrd: xz, CRC32 check (the kernel's decompressor
#      rejects CRC64), a dictionary no bigger than needed (the kernel
#      allocates all of it while unpacking); checks it has no setuid files;
#   2. enforces the RAM budget (DESIGN 13.2) and fails the build when over:
#         compressed initrd   i686 <= 24 MiB, x86_64 <= 32 MiB
#         unpacked rootfs     i686 <= 48 MiB, x86_64 <= 64 MiB (excl. modloop)
#   3. writes the payload to BINARIES_DIR/payload/ARCH/:
#         vmlinuz, initrd, kernel.config, savior-release, budget.txt, SHA256SUMS
#   4. optionally builds boot media with os/image/mkimage.sh into
#      BINARIES_DIR/media/ (SAVIOR_MKIMAGE=yes|no|auto; default auto = when
#      the host has GRUB's i386-pc and x86_64-efi modules). "make image" sets
#      no and runs mkimage.sh itself after check-kconfig.
set -eu

log() { echo "savior post-image: $*"; }
die() { echo "savior post-image: ERROR: $*" >&2; exit 1; }

BIN=${1:-}
ARCH=${2:-}
if [ -z "$BIN" ] || [ ! -d "$BIN" ]; then
	die "usage: post-image.sh BINARIES_DIR x86_64|i686"
fi
BIN=$(cd "$BIN" && pwd)
if [ -z "$ARCH" ] && [ -n "${BR2_CONFIG:-}" ] && [ -f "$BR2_CONFIG" ]; then
	ARCH=$(sed -n 's/^BR2_ARCH="\(.*\)"$/\1/p' "$BR2_CONFIG")
fi
case "$ARCH" in
x86_64) MAX_INITRD_MIB=32; MAX_ROOTFS_MIB=64 ;;
i686) MAX_INITRD_MIB=24; MAX_ROOTFS_MIB=48 ;;
*) die "unsupported ARCH '$ARCH'" ;;
esac
BOARD_DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$BOARD_DIR/../../../.." && pwd)
BUILD_DIR=${BUILD_DIR:-$(dirname "$BIN")/build}
TARGET_DIR=${TARGET_DIR:-$(dirname "$BIN")/target}
# shellcheck source=kernel-dir.sh
. "$BOARD_DIR/kernel-dir.sh"

KERNEL="$BIN/bzImage"
[ -f "$KERNEL" ] || die "$KERNEL not found (BR2_LINUX_KERNEL_BZIMAGE)"
[ -f "$BIN/rootfs.cpio.xz" ] || die "$BIN/rootfs.cpio.xz not found (BR2_TARGET_ROOTFS_CPIO + CPIO_XZ)"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/savior-post-image.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# ---------------------------------------------------------------------------
# 1. initrd format and content

# The kernel unpacks the initramfs with its xz decoder in "dynalloc" mode,
# which vmallocs the stream's whole dictionary: 64 MiB for Buildroot's xz -9.
# That is a lot on a 256 MB machine with shared video memory, and on i686 it
# is half the vmalloc area. Recompress the shipped initrd with the smallest
# dictionary that still spans the largest savior binary (the two 386 builds
# deduplicate against each other; a 16 MiB dictionary compresses as well as
# 64 MiB there), a CRC32 check (the kernel rejects CRC64) and one block.
largest=0
for f in "$TARGET_DIR"/usr/bin/savior*; do
	[ -f "$f" ] || continue
	sz=$(wc -c <"$f" | tr -d ' ')
	[ "$sz" -le "$largest" ] || largest=$sz
done
need=$((largest / 1048576 + 2))
dict=${SAVIOR_INITRD_DICT_MIB:-}
if [ -z "$dict" ]; then
	dict=64
	for d in 8 12 16 24 32 48 64; do
		if [ "$d" -ge "$need" ]; then dict=$d; break; fi
	done
fi
case "$dict" in ''|*[!0-9]*) die "SAVIOR_INITRD_DICT_MIB must be a number" ;; esac
xz -dc "$BIN/rootfs.cpio.xz" | xz -9 --check=crc32 --lzma2=preset=9,dict="${dict}MiB" -T1 >"$WORK/initrd.xz" ||
	die "cannot recompress $BIN/rootfs.cpio.xz"
INITRD="$WORK/initrd.xz"
# xz --robot --list: the "file" line is
#   file <streams> <blocks> <compressed> <uncompressed> <ratio> <check> <padding>
xz --robot --list "$INITRD" >"$WORK/xzlist" || die "recompressed initrd is not valid xz"
check=$(awk -F'\t' '$1 == "file" { print $7 }' "$WORK/xzlist")
[ "$check" = CRC32 ] || die "initrd xz check is '$check', need CRC32"
compressed=$(wc -c <"$INITRD" | tr -d ' ')
uncompressed=$(awk -F'\t' '$1 == "file" { print $5 }' "$WORK/xzlist")
case "$uncompressed" in ''|*[!0-9]*) die "cannot read the uncompressed size of $INITRD" ;; esac

xz -dc "$INITRD" | (cd "$WORK" && cpio -itv --quiet) >"$WORK/listing" 2>/dev/null ||
	die "cannot list $INITRD with cpio"
# Mode string is field 1 (e.g. -rwsr-xr-x); s/S in the user or group
# execute position means setuid/setgid.
suid=$(awk '{ m = $1; if (substr(m, 4, 1) ~ /[sS]/ || substr(m, 7, 1) ~ /[sS]/) print $NF }' "$WORK/listing" | head -n 5)
[ -z "$suid" ] || die "setuid/setgid files in the initrd: $suid (post-fakeroot.sh should have refused them)"
for f in init sbin/init etc/inittab lib/modloop.sqfs etc/savior-release; do
	# cpio -tv prints the path last; symlinks end in "path -> target".
	awk -v f="$f" '{ p = $0; sub(/ -> .*$/, "", p); n = split(p, a, " "); if (a[n] == f || a[n] == "./" f) found = 1 } END { exit !found }' \
		"$WORK/listing" || die "/$f is missing from the initrd"
done

# ---------------------------------------------------------------------------
# 2. budget

modloop=0
if [ -f "$TARGET_DIR/lib/modloop.sqfs" ]; then
	modloop=$(wc -c <"$TARGET_DIR/lib/modloop.sqfs" | tr -d ' ')
fi
rootfs=$((uncompressed - modloop))
mib() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1048576 }'; }
max_initrd=$((MAX_INITRD_MIB * 1048576))
max_rootfs=$((MAX_ROOTFS_MIB * 1048576))
PAYLOAD="$BIN/payload/$ARCH"
mkdir -p "$PAYLOAD"
cat >"$PAYLOAD/budget.txt" <<EOF
arch=$ARCH
initrd_compressed_bytes=$compressed
initrd_compressed_max_bytes=$max_initrd
rootfs_unpacked_bytes=$rootfs
rootfs_unpacked_max_bytes=$max_rootfs
modloop_bytes=$modloop
cpio_unpacked_bytes=$uncompressed
xz_dict_mib=$dict
EOF
log "budget $ARCH: initrd $(mib "$compressed")/$MAX_INITRD_MIB MiB compressed, rootfs $(mib "$rootfs")/$MAX_ROOTFS_MIB MiB unpacked (+ modloop $(mib "$modloop") MiB)"
over=""
[ "$compressed" -le "$max_initrd" ] || over="$over compressed initrd $(mib "$compressed") MiB > $MAX_INITRD_MIB MiB;"
[ "$rootfs" -le "$max_rootfs" ] || over="$over unpacked rootfs $(mib "$rootfs") MiB > $MAX_ROOTFS_MIB MiB;"
if [ -n "$over" ]; then
	echo "largest files in the rootfs:" >&2
	awk '$1 !~ /^d/ { print $5, $NF }' "$WORK/listing" | sort -rn | head -n 15 >&2
	die "RAM budget exceeded:$over (DESIGN 13.2)"
fi

# ---------------------------------------------------------------------------
# 3. payload

cp "$KERNEL" "$PAYLOAD/vmlinuz"
cp "$INITRD" "$PAYLOAD/initrd"
rm -f "$PAYLOAD/savior-release" "$PAYLOAD/kernel.config"
[ -f "$TARGET_DIR/etc/savior-release" ] && cp "$TARGET_DIR/etc/savior-release" "$PAYLOAD/savior-release"
# The .config of the kernel this configuration builds, which check-kconfig
# validates (not the first build/linux-*: an old one stays after a bump).
KDIR=$(savior_kernel_dir) || die "cannot tell which kernel this build uses"
[ -f "$KDIR/.config" ] || die "$KDIR/.config not found (did the kernel build?)"
cp "$KDIR/.config" "$PAYLOAD/kernel.config"
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$PAYLOAD" && sha256sum vmlinuz initrd >SHA256SUMS)
else
	(cd "$PAYLOAD" && shasum -a 256 vmlinuz initrd >SHA256SUMS)
fi
log "payload in $PAYLOAD"

# ---------------------------------------------------------------------------
# 4. boot media (optional)

mode=${SAVIOR_MKIMAGE:-auto}
if [ "$mode" = auto ]; then
	grub_lib=${GRUB_LIB:-/usr/lib/grub}
	if command -v grub-mkimage >/dev/null 2>&1 && [ -d "$grub_lib/i386-pc" ] && [ -d "$grub_lib/x86_64-efi" ]; then
		mode=yes
	else
		mode=no
		log "host GRUB not found: skipping boot media (os/image/mkimage.sh builds them from the payload)"
	fi
fi
if [ "$mode" = yes ]; then
	version=$(sed -n 's/^VERSION="\(.*\)"$/\1/p' "$PAYLOAD/savior-release" 2>/dev/null || true)
	sh "$REPO/os/image/mkimage.sh" --out "$BIN/media" --name "savior-$ARCH" --version "${version:-dev}" \
		--payload "$ARCH=$PAYLOAD/vmlinuz,$PAYLOAD/initrd"
fi
