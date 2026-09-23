#!/bin/sh
# post-build.sh - Buildroot post-build script for SaviorOS (DESIGN 13.2, 13.3, 14).
#
# Buildroot runs it as:  post-build.sh TARGET_DIR ARCH     (ARCH = x86_64 | i686,
# from BR2_ROOTFS_POST_SCRIPT_ARGS), after the rootfs overlay has been copied,
# on every "make". It is idempotent.
#
# It:
#   1. installs the prebuilt savior binaries (x86_64: /usr/bin/savior;
#      i686: /usr/bin/savior-sse2 + /usr/bin/savior-softfloat, S00mounts keeps
#      one of them) after checking they are static ELF files for ARCH;
#   2. writes /etc/savior-release and /usr/lib/os-release (the kernel is
#      the one BR2_CONFIG names, build/linux-<BR2_LINUX_KERNEL_VERSION>);
#   3. deletes every /etc/init.d/S* script the rootfs overlay doesn't ship and
#      asserts the result; makes /etc/dropbear a directory;
#   4. strips files a RAM-only node doesn't need;
#   5. moves /lib/modules and /lib/firmware into /lib/modloop.sqfs (xz squashfs
#      with top-level directories modules/ and firmware/), leaving empty
#      mountpoints for S00mounts;
#   6. prunes the firmware to board/savior/firmware.list on the way;
#   7. checks the applets and programs the init scripts rely on, and the
#      radeon module option this kernel needs.
#
# Environment from Buildroot: HOST_DIR, BUILD_DIR, BR2_CONFIG.
# Optional environment (the top-level Makefile sets these):
#   SAVIOR_BIN_DIR     directory holding the savior build(s) for ARCH
#                      (default <repo>/build/linux-amd64 or <repo>/build/linux-386)
#   SAVIOR_VERSION     version string (default: git describe, else "dev")
#   SAVIOR_BUILD_UNIX  build time, Unix seconds (default: SOURCE_DATE_EPOCH, else now)
#   SAVIOR_OVERLAY     rootfs overlay (default <repo>/os/rootfs-overlay)
set -eu
umask 022

log() { echo "savior post-build: $*"; }
die() { echo "savior post-build: ERROR: $*" >&2; exit 1; }

TARGET_DIR=${1:-}
ARCH=${2:-}
if [ -z "$TARGET_DIR" ] || [ ! -d "$TARGET_DIR" ]; then
	die "usage: post-build.sh TARGET_DIR x86_64|i686"
fi
TARGET_DIR=$(cd "$TARGET_DIR" && pwd)
case "$TARGET_DIR" in /|"") die "refusing to work on TARGET_DIR=$TARGET_DIR" ;; esac
if [ -z "$ARCH" ] && [ -n "${BR2_CONFIG:-}" ] && [ -f "$BR2_CONFIG" ]; then
	ARCH=$(sed -n 's/^BR2_ARCH="\(.*\)"$/\1/p' "$BR2_CONFIG")
fi
case "$ARCH" in
x86_64) GOARCH=amd64; ELFCLASS=2; ELFMACHINE=62 ;;
i686) GOARCH=386; ELFCLASS=1; ELFMACHINE=3 ;;
*) die "unsupported ARCH '$ARCH' (want x86_64 or i686; set BR2_ROOTFS_POST_SCRIPT_ARGS)" ;;
esac

BOARD_DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$BOARD_DIR/../../../.." && pwd)
: "${HOST_DIR:?HOST_DIR is not set: run this from Buildroot}"
BUILD_DIR=${BUILD_DIR:-$(dirname "$HOST_DIR")/build}
OVERLAY=${SAVIOR_OVERLAY:-$REPO/os/rootfs-overlay}
BIN_DIR=${SAVIOR_BIN_DIR:-$REPO/build/linux-$GOARCH}
FW_LIST="$BOARD_DIR/firmware.list"
# shellcheck source=kernel-dir.sh
. "$BOARD_DIR/kernel-dir.sh"

# ---------------------------------------------------------------------------
# 1. savior binaries

# rd FILE OFFSET SIZE: unsigned little-endian integer at OFFSET.
rd() {
	od -An -tu1 -j "$2" -N "$3" "$1" |
		awk 'BEGIN { m = 1 } { for (i = 1; i <= NF; i++) { v += $i * m; m *= 256 } } END { printf "%d\n", v }'
}

# check_elf FILE: static ELF executable of this ARCH.
check_elf() {
	f=$1
	[ "$(od -An -tx1 -N4 "$f" | tr -d ' \n')" = "7f454c46" ] || die "$f is not an ELF file"
	[ "$(rd "$f" 4 1)" = "$ELFCLASS" ] || die "$f has the wrong ELF class for $ARCH"
	[ "$(rd "$f" 18 2)" = "$ELFMACHINE" ] || die "$f is not built for $ARCH (e_machine $(rd "$f" 18 2))"
	if [ "$ELFCLASS" = 2 ]; then
		phoff=$(rd "$f" 32 8); phentsize=$(rd "$f" 54 2); phnum=$(rd "$f" 56 2)
	else
		phoff=$(rd "$f" 28 4); phentsize=$(rd "$f" 42 2); phnum=$(rd "$f" 44 2)
	fi
	i=0
	while [ "$i" -lt "$phnum" ]; do
		# PT_INTERP (3): dynamically linked; the node has no matching libc.
		[ "$(rd "$f" $((phoff + i * phentsize)) 4)" != 3 ] ||
			die "$f is dynamically linked; build it with CGO_ENABLED=0"
		i=$((i + 1))
	done
}

# check_goenv FILE KEY VALUE: the Go build info, when present, says KEY=VALUE.
check_goenv() {
	got=$(LC_ALL=C grep -a -o "$2=[A-Za-z0-9]*" "$1" | head -n 1 || true)
	if [ -z "$got" ]; then
		log "warning: no $2 in the build info of $1; cannot verify it"
	elif [ "$got" != "$2=$3" ]; then
		die "$1 was built with $got, need $2=$3"
	fi
}

install_bin() {
	src=$1 dst=$2
	[ -f "$src" ] || die "missing $src (run 'make build-linux' first, or set SAVIOR_BIN_DIR)"
	check_elf "$src"
	mkdir -p "$TARGET_DIR/usr/bin"
	rm -f "$TARGET_DIR$dst"
	cp "$src" "$TARGET_DIR$dst"
	chmod 0755 "$TARGET_DIR$dst"
	log "installed $dst ($(wc -c <"$src" | tr -d ' ') bytes)"
}

rm -f "$TARGET_DIR/usr/bin/savior" "$TARGET_DIR/usr/bin/savior-sse2" "$TARGET_DIR/usr/bin/savior-softfloat"
if [ "$ARCH" = x86_64 ]; then
	check_goenv "$BIN_DIR/savior" GOAMD64 v1
	install_bin "$BIN_DIR/savior" /usr/bin/savior
else
	check_goenv "$BIN_DIR/savior-sse2" GO386 sse2
	check_goenv "$BIN_DIR/savior-softfloat" GO386 softfloat
	install_bin "$BIN_DIR/savior-sse2" /usr/bin/savior-sse2
	install_bin "$BIN_DIR/savior-softfloat" /usr/bin/savior-softfloat
fi

# ---------------------------------------------------------------------------
# 2. release files

VERSION=${SAVIOR_VERSION:-$(git -C "$REPO" describe --always --dirty 2>/dev/null || echo dev)}
BUILD_UNIX=${SAVIOR_BUILD_UNIX:-${SOURCE_DATE_EPOCH:-$(date +%s)}}
case "$VERSION" in *[!A-Za-z0-9._+~-]*|"") die "unsafe SAVIOR_VERSION '$VERSION'" ;; esac
case "$BUILD_UNIX" in *[!0-9]*|"") die "SAVIOR_BUILD_UNIX must be a number" ;; esac
BR_VERSION=""
if [ -n "${BR2_CONFIG:-}" ] && [ -f "$BR2_CONFIG" ]; then
	BR_VERSION=$(sed -n 's/^# Buildroot \([^ ]*\) Configuration$/\1/p' "$BR2_CONFIG" | head -n 1)
fi
# The kernel this configuration builds (not the first build/linux-*: an old
# one stays there after a kernel bump).
KDIR=$(savior_kernel_dir) || die "cannot tell which kernel this build uses"
if [ ! -f "$KDIR/.config" ] || [ ! -f "$KDIR/include/config/kernel.release" ]; then
	die "$KDIR has no .config or include/config/kernel.release (did the kernel build?)"
fi
KVER=$(cat "$KDIR/include/config/kernel.release")
case "$KVER" in ""|*/*|.*) die "bad kernel release '$KVER' in $KDIR" ;; esac

# Never write through a symlink that could point out of TARGET_DIR.
rm -f "$TARGET_DIR/etc/savior-release"
cat >"$TARGET_DIR/etc/savior-release" <<EOF
NAME="SaviorOS"
VERSION="$VERSION"
ARCH="$ARCH"
BUILD_UNIX="$BUILD_UNIX"
KERNEL="$KVER"
BUILDROOT="$BR_VERSION"
EOF
chmod 0644 "$TARGET_DIR/etc/savior-release"
mkdir -p "$TARGET_DIR/usr/lib"
rm -f "$TARGET_DIR/usr/lib/os-release" "$TARGET_DIR/etc/os-release"
cat >"$TARGET_DIR/usr/lib/os-release" <<EOF
NAME="SaviorOS"
ID=savior
VERSION="$VERSION"
VERSION_ID="$VERSION"
PRETTY_NAME="SaviorOS $VERSION ($ARCH)"
HOME_URL="https://github.com/platteration/ewastesavior"
EOF
chmod 0644 "$TARGET_DIR/usr/lib/os-release"
ln -s ../usr/lib/os-release "$TARGET_DIR/etc/os-release"
log "release $VERSION, kernel $KVER ($KDIR), Buildroot ${BR_VERSION:-unknown}"

# ---------------------------------------------------------------------------
# 3. init scripts: only what the overlay ships (rcS runs an allowlist anyway)

DESIGN_INIT="S00mounts S05mdev S08config S10system S20scratch S30network S35dhcpd S40time S50sshd S65netboot"
[ -d "$OVERLAY/etc/init.d" ] || die "rootfs overlay $OVERLAY has no etc/init.d"
ALLOW=""
for f in "$OVERLAY"/etc/init.d/S??*; do
	[ -f "$f" ] || continue
	ALLOW="$ALLOW $(basename "$f")"
done
[ -n "$ALLOW" ] || die "rootfs overlay $OVERLAY ships no /etc/init.d/S??* scripts"
for f in "$TARGET_DIR"/etc/init.d/S*; do
	[ -e "$f" ] || [ -L "$f" ] || continue
	n=$(basename "$f")
	case " $ALLOW " in
	*" $n "*) ;;
	*) rm -f "$f"; log "removed init script $n (not in the overlay allowlist)" ;;
	esac
done
have=""
for f in "$TARGET_DIR"/etc/init.d/S*; do
	[ -e "$f" ] || [ -L "$f" ] || continue
	have="$have $(basename "$f")"
done
[ "$have" = "$ALLOW" ] || die "init scripts in target ($have ) differ from the overlay ($ALLOW )"
for n in $ALLOW; do
	cmp -s "$OVERLAY/etc/init.d/$n" "$TARGET_DIR/etc/init.d/$n" ||
		die "/etc/init.d/$n in target differs from the overlay (a package overwrote it?)"
	chmod 0755 "$TARGET_DIR/etc/init.d/$n"
	case " $DESIGN_INIT " in *" $n "*) ;; *) log "warning: init script $n is not in the DESIGN 13.3 table" ;; esac
done
for n in $DESIGN_INIT; do
	case " $ALLOW " in *" $n "*) ;; *) log "warning: DESIGN 13.3 lists $n but the overlay doesn't ship it" ;; esac
done
for f in etc/inittab etc/init.d/rcS etc/init.d/rcK; do
	if [ -f "$OVERLAY/$f" ]; then
		cmp -s "$OVERLAY/$f" "$TARGET_DIR/$f" || die "/$f in target differs from the overlay"
	fi
done
[ -f "$TARGET_DIR/etc/inittab" ] || die "no /etc/inittab in target"

# Root must not have a usable password (DESIGN 13.5).
if [ -f "$TARGET_DIR/etc/shadow" ]; then
	rootpw=$(sed -n 's/^root:\([^:]*\):.*/\1/p' "$TARGET_DIR/etc/shadow")
	case "$rootpw" in
	"*"|"!"*) ;;
	"") die "root has an empty password in /etc/shadow (enable-root-login must be off)" ;;
	*) die "root has a password in /etc/shadow; SaviorOS locks it" ;;
	esac
fi

# SSH host key directory. Buildroot's dropbear package makes /etc/dropbear a
# link to /var/run/dropbear and relies on its S50dropbear (deleted above) to
# replace the link with a directory at boot. /run is an empty tmpfs here, so
# the link would dangle and S50sshd could not write the host key.
if [ -L "$TARGET_DIR/etc/dropbear" ]; then
	rm -f "$TARGET_DIR/etc/dropbear"
	log "replaced the /etc/dropbear link with a directory"
fi
# A reinstalled package ("ln -snf" onto the directory) leaves a link inside.
if [ -L "$TARGET_DIR/etc/dropbear/dropbear" ]; then rm -f "$TARGET_DIR/etc/dropbear/dropbear"; fi
mkdir -p "$TARGET_DIR/etc/dropbear"
chmod 0700 "$TARGET_DIR/etc/dropbear"
if [ -L "$TARGET_DIR/etc/dropbear" ] || [ ! -d "$TARGET_DIR/etc/dropbear" ]; then
	die "/etc/dropbear in target is not a directory"
fi

# ---------------------------------------------------------------------------
# 4. strip what a RAM-only node doesn't need

for d in usr/share/man usr/man usr/share/info usr/info usr/share/doc usr/doc \
	usr/include usr/lib/pkgconfig usr/share/pkgconfig usr/lib/cmake usr/share/cmake \
	usr/share/aclocal usr/share/bash-completion usr/share/zsh usr/share/locale \
	usr/share/gtk-doc usr/lib/gconv; do
	rm -rf "${TARGET_DIR:?}/$d"
done
for d in lib usr/lib usr/libexec; do
	[ -d "$TARGET_DIR/$d" ] || continue
	find "$TARGET_DIR/$d" \( -path "$TARGET_DIR/lib/modules" -o -path "$TARGET_DIR/lib/firmware" \) -prune -o \
		-type f \( -name '*.a' -o -name '*.la' -o -name '*.o' -o -name '*.prl' \) -exec rm -f {} +
done
# e2fsprogs: keep mke2fs/mkfs.ext* (scratch, hive data) and e2fsck/fsck.ext*;
# drop the rest unless it is a BusyBox applet link.
for n in badblocks debugfs dumpe2fs e2freefrag e2image e2mmpstatus e2undo e4crypt \
	e4defrag filefrag logsave mklost+found resize2fs uuidd compile_et mk_cmds \
	e2scrub e2scrub_all; do
	for d in bin sbin usr/bin usr/sbin; do
		p="$TARGET_DIR/$d/$n"
		if [ -f "$p" ] && [ ! -L "$p" ]; then rm -f "$p"; fi
	done
done

# ---------------------------------------------------------------------------
# 5 + 6. the modloop squashfs, firmware pruned to the allowlist

# The staging tree lives in BUILD_DIR so re-runs (which find the target's
# /lib/modules already emptied) rebuild the same squashfs; "make clean"
# removes it together with the target.
STAGE="$BUILD_DIR/savior-modloop"
mkdir -p "$STAGE/modules" "$STAGE/firmware"
if [ -d "$TARGET_DIR/lib/modules" ]; then
	for d in "$TARGET_DIR"/lib/modules/*; do
		[ -d "$d" ] || continue
		k=$(basename "$d")
		# Buildroot's depmod hook may have written index files into the empty
		# directory we left on a previous run: only real module trees count.
		if [ -n "$(find "$d" -name '*.ko*' -print | head -n 1)" ]; then
			# Modules the kernel we ship could never load (a package built
			# against another kernel): fail rather than drop them quietly.
			[ "$k" = "$KVER" ] || die "target has modules for kernel $k, but this build's kernel is $KVER ($KDIR)"
			rm -rf "${STAGE:?}/modules/$k"
			mv "$d" "$STAGE/modules/$k"
			log "staged kernel modules $k"
		else
			rm -rf "$d"
		fi
	done
fi
# Module trees staged by earlier runs for an older kernel.
for d in "$STAGE"/modules/*; do
	[ -d "$d" ] || continue
	[ "$(basename "$d")" = "$KVER" ] || { rm -rf "$d"; log "dropped stale modules $(basename "$d")"; }
done
[ -d "$STAGE/modules/$KVER" ] || die "no kernel modules for $KVER (did the kernel install its modules?)"
if [ -d "$TARGET_DIR/lib/firmware" ]; then
	# File by file: a package reinstalled later may ship part of a directory
	# another package also installs into (e.g. radeon/ vs amdgpu/ links).
	(cd "$TARGET_DIR/lib/firmware" && find . \( -type f -o -type l \) -print) | sed 's#^\./##' |
		while IFS= read -r rel; do
			mkdir -p "$STAGE/firmware/$(dirname "$rel")"
			rm -rf "${STAGE:?}/firmware/$rel"
			mv "$TARGET_DIR/lib/firmware/$rel" "$STAGE/firmware/$rel"
		done
	find "$TARGET_DIR/lib/firmware" -mindepth 1 -depth -type d -exec rmdir {} + 2>/dev/null || true
fi
# Enforce the allowlist on the merged tree (other packages' firmware, and
# files a changed list no longer names on an incremental rebuild).
sh "$BOARD_DIR/install-firmware.sh" --prune "$STAGE/firmware" "$FW_LIST"
nmods=$(find "$STAGE/modules" -name '*.ko*' | wc -l | tr -d ' ')
[ "$nmods" -gt 0 ] || die "no kernel modules found (is CONFIG_MODULES set, did the kernel install?)"
for k in "$STAGE"/modules/*; do
	[ -f "$k/modules.dep" ] || die "$k/modules.dep missing (depmod did not run)"
done

MKSQUASHFS="$HOST_DIR/bin/mksquashfs"
if [ ! -x "$MKSQUASHFS" ]; then
	MKSQUASHFS=$(command -v mksquashfs || true)
	[ -n "$MKSQUASHFS" ] || die "mksquashfs not found (enable BR2_PACKAGE_HOST_SQUASHFS)"
	log "warning: using the build host's $MKSQUASHFS"
fi
rm -f "$TARGET_DIR/lib/modloop.sqfs"
# 1 MiB blocks: ~16% smaller than the 128K default (the initrd budget counts
# the modloop); reading a module decompresses at most 1 MiB at a time.
SOURCE_DATE_EPOCH=$BUILD_UNIX "$MKSQUASHFS" "$STAGE" "$TARGET_DIR/lib/modloop.sqfs" \
	-comp xz -Xbcj x86 -b 1M -all-root -no-xattrs -noappend -no-progress >/dev/null ||
	die "mksquashfs failed (does it support xz?)"
chmod 0644 "$TARGET_DIR/lib/modloop.sqfs"
# Empty mountpoints. Keep lib/modules/<kver> so Buildroot's depmod hook finds
# its directory on the next run; the modloop mount hides it.
mkdir -p "$TARGET_DIR/lib/modules/$KVER" "$TARGET_DIR/lib/firmware"
log "modloop: $nmods modules, $(find "$STAGE/firmware" \( -type f -o -type l \) | wc -l | tr -d ' ') firmware files -> /lib/modloop.sqfs ($(($(wc -c <"$TARGET_DIR/lib/modloop.sqfs") / 1024)) KiB)"

# ---------------------------------------------------------------------------
# 7. what the overlay's scripts rely on

missing=""
for c in sh mount umount losetup mdev modprobe insmod udhcpc udhcpd ntpd zcip findfs blkid \
	mkfs.vfat fdisk mkswap swapon sysctl hwclock ip start-stop-daemon logger setsid \
	syslogd klogd setpriv timeout pidof uname tr \
	mke2fs dropbear dropbearkey dnsmasq wpa_supplicant wpa_passphrase iw; do
	found=no
	for d in bin sbin usr/bin usr/sbin; do
		if [ -e "$TARGET_DIR/$d/$c" ] || [ -L "$TARGET_DIR/$d/$c" ]; then found=yes; break; fi
	done
	[ "$found" = yes ] || missing="$missing $c"
done
[ -z "$missing" ] || die "missing programs in target:$missing (check busybox.fragment / defconfig)"
[ -f "$TARGET_DIR/etc/ssl/certs/ca-certificates.crt" ] || die "CA bundle missing (BR2_PACKAGE_CA_CERTIFICATES)"
# radeon without amdgpu's CIK support claims GCN 1.1 GPUs, whose firmware
# firmware.list leaves out, and kills the firmware framebuffer on them
# (board/savior/rootfs-overlay/etc/modprobe.d/savior-kernel.conf).
if grep -Eq '^CONFIG_DRM_RADEON=[ym]$' "$KDIR/.config" && ! grep -q '^CONFIG_DRM_AMDGPU_CIK=y$' "$KDIR/.config"; then
	cat "$TARGET_DIR"/etc/modprobe.d/*.conf 2>/dev/null |
		grep -Eq '^[[:space:]]*options[[:space:]]+radeon([[:space:]].*)?[[:space:]]cik_support=0([[:space:]]|$)' ||
		die "radeon is built without amdgpu CIK support, but no /etc/modprobe.d file sets 'options radeon cik_support=0' (is board/savior/rootfs-overlay in BR2_ROOTFS_OVERLAY?)"
fi
suid=$(find "$TARGET_DIR" -type f \( -perm -4000 -o -perm -2000 \) -print | head -n 5)
[ -z "$suid" ] || die "setuid/setgid files in target: $suid"

log "done ($ARCH, $(du -sk --exclude=lib/modloop.sqfs "$TARGET_DIR" 2>/dev/null | cut -f1 || echo '?') KiB excluding the modloop)"
