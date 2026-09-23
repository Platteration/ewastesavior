#!/bin/sh
# os/dev/build.sh - build the SaviorOS dev image from Ubuntu 24.04 packages
# (DESIGN 14 "Dev image"): the Ubuntu generic kernel, the modules listed in
# os/dev/modules.txt (plus dependencies) in /lib/modloop.sqfs, busybox-static,
# os/rootfs-overlay, the savior binary and optionally dropbear, dnsmasq and
# e2fsprogs' mke2fs. Then os/image/mkimage.sh makes the boot media.
#
# Usage:
#   os/dev/build.sh [--out DIR] [--savior PATH] [--kernel-version V]
#                   [--cache DIR] [--no-extras] [--no-media] [--conf FILE]
#                   [--formats img,iso,netboot]
#
#   --out DIR             output directory (default build/dev)
#   --savior PATH         use this savior binary (linux/amd64) instead of
#                         building one from this checkout
#   --kernel-version V    Ubuntu kernel ABI (default 6.8.0-142-generic)
#   --cache DIR           downloaded .debs and extracted kernels
#                         (default build/cache)
#   --no-extras           no dropbear, dnsmasq or e2fsprogs (busybox only)
#   --no-media            stop after vmlinuz + initrd
#   --conf FILE           passed to mkimage.sh --conf (baked config + stick copy)
#   --formats LIST        passed to mkimage.sh (default img,iso,netboot)
#
# Output:
#   DIR/vmlinuz, DIR/initrd       the payload (xz --check=crc32 newc cpio)
#   DIR/savior                    the savior binary inside the image
#   DIR/media/savior.img          USB image (BIOS + UEFI)
#   DIR/media/savior.iso          CD image
#   DIR/media/netboot/            PXE tree
#
# Needs: apt-get + dpkg-deb (Ubuntu/Debian host), depmod (kmod), zstd, xz,
# cpio, mksquashfs (squashfs-tools; installed with apt-get when missing and
# running as root), busybox-static, Go 1.24 (unless --savior), and the
# mkimage.sh tools (GRUB, mtools, dosfstools, xorriso).
set -eu
umask 022

REPO=$(cd "$(dirname "$0")/../.." && pwd)
OUT=$REPO/build/dev
CACHE=$REPO/build/cache
KVER=6.8.0-142-generic
SAVIOR_BIN=""
EXTRAS=yes
MEDIA=yes
CONF=""
FORMATS=img,iso,netboot
MODULES_TXT=$REPO/os/dev/modules.txt

die() { echo "build: error: $*" >&2; exit 1; }
log() { echo "build: $*" >&2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--out) OUT=$2; shift 2 ;;
	--savior) SAVIOR_BIN=$2; shift 2 ;;
	--kernel-version) KVER=$2; shift 2 ;;
	--cache) CACHE=$2; shift 2 ;;
	--no-extras) EXTRAS=no; shift ;;
	--no-media) MEDIA=no; shift ;;
	--conf) CONF=$2; shift 2 ;;
	--formats) FORMATS=$2; shift 2 ;;
	-h | --help) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done
case "$KVER" in
*[!A-Za-z0-9._-]* | "") die "bad --kernel-version: $KVER" ;;
esac
[ -z "$SAVIOR_BIN" ] || [ -f "$SAVIOR_BIN" ] || die "--savior: $SAVIOR_BIN not found"
[ -z "$CONF" ] || [ -f "$CONF" ] || die "--conf: $CONF not found"
[ -f "$MODULES_TXT" ] || die "$MODULES_TXT is missing"

need() { command -v "$1" >/dev/null 2>&1 || die "missing tool: $1 ($2)"; }
need apt-get "apt"
need dpkg-deb "dpkg"
need depmod "kmod"
need zstd "zstd"
need xz "xz-utils"
need cpio "cpio"
if ! command -v mksquashfs >/dev/null 2>&1; then
	if [ "$(id -u)" = 0 ]; then
		log "installing squashfs-tools"
		apt-get install -y squashfs-tools >/dev/null || die "apt-get install squashfs-tools failed"
	fi
	need mksquashfs "squashfs-tools"
fi

mkdir -p "$OUT" "$CACHE/deb"
OUT=$(cd "$OUT" && pwd)
CACHE=$(cd "$CACHE" && pwd)
WORK=$OUT/.work
rm -rf "$WORK"
mkdir -p "$WORK"
ROOT=$WORK/rootfs
ML=$WORK/modloop
START=$(date +%s)

# ---------------------------------------------------------------------------
# Packages

# fetch PKG: download PKG's .deb into the cache (once); print its path.
fetch() {
	_deb=$(find "$CACHE/deb" -maxdepth 1 -name "${1}_*.deb" | sort | tail -n 1)
	if [ -z "$_deb" ]; then
		log "downloading $1"
		(cd "$CACHE/deb" && apt-get download "$1" >/dev/null 2>"$WORK/apt.err") || {
			cat "$WORK/apt.err" >&2
			die "apt-get download $1 failed (apt-get update? is $KVER still in the archive?)"
		}
		_deb=$(find "$CACHE/deb" -maxdepth 1 -name "${1}_*.deb" | sort | tail -n 1)
		[ -n "$_deb" ] || die "apt-get download $1 left no .deb in $CACHE/deb"
	fi
	echo "$_deb"
}

KDIR=$CACHE/kernel-$KVER
if [ ! -e "$KDIR/.done" ]; then
	rm -rf "$KDIR"
	mkdir -p "$KDIR"
	for pkg in "linux-image-unsigned-$KVER" "linux-modules-$KVER" "linux-modules-extra-$KVER"; do
		deb=$(fetch "$pkg")
		log "extracting $(basename "$deb")"
		dpkg-deb -x "$deb" "$KDIR"
	done
	[ -f "$KDIR/boot/vmlinuz-$KVER" ] || die "no boot/vmlinuz-$KVER in linux-image-unsigned-$KVER"
	depmod -b "$KDIR" "$KVER" || die "depmod on the full module tree failed"
	: >"$KDIR/.done"
fi
KMOD=$KDIR/lib/modules/$KVER

# The busybox binary: the host's when it is static, else busybox-static.
BUSYBOX=""
for b in /bin/busybox /usr/bin/busybox; do
	if [ -x "$b" ] && ! ldd "$b" >/dev/null 2>&1; then
		BUSYBOX=$b
		break
	fi
done
if [ -z "$BUSYBOX" ]; then
	bbdir=$CACHE/busybox-static
	if [ ! -x "$bbdir/bin/busybox" ] && [ ! -x "$bbdir/usr/bin/busybox" ]; then
		dpkg-deb -x "$(fetch busybox-static)" "$bbdir"
	fi
	for b in "$bbdir/bin/busybox" "$bbdir/usr/bin/busybox"; do
		[ -x "$b" ] && BUSYBOX=$b
	done
	[ -n "$BUSYBOX" ] || die "no busybox in the busybox-static package"
fi

# ---------------------------------------------------------------------------
# Kernel modules -> modloop

# norm NAME: module names treat - and _ alike.
norm() { printf '%s\n' "$1" | tr - _; }

# Index: "<name> <path>" for every module file, "<name> builtin" for built-ins.
awk -F: '{ p = $1; n = p; sub(/.*\//, "", n); sub(/\.ko(\.(zst|xz|gz))?$/, "", n); gsub(/-/, "_", n); print n, p }' \
	"$KMOD/modules.dep" >"$WORK/mod.index"
awk '{ n = $1; sub(/.*\//, "", n); sub(/\.ko$/, "", n); gsub(/-/, "_", n); print n, "builtin" }' \
	"$KMOD/modules.builtin" >>"$WORK/mod.index"

: >"$WORK/mod.want"
: >"$WORK/mod.report"
missing=""
while read -r line; do
	line=${line%%#*}
	# shellcheck disable=SC2086 # trim blanks
	set -- $line
	[ $# -gt 0 ] || continue
	optional=no
	if [ "$1" = "optional:" ]; then
		optional=yes
		shift
	fi
	[ $# -eq 1 ] || die "modules.txt: bad line: $line"
	found=""
	alts=$1
	while [ -n "$alts" ]; do
		alt=${alts%%|*}
		[ "$alt" = "$alts" ] && alts="" || alts=${alts#*|}
		hit=$(awk -v n="$(norm "$alt")" '$1 == n { print $2; exit }' "$WORK/mod.index")
		if [ -n "$hit" ]; then
			found=$hit
			echo "$alt $hit" >>"$WORK/mod.report"
			[ "$hit" = builtin ] || echo "$hit" >>"$WORK/mod.want"
			break
		fi
	done
	if [ -z "$found" ]; then
		if [ "$optional" = yes ]; then
			echo "$1 absent (optional)" >>"$WORK/mod.report"
		else
			missing="$missing $1"
		fi
	fi
done <"$MODULES_TXT"
[ -z "$missing" ] || die "modules.txt: kernel $KVER has no module or built-in driver named:$missing"

# Dependency closure from modules.dep.
awk -v depfile="$KMOD/modules.dep" '
BEGIN {
	while ((getline l < depfile) > 0) {
		n = split(l, f, /[: ]+/)
		ds = ""
		for (i = 2; i <= n; i++) if (f[i] != "") ds = ds " " f[i]
		deps[f[1]] = ds
	}
}
{ q[++qn] = $1 }
END {
	for (i = 1; i <= qn; i++) {
		m = q[i]
		if (m in seen) continue
		seen[m] = 1
		print m
		k = split(deps[m], dl, " ")
		for (j = 1; j <= k; j++) if (!(dl[j] in seen)) q[++qn] = dl[j]
	}
}' "$WORK/mod.want" >"$WORK/mod.closure.u" || die "module dependency resolution failed"
sort "$WORK/mod.closure.u" >"$WORK/mod.closure"
[ "$(wc -l <"$WORK/mod.closure")" -ge "$(wc -l <"$WORK/mod.want")" ] || die "module dependency resolution lost modules"

MLMOD=$ML/lib/modules/$KVER
mkdir -p "$MLMOD" "$ML/lib/firmware"
while read -r rel; do
	src=$KMOD/$rel
	[ -f "$src" ] || die "module file missing: $src"
	dst=$MLMOD/${rel%.zst}
	dst=${dst%.xz}
	dst=${dst%.gz}
	mkdir -p "$(dirname "$dst")"
	case "$rel" in
	*.zst) zstd -q -d -f -o "$dst" "$src" ;;
	*.xz) xz -dc "$src" >"$dst" ;;
	*.gz) gzip -dc "$src" >"$dst" ;;
	*) cp "$src" "$dst" ;;
	esac
done <"$WORK/mod.closure"
for f in modules.builtin modules.order; do
	[ -f "$KMOD/$f" ] && sed 's/\.ko\.\(zst\|xz\|gz\)$/.ko/' "$KMOD/$f" >"$MLMOD/$f"
done
[ -f "$KMOD/modules.builtin.modinfo" ] && cp "$KMOD/modules.builtin.modinfo" "$MLMOD/"

depmod -b "$ML" "$KVER" || die "depmod on the selected modules failed"
cat >"$ML/lib/firmware/README" <<'EOF'
The SaviorOS dev image ships no firmware. SaviorOS also loads firmware from
/firmware on the stick (firmware_class.path, set by S08config).
EOF
nmods=$(wc -l <"$WORK/mod.closure")
log "modules: $nmods files ($(grep -c ' builtin$' "$WORK/mod.report") listed ones are built in, $(grep -c 'absent' "$WORK/mod.report") optional ones absent)"

# ---------------------------------------------------------------------------
# Root filesystem

mkdir -p "$ROOT"
chmod 0755 "$ROOT"
for d in bin sbin usr/bin usr/sbin usr/lib usr/share lib lib64 etc dev proc sys run tmp mnt \
	var/log var/lib/savior/work var/lib/savior/data media/savior root; do
	mkdir -p "$ROOT/$d"
done

# busybox and its applet links (never over a real file).
cp "$BUSYBOX" "$ROOT/bin/busybox"
chmod 0755 "$ROOT/bin/busybox"
"$BUSYBOX" --list-full | while read -r applet; do
	case "$applet" in "" | /* | *..*) continue ;; esac
	[ -e "$ROOT/$applet" ] || [ -L "$ROOT/$applet" ] && continue
	mkdir -p "$ROOT/$(dirname "$applet")"
	ln -s /bin/busybox "$ROOT/$applet"
done
[ -e "$ROOT/sbin/init" ] || die "busybox has no init applet"

# The overlay, with explicit modes (git keeps only the x bit).
(cd "$REPO/os/rootfs-overlay" && find . -type d) | while read -r d; do
	mkdir -p "$ROOT/$d"
	chmod 0755 "$ROOT/$d"
done
(cd "$REPO/os/rootfs-overlay" && find . ! -type d) | while read -r f; do
	src=$REPO/os/rootfs-overlay/$f
	rm -f "$ROOT/$f"
	if [ -L "$src" ]; then
		cp -P "$src" "$ROOT/$f"
	elif [ -x "$src" ]; then
		cp "$src" "$ROOT/$f"
		chmod 0755 "$ROOT/$f"
	else
		cp "$src" "$ROOT/$f"
		chmod 0644 "$ROOT/$f"
	fi
done
chmod 0600 "$ROOT/etc/shadow"
# The kernel runs /init: the overlay's (re-roots the initramfs, then starts
# BusyBox init), else BusyBox init itself.
[ -e "$ROOT/init" ] || ln -s /bin/busybox "$ROOT/init"
[ -e "$ROOT/etc/mtab" ] || ln -s /proc/mounts "$ROOT/etc/mtab"
: >"$ROOT/etc/resolv.conf"
chmod 0644 "$ROOT/etc/resolv.conf"
chmod 0700 "$ROOT/root" "$ROOT/media/savior" "$ROOT/var/lib/savior/data"
chmod 0711 "$ROOT/var/lib/savior/work"
chmod 1777 "$ROOT/tmp"

# copy_bin SRC DEST: a dynamically linked program plus its libraries.
copy_bin() {
	mkdir -p "$ROOT$(dirname "$2")"
	rm -f "$ROOT$2"
	cp "$1" "$ROOT$2"
	chmod 0755 "$ROOT$2"
	ldd "$1" 2>/dev/null | awk '$2 == "=>" && $3 ~ /^\// { print $3 } $1 ~ /^\// { print $1 }' |
		while read -r lib; do
			[ -e "$ROOT$lib" ] && continue
			mkdir -p "$ROOT$(dirname "$lib")"
			cp -L "$lib" "$ROOT$lib"
			chmod 0755 "$ROOT$lib"
		done
}

if [ "$EXTRAS" = yes ]; then
	for spec in dropbear:/usr/sbin/dropbear:dropbear-bin dropbearkey:/usr/bin/dropbearkey:dropbear-bin \
		dnsmasq:/usr/sbin/dnsmasq:dnsmasq-base mke2fs:/usr/sbin/mke2fs:e2fsprogs; do
		prog=${spec%%:*}
		rest=${spec#*:}
		dest=${rest%%:*}
		pkg=${rest#*:}
		src=""
		for p in "/usr/sbin/$prog" "/usr/bin/$prog" "/sbin/$prog" "/bin/$prog"; do
			if [ -x "$p" ]; then
				src=$p
				break
			fi
		done
		if [ -z "$src" ]; then
			log "warning: $prog not found on this host (apt install $pkg); leaving it out"
			continue
		fi
		copy_bin "$src" "$dest"
	done
	if [ -x "$ROOT/usr/sbin/mke2fs" ]; then
		ln -sf mke2fs "$ROOT/usr/sbin/mkfs.ext4"
		[ -f /etc/mke2fs.conf ] && cp /etc/mke2fs.conf "$ROOT/etc/mke2fs.conf" && chmod 0644 "$ROOT/etc/mke2fs.conf"
	fi
fi

# The savior binary.
GITREV=$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null) || GITREV=unknown
VERSION=dev-$GITREV
if [ -n "$SAVIOR_BIN" ]; then
	cp "$SAVIOR_BIN" "$WORK/savior"
	v=$("$SAVIOR_BIN" version 2>/dev/null | awk '{ print $2 }') || v=""
	[ -z "$v" ] || VERSION=$v
else
	GO=$(command -v go 2>/dev/null) || GO=/usr/local/go/bin/go
	[ -x "$GO" ] || die "go not found (install Go 1.24 or pass --savior)"
	pkg=github.com/platteration/ewastesavior/internal/version
	log "building savior $VERSION"
	(cd "$REPO" && GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
		"$GO" build -trimpath \
		-ldflags "-s -w -X $pkg.Version=$VERSION -X $pkg.BuildUnix=$(date +%s)" \
		-o "$WORK/savior" ./cmd/savior) || die "go build failed (pass --savior PATH to use a prebuilt binary)"
fi
cp "$WORK/savior" "$ROOT/usr/bin/savior"
chmod 0755 "$ROOT/usr/bin/savior"
cp "$WORK/savior" "$OUT/savior"
chmod 0755 "$OUT/savior"

printf '%s (dev image, Ubuntu kernel %s)\n' "$VERSION" "$KVER" >"$ROOT/etc/savior-release"
chmod 0644 "$ROOT/etc/savior-release"

# The modloop (DESIGN 13.2): modules/ and firmware/ at the top level.
mksquashfs "$ML/lib/modules" "$ML/lib/firmware" "$ROOT/lib/modloop.sqfs" \
	-comp xz -Xbcj x86 -b 256K -noappend -all-root -no-progress -quiet >/dev/null ||
	die "mksquashfs failed"
chmod 0644 "$ROOT/lib/modloop.sqfs"

# ---------------------------------------------------------------------------
# initramfs: newc cpio (root-owned) + /dev/console, xz with CRC32

# cpio_node NAME MODE MAJOR MINOR INO: a newc entry for a device node (made
# here because mknod may be forbidden on the build host).
cpio_node() {
	printf '070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%s\000' \
		"$5" "$2" 0 0 1 0 0 0 0 "$3" "$4" $((${#1} + 1)) 0 "$1"
	_pad=$(((4 - (110 + ${#1} + 1) % 4) % 4))
	while [ "$_pad" -gt 0 ]; do
		printf '\000'
		_pad=$((_pad - 1))
	done
}
{
	(cd "$ROOT" && find . -mindepth 1 | LC_ALL=C sort | cpio -o -H newc -R 0:0 --quiet)
	cpio_node dev/console $((0020600)) 5 1 900001
	cpio_node dev/null $((0020666)) 1 3 900002
	cpio_node TRAILER!!! 0 0 0 0
} >"$WORK/initrd.cpio"
log "compressing the initramfs (xz -9, this takes a while)"
xz --check=crc32 -9 -T1 -c "$WORK/initrd.cpio" >"$OUT/initrd.tmp"
mv -f "$OUT/initrd.tmp" "$OUT/initrd"
cp "$KDIR/boot/vmlinuz-$KVER" "$OUT/vmlinuz"

# ---------------------------------------------------------------------------
# Sizes and the DESIGN 13.2 budget (x86_64); warnings only in the dev image.

mib() { awk -v b="$1" 'BEGIN { printf "%.1f MiB", b / 1048576 }'; }
initrd_b=$(wc -c <"$OUT/initrd")
modloop_b=$(wc -c <"$ROOT/lib/modloop.sqfs")
rootfs_b=$(find "$ROOT" -type f ! -path "$ROOT/lib/modloop.sqfs" -exec wc -c {} + | awk '$2 != "total" { s += $1 } END { print s + 0 }')
savior_b=$(wc -c <"$ROOT/usr/bin/savior")
log "vmlinuz $(mib "$(wc -c <"$OUT/vmlinuz")"), initrd $(mib "$initrd_b") (xz), unpacked rootfs $(mib "$rootfs_b") without the modloop, modloop $(mib "$modloop_b"), savior $(mib "$savior_b")"
over=no
if [ "$initrd_b" -gt $((32 * 1048576)) ]; then
	log "WARNING: initrd exceeds the x86_64 budget of 32 MiB"
	over=yes
fi
if [ "$rootfs_b" -gt $((64 * 1048576)) ]; then
	log "WARNING: unpacked rootfs exceeds the x86_64 budget of 64 MiB"
	over=yes
fi
[ "$over" = yes ] || log "within the DESIGN 13.2 x86_64 budget (initrd <= 32 MiB, rootfs <= 64 MiB)"

# ---------------------------------------------------------------------------
# Boot media

if [ "$MEDIA" = yes ]; then
	set -- --out "$OUT/media" --payload "x86_64=$OUT/vmlinuz,$OUT/initrd" --version "$VERSION" --formats "$FORMATS"
	[ -z "$CONF" ] || set -- "$@" --conf "$CONF"
	sh "$REPO/os/image/mkimage.sh" "$@" || die "mkimage.sh failed"
	ls -l "$OUT/media" >&2
fi
rm -rf "$WORK"
log "done in $(($(date +%s) - START)) s: $OUT"
