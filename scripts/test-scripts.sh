#!/bin/sh
# test-scripts.sh - self-tests for the image build scripts, runnable anywhere
# (no Buildroot, no network): check-kconfig.sh, check-defconfig.sh, the
# defconfigs' board scripts, install-firmware.sh, fetch-buildroot.sh (file://
# mirror), post-fakeroot.sh, post-build.sh and post-image.sh (fake Buildroot
# trees; skipped without mksquashfs), flash-usb.sh (fake sysfs, never
# writes), qemu-smoke.sh with the images workflow's expectations (fake QEMU)
# and S50sshd (in a chroot; only as root, with a static busybox).
#
# Usage: scripts/test-scripts.sh     (exit status 1 if any test fails)
set -eu

case "${1:-}" in
-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
esac
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
BOARD="$ROOT/os/buildroot/board/savior"
T=$(mktemp -d "${TMPDIR:-/tmp}/savior-test-scripts.XXXXXX")
trap 'rm -rf "$T"' EXIT INT TERM
fails=0
passes=0

ok() { passes=$((passes + 1)); echo "ok   $*"; }
bad() { fails=$((fails + 1)); echo "FAIL $*"; }
# expect RC NAME CMD...: CMD exits with RC (output in $T/out).
expect() {
	want=$1 name=$2
	shift 2
	set +e
	"$@" >"$T/out" 2>&1
	rc=$?
	set -e
	if [ "$rc" -eq "$want" ]; then ok "$name"; else bad "$name (exit $rc, want $want)"; sed 's/^/     | /' "$T/out"; fi
}
# contains TEXT NAME: the last command's output contains TEXT.
contains() {
	if grep -q -F -- "$1" "$T/out"; then ok "$2"; else bad "$2 (no '$1' in output)"; sed 's/^/     | /' "$T/out"; fi
}

# --- check-kconfig ------------------------------------------------------------
cat >"$T/req" <<'EOF'
# comment
[all]
CONFIG_A=y
CONFIG_B=y|m
# CONFIG_C is not set
# CONFIG_D is not set
CONFIG_S="hello world"
[x86_64]
CONFIG_X=y
[i686]
CONFIG_I=y
EOF
cat >"$T/kc" <<'EOF'
CONFIG_64BIT=y
CONFIG_A=y
CONFIG_B=m
# CONFIG_C is not set
CONFIG_S="hello world"
CONFIG_X=y
EOF
expect 0 "check-kconfig: all met (x86_64 detected)" sh "$HERE/check-kconfig.sh" --required "$T/req" "$T/kc"
expect 1 "check-kconfig: i686 section enforced" sh "$HERE/check-kconfig.sh" --arch i686 --required "$T/req" "$T/kc"
contains "FAIL CONFIG_I" "check-kconfig: reports the missing symbol"
sed 's/^CONFIG_B=m$/CONFIG_B=n/; s/^# CONFIG_C is not set$/CONFIG_C=y/' "$T/kc" >"$T/kc2"
expect 1 "check-kconfig: y|m and not-set violations" sh "$HERE/check-kconfig.sh" --required "$T/req" "$T/kc2"
contains "FAIL CONFIG_B" "check-kconfig: y|m failure listed"
contains "FAIL CONFIG_C" "check-kconfig: not-set failure listed"
sed 's/hello world/hello/' "$T/kc" >"$T/kc3"
expect 1 "check-kconfig: string mismatch" sh "$HERE/check-kconfig.sh" --required "$T/req" "$T/kc3"
printf '[all]\nCONFIG_A\n' >"$T/badreq"
expect 2 "check-kconfig: syntax error" sh "$HERE/check-kconfig.sh" --required "$T/badreq" "$T/kc"
for a in x86_64 i686; do
	# The shipped list parses and its sections are known.
	sh "$HERE/check-kconfig.sh" --arch "$a" -q "$T/kc" >"$T/out" 2>&1 || true
	contains "requirements checked for $a" "check-kconfig: required.txt parses for $a"
done

# --- check-defconfig --------------------------------------------------------------
cat >"$T/defconfig" <<'EOF'
BR2_x86_64=y
BR2_STR="$(BR2_EXTERNAL_SAVIOR_PATH)/x"
# BR2_OFF is not set
# BR2_GONE is not set
EOF
cat >"$T/brconfig" <<'EOF'
BR2_x86_64=y
BR2_STR="$(BR2_EXTERNAL_SAVIOR_PATH)/x"
# BR2_OFF is not set
EOF
expect 0 "check-defconfig: all survive" sh "$HERE/check-defconfig.sh" --config "$T/brconfig" "$T/defconfig"
contains "warn BR2_GONE" "check-defconfig: absent 'not set' symbol is a warning"
expect 1 "check-defconfig: --strict fails on it" sh "$HERE/check-defconfig.sh" --strict --config "$T/brconfig" "$T/defconfig"
printf 'BR2_TYPO=y\n' >>"$T/defconfig"
expect 1 "check-defconfig: dropped symbol fails" sh "$HERE/check-defconfig.sh" --config "$T/brconfig" "$T/defconfig"
contains "FAIL BR2_TYPO" "check-defconfig: names the dropped symbol"
for d in "$ROOT"/os/buildroot/configs/*_defconfig; do
	# Every defconfig line is parseable, no symbol twice.
	if sh "$HERE/check-defconfig.sh" --config "$d" "$d" >"$T/out" 2>&1; then
		ok "check-defconfig: $(basename "$d") is well-formed"
	else
		bad "check-defconfig: $(basename "$d") is well-formed"
		sed 's/^/     | /' "$T/out"
	fi
	# Buildroot executes the post scripts directly: they must exist and be
	# executable. The post-fakeroot script is what keeps busybox's 4755 out
	# of the initrd (post-image would reject it).
	for v in BR2_ROOTFS_POST_BUILD_SCRIPT BR2_ROOTFS_POST_FAKEROOT_SCRIPT BR2_ROOTFS_POST_IMAGE_SCRIPT BR2_ROOTFS_OVERLAY; do
		val=$(sed -n "s/^$v=\"\(.*\)\"\$/\1/p" "$d")
		okv=yes
		[ -n "$val" ] || okv=no
		for f in $val; do
			# shellcheck disable=SC2016 # a literal $(BR2_EXTERNAL_SAVIOR_PATH)
			f=$(printf '%s\n' "$f" | sed 's#^\$(BR2_EXTERNAL_SAVIOR_PATH)#'"$ROOT"'/os/buildroot#')
			case "$v" in
			BR2_ROOTFS_OVERLAY) [ -d "$f" ] || okv=no ;;
			*) [ -f "$f" ] && [ -x "$f" ] || okv=no ;;
			esac
		done
		if [ "$okv" = yes ]; then ok "defconfig: $(basename "$d") $v exists"; else bad "defconfig: $(basename "$d") $v exists ('$val')"; fi
	done
	case "$(sed -n 's/^BR2_ROOTFS_POST_FAKEROOT_SCRIPT="\(.*\)"$/\1/p' "$d")" in
	*/board/savior/post-fakeroot.sh) ok "defconfig: $(basename "$d") runs post-fakeroot.sh" ;;
	*) bad "defconfig: $(basename "$d") runs post-fakeroot.sh" ;;
	esac
	case " $(sed -n 's/^BR2_ROOTFS_OVERLAY="\(.*\)"$/\1/p' "$d") " in
	*" \$(BR2_EXTERNAL_SAVIOR_PATH)/board/savior/rootfs-overlay "*) ok "defconfig: $(basename "$d") has the board overlay" ;;
	*) bad "defconfig: $(basename "$d") has the board overlay" ;;
	esac
done
# The production kernel builds radeon without amdgpu, so radeon must be told
# to leave CIK GPUs alone (post-build.sh enforces it against the real .config).
if grep -q '^CONFIG_DRM_RADEON=m$' "$BOARD/linux/display.config" &&
	grep -q '^# CONFIG_DRM_AMDGPU is not set$' "$BOARD/linux/display.config" &&
	grep -Eq '^options[[:space:]]+radeon[[:space:]]+cik_support=0$' "$BOARD/rootfs-overlay/etc/modprobe.d/savior-kernel.conf"; then
	ok "board overlay: radeon cik_support=0 for a kernel without amdgpu"
else bad "board overlay: radeon cik_support=0 for a kernel without amdgpu"; fi

# --- install-firmware ----------------------------------------------------------------
FW="$T/fw"
mkdir -p "$FW/intel/iwlwifi" "$FW/radeon" "$FW/amdgpu" "$FW/rtl_nic"
echo a >"$FW/intel/iwlwifi/iwlwifi-6000-4.ucode"
echo b >"$FW/radeon/R100_cp.bin"
echo c >"$FW/radeon/BONAIRE_ce.bin"
echo d >"$FW/radeon/RV710_uvd.bin"
echo e >"$FW/amdgpu/tahiti_mc.bin"
echo f >"$FW/amdgpu/navi10_sos.bin"
echo g >"$FW/rtl_nic/rtl8168d-1.fw"
echo h >"$FW/rt2860.bin"
cat >"$FW/WHENCE" <<'EOF'
File: intel/iwlwifi/iwlwifi-6000-4.ucode
Link: iwlwifi-6000-4.ucode -> intel/iwlwifi/iwlwifi-6000-4.ucode
Link: radeon/tahiti_mc.bin -> ../amdgpu/tahiti_mc.bin
Link: rt3090.bin -> rt2860.bin
EOF
cat >"$T/fwlist" <<'EOF'
iwlwifi-6000-4.ucode
radeon/*
!radeon/BONAIRE*
!radeon/*_uvd.bin
rtl_nic/*
rt2860.bin
?rt9999.bin
EOF
expect 0 "install-firmware: install" sh "$BOARD/install-firmware.sh" "$FW" "$T/fwout" "$T/fwlist"
if [ -f "$T/fwout/intel/iwlwifi/iwlwifi-6000-4.ucode" ] && [ -L "$T/fwout/iwlwifi-6000-4.ucode" ] &&
	[ "$(cat "$T/fwout/iwlwifi-6000-4.ucode")" = a ]; then
	ok "install-firmware: moved file installed via its WHENCE link"
else bad "install-firmware: moved file installed via its WHENCE link"; fi
if [ -f "$T/fwout/amdgpu/tahiti_mc.bin" ] && [ ! -e "$T/fwout/amdgpu/navi10_sos.bin" ] &&
	[ "$(cat "$T/fwout/radeon/tahiti_mc.bin")" = e ]; then
	ok "install-firmware: link target outside the pattern's directory"
else bad "install-firmware: link target outside the pattern's directory"; fi
if [ -f "$T/fwout/radeon/R100_cp.bin" ] && [ ! -e "$T/fwout/radeon/BONAIRE_ce.bin" ] && [ ! -e "$T/fwout/radeon/RV710_uvd.bin" ]; then
	ok "install-firmware: exclusions"
else bad "install-firmware: exclusions"; fi
if [ -L "$T/fwout/rt3090.bin" ]; then ok "install-firmware: links to installed files are created"; else bad "install-firmware: links to installed files are created"; fi
printf 'nothing-matches-*.bin\n' >"$T/fwlist2"
expect 1 "install-firmware: a required pattern without match fails" sh "$BOARD/install-firmware.sh" "$FW" "$T/fwout2" "$T/fwlist2"
echo x >"$T/fwout/stray.bin"
echo r >"$T/fwout/regulatory.db"
expect 0 "install-firmware: prune" sh "$BOARD/install-firmware.sh" --prune "$T/fwout" "$T/fwlist"
if [ ! -e "$T/fwout/stray.bin" ] && [ -f "$T/fwout/regulatory.db" ] && [ -f "$T/fwout/amdgpu/tahiti_mc.bin" ]; then
	ok "install-firmware: prune keeps the allowlist and regulatory.db"
else bad "install-firmware: prune keeps the allowlist and regulatory.db"; fi
# The shipped list parses: every pattern compiles (pruning an empty directory).
mkdir -p "$T/empty-fw"
expect 0 "install-firmware: shipped firmware.list parses" \
	sh "$BOARD/install-firmware.sh" --prune "$T/empty-fw" "$BOARD/firmware.list"

# --- fetch-buildroot -------------------------------------------------------------------
if command -v curl >/dev/null 2>&1 && command -v xz >/dev/null 2>&1; then
	M="$T/mirror"
	mkdir -p "$M/src/buildroot-9999.01"
	echo 'all:' >"$M/src/buildroot-9999.01/Makefile"
	(cd "$M/src" && tar -cJf "$M/buildroot-9999.01.tar.xz" buildroot-9999.01)
	sum=$( (sha256sum "$M/buildroot-9999.01.tar.xz" 2>/dev/null || shasum -a 256 "$M/buildroot-9999.01.tar.xz") | cut -d' ' -f1)
	printf 'MD5: 0 buildroot-9999.01.tar.xz\nSHA256: %s  buildroot-9999.01.tar.xz\n' "$sum" >"$M/buildroot-9999.01.tar.xz.sign"
	: >"$T/nohash"
	expect 0 "fetch-buildroot: verified by the .sign manifest" env BUILDROOT_MIRRORS="file://$M" \
		sh "$HERE/fetch-buildroot.sh" --version 9999.01 --dest "$T/br" --dl-dir "$T/dl" --hash-file "$T/nohash"
	if [ -f "$T/br/Makefile" ]; then ok "fetch-buildroot: unpacked"; else bad "fetch-buildroot: unpacked"; fi
	expect 0 "fetch-buildroot: re-run is a no-op" env BUILDROOT_MIRRORS="file://$M" \
		sh "$HERE/fetch-buildroot.sh" --version 9999.01 --dest "$T/br" --dl-dir "$T/dl" --hash-file "$T/nohash"
	contains "already in" "fetch-buildroot: reports the existing tree"
	expect 1 "fetch-buildroot: BUILDROOT_REQUIRE_PIN refuses an unpinned tarball" env BUILDROOT_MIRRORS="file://$M" BUILDROOT_REQUIRE_PIN=1 \
		sh "$HERE/fetch-buildroot.sh" --version 9999.01 --dest "$T/br2" --dl-dir "$T/dl" --hash-file "$T/nohash"
	printf 'sha256  %s  buildroot-9999.01.tar.xz\n' "$sum" >"$T/pin"
	expect 0 "fetch-buildroot: pinned hash" env BUILDROOT_MIRRORS="file://$M" BUILDROOT_REQUIRE_PIN=1 \
		sh "$HERE/fetch-buildroot.sh" --version 9999.01 --dest "$T/br3" --dl-dir "$T/dl" --hash-file "$T/pin"
	printf 'sha256  %s  buildroot-9999.01.tar.xz\n' 0000000000000000000000000000000000000000000000000000000000000000 >"$T/pin"
	expect 1 "fetch-buildroot: wrong pinned hash fails" env BUILDROOT_MIRRORS="file://$M" \
		sh "$HERE/fetch-buildroot.sh" --version 9999.01 --dest "$T/br4" --dl-dir "$T/dl" --hash-file "$T/pin"
	if [ ! -e "$T/br4" ]; then ok "fetch-buildroot: nothing unpacked after a mismatch"; else bad "fetch-buildroot: nothing unpacked after a mismatch"; fi
else
	echo "skip fetch-buildroot tests (need curl and xz)"
fi

# --- post-fakeroot ------------------------------------------------------------------------
# Buildroot's makedevs applies busybox's "/bin/busybox f 4755 0 0" permission
# entry inside fakeroot, after post-build.sh has checked the tree.
PF="$T/pf"
mkdir -p "$PF/bin" "$PF/usr/bin"
: >"$PF/bin/busybox"
chmod 4755 "$PF/bin/busybox"
ln -s busybox "$PF/bin/sh"
expect 0 "post-fakeroot: runs (executable, as Buildroot calls it)" "$BOARD/post-fakeroot.sh" "$PF" x86_64
if [ -n "$(find "$PF/bin/busybox" -perm 0755 -print)" ]; then
	ok "post-fakeroot: /bin/busybox 4755 -> 0755"
else bad "post-fakeroot: /bin/busybox 4755 -> 0755 ($(ls -l "$PF/bin/busybox"))"; fi
: >"$PF/usr/bin/sneaky"
chmod 2755 "$PF/usr/bin/sneaky"
expect 1 "post-fakeroot: fails on any other setgid file" "$BOARD/post-fakeroot.sh" "$PF" x86_64
contains "usr/bin/sneaky" "post-fakeroot: names it"

# --- post-build ---------------------------------------------------------------------------
# elf CLASS MACHINE INTERP OUT: a minimal ELF header + one program header
# (PT_LOAD, or PT_INTERP when INTERP=yes). Enough for post-build's checks.
# shellcheck disable=SC2059 # the format string is the octal escape
oct() { printf "\\$(printf '%03o' "$1")"; }
le() { n=$1; i=0; while [ "$i" -lt "$2" ]; do oct $((n & 255)); n=$((n >> 8)); i=$((i + 1)); done; }
elf() {
	ptype=1
	[ "$3" = yes ] && ptype=3
	{
		printf '\177ELF'; oct "$1"; oct 1; oct 1; le 0 9
		le 2 2; le "$2" 2; le 1 4
		if [ "$1" = 2 ]; then
			le 0 8; le 64 8; le 0 8; le 0 4; le 64 2; le 56 2; le 1 2; le 0 6
			le "$ptype" 4; le 0 52
		else
			le 0 4; le 52 4; le 0 4; le 0 4; le 52 2; le 32 2; le 1 2; le 0 6
			le "$ptype" 4; le 0 28
		fi
	} >"$4"
}
if command -v mksquashfs >/dev/null 2>&1; then
	B="$T/br-fake"
	mkdir -p "$B/host/bin" "$B/build/linux-6.12.40/include/config" "$B/target" "$B/ov/etc/init.d" "$B/bin/linux-amd64" "$B/bin/linux-386"
	ln -s "$(command -v mksquashfs)" "$B/host/bin/mksquashfs"
	echo 'CONFIG_X86=y' >"$B/build/linux-6.12.40/.config"
	echo '6.12.40-savior' >"$B/build/linux-6.12.40/include/config/kernel.release"
	printf '# Buildroot 2025.02.5 Configuration\nBR2_ARCH="x86_64"\nBR2_LINUX_KERNEL_VERSION="6.12.40"\n' >"$B/br.config"
	tg="$B/target"
	mkdir -p "$tg/bin" "$tg/sbin" "$tg/usr/bin" "$tg/usr/sbin" "$tg/etc/init.d" "$tg/etc/ssl/certs" \
		"$tg/lib/modules/6.12.40-savior/kernel" "$tg/lib/firmware/rtl_nic" "$tg/lib/firmware/junk" "$tg/usr/share/man"
	for c in sh mount umount losetup mdev modprobe insmod udhcpc udhcpd ntpd zcip findfs blkid mkfs.vfat fdisk mkswap \
		swapon sysctl hwclock ip start-stop-daemon logger setsid syslogd klogd setpriv timeout mke2fs dropbear dropbearkey wpa_passphrase \
		dnsmasq wpa_supplicant iw debugfs pidof uname tr; do
		: >"$tg/usr/sbin/$c"
	done
	echo certs >"$tg/etc/ssl/certs/ca-certificates.crt"
	echo 'root:*:1:0:99999:7:::' >"$tg/etc/shadow"
	echo '::sysinit:/etc/init.d/rcS' >"$B/ov/etc/inittab"
	cp "$B/ov/etc/inittab" "$tg/etc/inittab"
	for s in S00mounts S05mdev; do printf '#!/bin/sh\n' >"$B/ov/etc/init.d/$s"; cp "$B/ov/etc/init.d/$s" "$tg/etc/init.d/$s"; done
	printf '#!/bin/sh\n' >"$tg/etc/init.d/S50dropbear"
	# Buildroot's dropbear package: /etc/dropbear -> /var/run/dropbear (dangles).
	ln -s /var/run/dropbear "$tg/etc/dropbear"
	echo ko >"$tg/lib/modules/6.12.40-savior/kernel/e1000.ko"
	echo 'kernel/e1000.ko:' >"$tg/lib/modules/6.12.40-savior/modules.dep"
	echo fw >"$tg/lib/firmware/rtl_nic/rtl8168d-1.fw"
	echo junk >"$tg/lib/firmware/junk/x.bin"
	elf 2 62 no "$B/bin/linux-amd64/savior"
	printf 'build\tGOAMD64=v1\n' >>"$B/bin/linux-amd64/savior"
	cp -pR "$B" "$T/br-bump" # pristine copy for the kernel-bump tests below
	pb() {
		env HOST_DIR="$B/host" BUILD_DIR="$B/build" BR2_CONFIG="$B/br.config" SAVIOR_OVERLAY="$B/ov" \
			SAVIOR_VERSION=v9.9.9 SAVIOR_BUILD_UNIX=1790000000 "$@"
	}
	expect 0 "post-build: x86_64 fake target" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	if [ -x "$tg/usr/bin/savior" ]; then ok "post-build: savior installed"; else bad "post-build: savior installed"; fi
	if grep -q '^VERSION="v9.9.9"$' "$tg/etc/savior-release"; then ok "post-build: savior-release"; else bad "post-build: savior-release"; fi
	if [ ! -e "$tg/etc/init.d/S50dropbear" ] && [ -f "$tg/etc/init.d/S00mounts" ]; then ok "post-build: init allowlist"; else bad "post-build: init allowlist"; fi
	if [ -f "$tg/lib/modloop.sqfs" ] && [ -d "$tg/lib/modules" ] && [ -z "$(ls -A "$tg/lib/firmware")" ]; then
		ok "post-build: modloop built, mountpoints left"
	else bad "post-build: modloop built, mountpoints left"; fi
	if [ ! -e "$tg/usr/share/man" ] && [ ! -e "$tg/usr/sbin/debugfs" ]; then ok "post-build: stripping"; else bad "post-build: stripping"; fi
	if [ -d "$tg/etc/dropbear" ] && [ ! -L "$tg/etc/dropbear" ] && [ -n "$(find "$tg/etc/dropbear" -prune -type d -perm 0700)" ]; then
		ok "post-build: dangling /etc/dropbear link replaced by a 0700 directory"
	else bad "post-build: dangling /etc/dropbear link replaced by a 0700 directory ($(ls -ld "$tg/etc/dropbear"))"; fi
	if grep -q '^KERNEL="6.12.40-savior"$' "$tg/etc/savior-release"; then ok "post-build: kernel release"; else bad "post-build: kernel release"; fi
	if command -v unsquashfs >/dev/null 2>&1; then
		unsquashfs -l "$tg/lib/modloop.sqfs" >"$T/out" 2>&1
		if grep -q 'squashfs-root/modules/6.12.40-savior/kernel/e1000.ko' "$T/out" &&
			grep -q 'squashfs-root/firmware/rtl_nic/rtl8168d-1.fw' "$T/out" && ! grep -q junk "$T/out"; then
			ok "post-build: modloop layout (modules/, firmware/, pruned)"
		else bad "post-build: modloop layout (modules/, firmware/, pruned)"; sed 's/^/     | /' "$T/out"; fi
	fi
	cp "$tg/lib/modloop.sqfs" "$T/modloop.1"
	# A reinstalled dropbear package runs "ln -snf" onto the directory.
	ln -snf /var/run/dropbear "$tg/etc/dropbear" 2>/dev/null || true
	expect 0 "post-build: second run" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	if cmp -s "$T/modloop.1" "$tg/lib/modloop.sqfs"; then ok "post-build: idempotent (same modloop)"; else bad "post-build: idempotent (same modloop)"; fi
	if [ -d "$tg/etc/dropbear" ] && [ ! -L "$tg/etc/dropbear" ] && [ ! -e "$tg/etc/dropbear/dropbear" ] && [ ! -L "$tg/etc/dropbear/dropbear" ]; then
		ok "post-build: /etc/dropbear stays a clean directory after a package reinstall"
	else bad "post-build: /etc/dropbear stays a clean directory after a package reinstall"; fi
	# radeon built without amdgpu's CIK support needs cik_support=0.
	echo 'CONFIG_DRM_RADEON=m' >>"$B/build/linux-6.12.40/.config"
	expect 1 "post-build: radeon without amdgpu CIK needs 'options radeon cik_support=0'" \
		pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	contains "cik_support=0" "post-build: names the missing radeon option"
	cp -R "$BOARD/rootfs-overlay/." "$tg/"
	expect 0 "post-build: the board overlay's modprobe.d file satisfies it" \
		pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	# A package reinstalled later with part of a directory: merged file by file.
	mkdir -p "$tg/lib/firmware/rtl_nic"
	echo fw2 >"$tg/lib/firmware/rtl_nic/rtl8168e-1.fw"
	expect 0 "post-build: run after a partial firmware reinstall" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	if command -v unsquashfs >/dev/null 2>&1; then
		unsquashfs -l "$tg/lib/modloop.sqfs" >"$T/out" 2>&1
		if grep -q 'firmware/rtl_nic/rtl8168d-1.fw' "$T/out" && grep -q 'firmware/rtl_nic/rtl8168e-1.fw' "$T/out"; then
			ok "post-build: firmware merged file by file"
		else bad "post-build: firmware merged file by file"; sed 's/^/     | /' "$T/out"; fi
	fi
	elf 2 62 yes "$B/bin/linux-amd64/savior"
	expect 1 "post-build: rejects a dynamic binary" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	elf 2 62 no "$B/bin/linux-amd64/savior"
	printf 'build\tGOAMD64=v3\n' >>"$B/bin/linux-amd64/savior"
	expect 1 "post-build: rejects GOAMD64=v3" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	elf 1 3 no "$B/bin/linux-386/savior-sse2"
	elf 2 62 no "$B/bin/linux-386/savior-softfloat"
	expect 1 "post-build: rejects a 64-bit binary for i686" pb SAVIOR_BIN_DIR="$B/bin/linux-386" sh "$BOARD/post-build.sh" "$tg" i686
	elf 1 3 no "$B/bin/linux-386/savior-softfloat"
	expect 0 "post-build: i686 installs both 386 builds" pb SAVIOR_BIN_DIR="$B/bin/linux-386" sh "$BOARD/post-build.sh" "$tg" i686
	if [ -x "$tg/usr/bin/savior-sse2" ] && [ -x "$tg/usr/bin/savior-softfloat" ] && [ ! -e "$tg/usr/bin/savior" ]; then
		ok "post-build: i686 binary names"
	else bad "post-build: i686 binary names"; fi
	echo 'root::1:0:99999:7:::' >"$tg/etc/shadow"
	expect 1 "post-build: rejects an empty root password" pb SAVIOR_BIN_DIR="$B/bin/linux-386" sh "$BOARD/post-build.sh" "$tg" i686

	# post-image on the i686 target above.
	if command -v cpio >/dev/null 2>&1 && command -v xz >/dev/null 2>&1; then
		img="$B/images"
		mkdir -p "$img" "$tg/sbin"
		printf 'kernel' >"$img/bzImage"
		printf '#!/bin/sh\n' >"$tg/init"
		[ -e "$tg/sbin/init" ] || : >"$tg/sbin/init"
		(cd "$tg" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -9 -C crc64 >"$img/rootfs.cpio.xz")
		pi() { env TARGET_DIR="$tg" BUILD_DIR="$B/build" HOST_DIR="$B/host" BR2_CONFIG="$B/br.config" SAVIOR_MKIMAGE=no "$@"; }
		expect 0 "post-image: i686 payload" pi sh "$BOARD/post-image.sh" "$img" i686
		p="$img/payload/i686"
		if cmp -s "$p/kernel.config" "$B/build/linux-6.12.40/.config"; then ok "post-image: kernel.config"; else bad "post-image: kernel.config"; fi
		if [ -f "$p/vmlinuz" ] && [ -f "$p/initrd" ] && [ -f "$p/SHA256SUMS" ] && grep -q '^arch=i686$' "$p/budget.txt"; then
			ok "post-image: payload files"
		else bad "post-image: payload files"; fi
		if xz --robot --list "$p/initrd" | awk -F'\t' '$1 == "file" && $7 == "CRC32" { f = 1 } END { exit !f }' &&
			grep -q '^xz_dict_mib=8$' "$p/budget.txt"; then
			ok "post-image: CRC64 initrd recompressed as CRC32 with a small dictionary"
		else bad "post-image: CRC64 initrd recompressed as CRC32 with a small dictionary"; fi
		# The rootfs as makedevs leaves it (busybox 4755): post-image refuses
		# it, and post-fakeroot.sh (which Buildroot runs before the cpio)
		# makes it acceptable.
		mkdir -p "$tg/bin"
		: >"$tg/bin/busybox"
		chmod 4755 "$tg/bin/busybox"
		(cd "$tg" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -C crc32 >"$img/rootfs.cpio.xz")
		expect 1 "post-image: rejects a setuid busybox (makedevs without post-fakeroot)" pi sh "$BOARD/post-image.sh" "$img" i686
		contains "bin/busybox" "post-image: names the setuid file"
		expect 0 "post-image: post-fakeroot.sh on the makedevs tree" "$BOARD/post-fakeroot.sh" "$tg" i686
		(cd "$tg" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -C crc32 >"$img/rootfs.cpio.xz")
		expect 0 "post-image: accepts it after post-fakeroot.sh" pi sh "$BOARD/post-image.sh" "$img" i686
		rm -f "$tg/init"
		(cd "$tg" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -C crc32 >"$img/rootfs.cpio.xz")
		expect 1 "post-image: rejects an initrd without /init" pi sh "$BOARD/post-image.sh" "$img" i686
	else
		echo "skip post-image tests (need cpio and xz)"
	fi

	# Kernel bump on an existing build tree: Buildroot keeps build/linux-<old>
	# next to build/linux-<new>; the kernel BR2_CONFIG names must win.
	K="$T/br-bump"
	kt="$K/target"
	pk() {
		env HOST_DIR="$K/host" BUILD_DIR="$K/build" BR2_CONFIG="$K/br.config" SAVIOR_OVERLAY="$K/ov" \
			SAVIOR_VERSION=v9.9.9 SAVIOR_BUILD_UNIX=1790000000 SAVIOR_BIN_DIR="$K/bin/linux-amd64" "$@"
	}
	expect 0 "kernel bump: build with 6.12.40" pk sh "$BOARD/post-build.sh" "$kt" x86_64
	mkdir -p "$K/build/linux-6.12.41/include/config" "$kt/lib/modules/6.12.41-savior/kernel"
	printf 'CONFIG_X86=y\n# 6.12.41\n' >"$K/build/linux-6.12.41/.config"
	echo '6.12.41-savior' >"$K/build/linux-6.12.41/include/config/kernel.release"
	echo ko >"$kt/lib/modules/6.12.41-savior/kernel/e1000.ko"
	echo 'kernel/e1000.ko:' >"$kt/lib/modules/6.12.41-savior/modules.dep"
	printf '# Buildroot 2025.02.5 Configuration\nBR2_ARCH="x86_64"\nBR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE="6.12.41"\nBR2_LINUX_KERNEL_VERSION="6.12.41"\n' >"$K/br.config"
	expect 0 "kernel bump: rebuild with 6.12.41" pk sh "$BOARD/post-build.sh" "$kt" x86_64
	contains "dropped stale modules 6.12.40-savior" "kernel bump: the old kernel's modules are dropped"
	if grep -q '^KERNEL="6.12.41-savior"$' "$kt/etc/savior-release"; then
		ok "kernel bump: savior-release names the new kernel"
	else bad "kernel bump: savior-release names the new kernel ($(grep KERNEL "$kt/etc/savior-release"))"; fi
	if command -v unsquashfs >/dev/null 2>&1; then
		unsquashfs -l "$kt/lib/modloop.sqfs" >"$T/out" 2>&1
		if grep -q 'squashfs-root/modules/6.12.41-savior/kernel/e1000.ko' "$T/out" && ! grep -q '6\.12\.40' "$T/out"; then
			ok "kernel bump: the modloop has only the new kernel's modules"
		else bad "kernel bump: the modloop has only the new kernel's modules"; sed 's/^/     | /' "$T/out"; fi
	fi
	if command -v cpio >/dev/null 2>&1 && command -v xz >/dev/null 2>&1; then
		mkdir -p "$K/images" "$kt/sbin"
		printf 'kernel' >"$K/images/bzImage"
		printf '#!/bin/sh\n' >"$kt/init"
		[ -e "$kt/sbin/init" ] || : >"$kt/sbin/init"
		(cd "$kt" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -C crc32 >"$K/images/rootfs.cpio.xz")
		expect 0 "kernel bump: post-image" env TARGET_DIR="$kt" BUILD_DIR="$K/build" HOST_DIR="$K/host" \
			BR2_CONFIG="$K/br.config" SAVIOR_MKIMAGE=no sh "$BOARD/post-image.sh" "$K/images" x86_64
		if cmp -s "$K/images/payload/x86_64/kernel.config" "$K/build/linux-6.12.41/.config"; then
			ok "kernel bump: kernel.config (checked by check-kconfig) is the new kernel's"
		else bad "kernel bump: kernel.config (checked by check-kconfig) is the new kernel's"; fi
	fi
	mkdir -p "$kt/lib/modules/6.1.0-other/kernel"
	echo ko >"$kt/lib/modules/6.1.0-other/kernel/foo.ko"
	expect 1 "kernel bump: modules for another kernel in the target fail the build" pk sh "$BOARD/post-build.sh" "$kt" x86_64
	contains "modules for kernel 6.1.0-other" "kernel bump: names that kernel"
	rm -rf "$kt/lib/modules/6.1.0-other"
	printf '# Buildroot 2025.02.5 Configuration\nBR2_ARCH="x86_64"\n' >"$K/br.config"
	expect 1 "kernel bump: two kernel dirs and no version in BR2_CONFIG: no guessing" pk sh "$BOARD/post-build.sh" "$kt" x86_64
	contains "several kernel build directories" "kernel bump: says why"
else
	echo "skip post-build tests (need mksquashfs: apt install squashfs-tools)"
fi

# --- flash-usb (fake sysfs; its test mode never writes) -----------------------------------
FR="$T/flash"
mkdir -p "$FR/sys/block" "$FR/sys/class/block" "$FR/proc" "$FR/dev/mapper"
# fake_disk NAME DEVDIR SECTORS REMOVABLE: a whole disk at sys/devices/DEVDIR/NAME.
fake_disk() {
	mkdir -p "$FR/sys/devices/$2/$1/device"
	echo "$3" >"$FR/sys/devices/$2/$1/size"
	echo "$4" >"$FR/sys/devices/$2/$1/removable"
	echo "Fake $1" >"$FR/sys/devices/$2/$1/device/model"
	ln -s "../devices/$2/$1" "$FR/sys/block/$1"
	ln -s "../../devices/$2/$1" "$FR/sys/class/block/$1"
	: >"$FR/dev/$1"
}
# fake_part DISK PART DEVDIR: partition PART of DISK.
fake_part() {
	mkdir -p "$FR/sys/devices/$3/$1/$2"
	echo 1 >"$FR/sys/devices/$3/$1/$2/partition"
	ln -s "../../devices/$3/$1/$2" "$FR/sys/class/block/$2"
	: >"$FR/dev/$2"
}
# fake_slave DEV PART DEVDIR DISK: virtual DEV sits on PART (of DISK in DEVDIR).
fake_slave() {
	mkdir -p "$FR/sys/devices/virtual/block/$1/slaves"
	ln -s "../../../../$3/$4/$2" "$FR/sys/devices/virtual/block/$1/slaves/$2"
}
ATA=pci0000:00/ata1/host0/target0:0:0/0:0:0:0/block
USB=pci0000:00/usb1/1-1/host6/target6:0:0/6:0:0:0/block
fake_disk nvme0n1 pci0000:00/nvme 1000215216 0
fake_part nvme0n1 nvme0n1p3 pci0000:00/nvme
fake_disk sdc "$ATA" 976773168 0
fake_part sdc sdc1 "$ATA"
fake_disk sdb "$USB" 31260672 0
fake_part sdb sdb1 "$USB"
# Root on LVM/LUKS (dm-1 on nvme0n1p3), /srv on md RAID (md127 on sdc1), and
# a stacked device whose name no list knows.
fake_disk dm-1 virtual/block 900000000 0
fake_slave dm-1 nvme0n1p3 pci0000:00/nvme nvme0n1
fake_disk md127 virtual/block 900000000 0
fake_slave md127 sdc1 "$ATA" sdc
fake_disk stack0 virtual/block 900000000 0
fake_slave stack0 sdc1 "$ATA" sdc
ln -s ../dm-1 "$FR/dev/mapper/vg-root"
printf '/dev/dm-1 / ext4 rw 0 0\n/dev/md127 /srv ext4 rw 0 0\nproc /proc proc rw 0 0\n' >"$FR/proc/mounts"
printf 'Filename\tType\tSize\tUsed\tPriority\n' >"$FR/proc/swaps"
dd if=/dev/zero of="$T/flash.img" bs=1024 count=64 2>/dev/null
printf '\125\252' | dd of="$T/flash.img" bs=1 seek=510 conv=notrunc 2>/dev/null
fu() { env FLASH_USB_TEST_ROOT="$FR" sh "$HERE/flash-usb.sh" "$@"; }
expect 1 "flash-usb: refuses /dev/mapper/<root> (dm, mounted root) even with --force" \
	fu --force --yes "$T/flash.img" "$FR/dev/mapper/vg-root"
contains "virtual or stacked" "flash-usb: says it is a virtual or stacked device"
expect 1 "flash-usb: refuses /dev/md127 (mounted RAID) even with --force" fu --force --yes "$T/flash.img" "$FR/dev/md127"
expect 1 "flash-usb: refuses any device that sits on other disks" fu --force --yes "$T/flash.img" "$FR/dev/stack0"
expect 1 "flash-usb: refuses the disk under the root filesystem even with --force" fu --force --yes "$T/flash.img" "$FR/dev/nvme0n1"
contains "holds the running system" "flash-usb: says why"
expect 1 "flash-usb: refuses a disk under a mounted md array" fu --force --yes "$T/flash.img" "$FR/dev/sdc"
expect 1 "flash-usb: refuses a partition" fu --yes "$T/flash.img" "$FR/dev/sdb1"
expect 0 "flash-usb: accepts a USB stick (test mode: checks only)" fu --yes "$T/flash.img" "$FR/dev/sdb"
contains "checks passed for sdb" "flash-usb: test mode stops after the checks"
printf '/dev/dm-1 / ext4 rw 0 0\n/dev/sdb1 /media/stick vfat rw 0 0\n' >"$FR/proc/mounts"
expect 1 "flash-usb: refuses a mounted stick without --unmount" fu --yes "$T/flash.img" "$FR/dev/sdb"
expect 0 "flash-usb: a fixed disk is accepted with --force" fu --force --yes "$T/flash.img" "$FR/dev/sdc"
expect 1 "flash-usb: ... and refused without it" fu --yes "$T/flash.img" "$FR/dev/sdc"
expect 0 "flash-usb: --list" fu --list
if grep -q '/dev/sdb ' "$T/out" && grep -q 'SYSTEM DISK' "$T/out" && ! grep -q -E 'dm-1|md127|stack0' "$T/out"; then
	ok "flash-usb: --list shows real disks only"
else bad "flash-usb: --list shows real disks only"; sed 's/^/     | /' "$T/out"; fi

# --- qemu-smoke with the images workflow's expectations (fake QEMU) -----------------------
# The fake writes the lines of $FAKE_SERIAL to QEMU's -serial file ("sleep N"
# pauses, "exit" ends it), then idles like a VM that keeps running.
cat >"$T/fake-qemu" <<'EOF'
#!/bin/sh
log=""
while [ $# -gt 0 ]; do
	case "$1" in -serial) log=${2#file:}; shift 2 ;; *) shift ;; esac
done
[ -n "$log" ] || exit 1
while IFS= read -r line; do
	case "$line" in
	sleep\ *) sleep "${line#sleep }" ;;
	exit) exit 0 ;;
	*) printf '%s\r\n' "$line" >>"$log" ;;
	esac
done <"$FAKE_SERIAL"
exec sleep 60
EOF
chmod 0755 "$T/fake-qemu"
: >"$T/fake.img"
BOOT_EXPECT=$(sed -n 's/^  BOOT_EXPECT: //p' "$ROOT/.github/workflows/images.yml")
BOOT_MARKER=$(sed -n 's/^  BOOT_MARKER: "\(.*\)"$/\1/p' "$ROOT/.github/workflows/images.yml")
if [ -n "$BOOT_EXPECT" ] && [ -n "$BOOT_MARKER" ]; then
	ok "images.yml: BOOT_MARKER and BOOT_EXPECT found"
else bad "images.yml: BOOT_MARKER and BOOT_EXPECT found"; fi
# smoke SERIAL-TEXT QEMU-SMOKE-ARGS...: qemu-smoke on the fake, with the CI's flags.
smoke() {
	printf '%s\n' "$1" >"$T/fake-serial"
	shift
	# shellcheck disable=SC2086 # BOOT_EXPECT is a list of arguments, as in images.yml
	env QEMU="$T/fake-qemu" FAKE_SERIAL="$T/fake-serial" sh "$HERE/qemu-smoke.sh" --arch i686 --image "$T/fake.img" \
		--accel tcg --timeout 20 --marker "$BOOT_MARKER" $BOOT_EXPECT "$@"
}
BL="SAVIOR-BOOT: rcS done up=31.2 cfg=yes media=/dev/sda1 key=no fb=yes net=10.0.2.15 console=yes node=yes ver=v1.2.3 t=net:9,fb:3,console:4"
AL="SAVIOR-AGENT: up=44.0 agent=up age=10 starts=1 arch=i686 ver=v1.2.3"
NL='
'
expect 0 "qemu-smoke: a good boot passes the CI checks" smoke "$BL${NL}sleep 1${NL}$AL" --expect arch=i686
contains "SAVIOR-AGENT: up=44.0 agent=up" "qemu-smoke: prints the status lines"
expect 1 "qemu-smoke: node=yes but savior crashes (ver=unknown, SIGILL) fails" \
	smoke "$(echo "$BL" | sed 's/ver=v1.2.3/ver=unknown/')${NL}SIGILL: illegal instruction" --expect arch=i686
expect 1 "qemu-smoke: node=yes but the agent keeps restarting (agent=crashing) fails" \
	smoke "$BL${NL}SAVIOR-AGENT: up=40.1 agent=crashing age=- starts=2 arch=i686 ver=v1.2.3" --expect arch=i686
contains "agent=crashing" "qemu-smoke: names the failure"
expect 1 "qemu-smoke: no NIC (net=none) fails" \
	smoke "$(echo "$BL" | sed 's/net=10.0.2.15/net=none/')${NL}$AL" --expect arch=i686
expect 1 "qemu-smoke: the wrong payload (arch) fails" \
	smoke "$BL${NL}$(echo "$AL" | sed 's/arch=i686/arch=x86_64/')${NL}exit" --expect arch=i686
contains "'arch=i686'" "qemu-smoke: names the missing string"
expect 1 "qemu-smoke: no SAVIOR-AGENT line fails" smoke "$BL${NL}exit" --expect arch=i686
contains "'agent=up'" "qemu-smoke: names agent=up"

# --- S50sshd in a chroot: Buildroot's dangling /etc/dropbear link ------------------------
BB=$(command -v busybox 2>/dev/null || true)
CH=""
if [ "$(id -u)" = 0 ] && command -v chroot >/dev/null 2>&1; then
	CH=chroot
elif command -v unshare >/dev/null 2>&1 && command -v chroot >/dev/null 2>&1 && unshare -r true 2>/dev/null; then
	CH="unshare -r chroot"
fi
C="$T/sshd-root"
if [ -n "$BB" ] && [ -n "$CH" ]; then
	mkdir -p "$C/bin" "$C/usr/sbin" "$C/etc/init.d" "$C/usr/libexec/savior" "$C/run/savior" "$C/var/log" "$C/dev" "$C/tmp" "$C/root"
	cp "$BB" "$C/bin/busybox"
	for a in $("$BB" --list 2>/dev/null); do
		case "$a" in */* | busybox) continue ;; esac
		[ -e "$C/bin/$a" ] || ln -s busybox "$C/bin/$a"
	done
fi
# shellcheck disable=SC2086 # CH is a command with arguments
if [ -n "$BB" ] && [ -n "$CH" ] && $CH "$C" /bin/sh -c 'exit 0' 2>/dev/null; then
	cp "$ROOT/os/rootfs-overlay/usr/libexec/savior/lib.sh" "$C/usr/libexec/savior/lib.sh"
	cp "$ROOT/os/rootfs-overlay/etc/init.d/S50sshd" "$C/etc/init.d/S50sshd"
	# Fakes: dropbearkey -t ed25519 -f FILE writes FILE.tmp and renames it,
	# as the real one does; dropbear records its arguments.
	cat >"$C/usr/sbin/dropbearkey" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -f) f=$2; shift 2 ;; *) shift ;; esac; done
echo key >"$f.tmp" && mv "$f.tmp" "$f"
EOF
	printf '#!/bin/sh\necho "$*" >/tmp/dropbear.args\n' >"$C/usr/sbin/dropbear"
	chmod 0755 "$C/usr/sbin/dropbearkey" "$C/usr/sbin/dropbear"
	echo "SAVIOR_SSH_KEY='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test'" >"$C/run/savior/env"
	# As Buildroot's dropbear package installs it; /run is a fresh tmpfs.
	mkdir -p "$C/var"
	ln -s ../run "$C/var/run"
	ln -s /var/run/dropbear "$C/etc/dropbear"
	# shellcheck disable=SC2086
	expect 0 "S50sshd: start in a chroot" $CH "$C" /bin/sh /etc/init.d/S50sshd start
	if [ -d "$C/etc/dropbear" ] && [ ! -L "$C/etc/dropbear" ] && [ -s "$C/etc/dropbear/dropbear_ed25519_host_key" ] &&
		grep -q -- '-r /etc/dropbear/dropbear_ed25519_host_key' "$C/tmp/dropbear.args" 2>/dev/null; then
		ok "S50sshd: host key made and dropbear started despite the dangling /etc/dropbear link"
	else
		bad "S50sshd: host key made and dropbear started despite the dangling /etc/dropbear link"
		sed 's/^/     | /' "$C/var/log/savior-init.log" 2>/dev/null || true
	fi
else
	echo "skip S50sshd test (needs root or unprivileged user namespaces, and a static busybox)"
fi

# --- S08config reprobe_wifi -------------------------------------------------------------
# Fake sysfs: b43legacy (SSB) and b43 (BCMA) cores bound without a netdev,
# a p54usb dongle that released its interface without firmware, and the
# cases that must be left alone. The function is run from the real S08config
# with /sys moved under $T and modprobe/log stubbed.
S08="$ROOT/os/rootfs-overlay/etc/init.d/S08config"
fs="$T/wifi"
mkdir -p "$fs/sys/module/ssb" "$fs/sys/module/b43legacy" "$fs/sys/module/bcma" "$fs/sys/module/b43" \
	"$fs/sys/module/iwl3945"
# wdev DIR DRIVER MODULE: a device node bound to DRIVER of MODULE ("" = unbound,
# or built in).
wdev() {
	mkdir -p "$fs/sys/bus/$1"
	: >"$fs/sys/bus/$1/uevent"
	# As in the kernel: PCI, SSB and USB interface nodes have modalias,
	# BCMA cores and USB device nodes do not.
	case $1 in pci/*|ssb/*|usb/devices/*:*) : >"$fs/sys/bus/$1/modalias" ;; esac
	if [ -n "$2" ]; then
		d="$fs/sys/bus/${1%%/*}/drivers/$2"
		mkdir -p "$d"
		[ -n "$3" ] && [ ! -e "$d/module" ] && ln -s "$fs/sys/module/$3" "$d/module"
		ln -s "$d" "$fs/sys/bus/$1/driver"
	fi
}
# BCM4306: the PCI function belongs to ssb (b43-pci-bridge), b43legacy has
# the SSB core.
wdev pci/devices/0000:02:00.0 b43-pci-bridge ssb
echo 0x028000 >"$fs/sys/bus/pci/devices/0000:02:00.0/class"
wdev ssb/devices/ssb0:1 b43legacy b43legacy
# BCM4331: bcma-pci-bridge, b43 on the BCMA core (which has no modalias).
wdev pci/devices/0000:03:00.0 bcma-pci-bridge bcma
echo 0x028000 >"$fs/sys/bus/pci/devices/0000:03:00.0/class"
wdev bcma/devices/bcma0:1 b43 b43
# A working Intel card (has net/) and an unbound PCI wireless controller.
wdev pci/devices/0000:04:00.0 iwl3945 iwl3945
echo 0x028000 >"$fs/sys/bus/pci/devices/0000:04:00.0/class"
mkdir -p "$fs/sys/bus/pci/devices/0000:04:00.0/net/wlan0"
wdev pci/devices/0000:05:00.0 "" ""
echo 0x028000 >"$fs/sys/bus/pci/devices/0000:05:00.0/class"
# USB: the p54usb interface released itself (unbound, vendor class ff); an
# unbound HID interface and the device nodes must not be probed.
wdev usb/devices/1-1 usb ""
wdev usb/devices/1-1:1.0 "" ""
echo ff >"$fs/sys/bus/usb/devices/1-1:1.0/bInterfaceClass"
wdev usb/devices/1-2:1.0 "" ""
echo 03 >"$fs/sys/bus/usb/devices/1-2:1.0/bInterfaceClass"
: >"$fs/sys/bus/pci/drivers_probe"
: >"$fs/sys/bus/usb/drivers_probe"
# shellcheck disable=SC2016 # the stubs expand when the generated script runs
{
	echo 'log() { :; }'
	echo 'modprobe() { echo "modprobe $*" >>"$fs/modprobe.log"; }'
	awk '/^WIFI_MODULES=/, /"$/ { print } /^reprobe_wifi\(\) \{/, /^}/ { print }' "$S08" |
		sed -e 's|/sys/|@SYS@/|g' -e 's|>@SYS@|>>@SYS@|g' -e "s|@SYS@|$fs/sys|g"
	echo reprobe_wifi
} >"$T/reprobe.sh"
: >"$fs/modprobe.log"
expect 0 "reprobe_wifi: runs on a fake sysfs" env fs="$fs" sh "$T/reprobe.sh"
for m in b43legacy b43; do
	if grep -qx "modprobe -r $m" "$fs/modprobe.log" && grep -qx "modprobe $m" "$fs/modprobe.log"; then
		ok "reprobe_wifi: reloads $m bound on the SSB/BCMA bus without a netdev"
	else bad "reprobe_wifi: reloads $m bound on the SSB/BCMA bus without a netdev"; sed 's/^/     | /' "$fs/modprobe.log"; fi
done
if grep -q -e ssb -e bcma -e iwl3945 "$fs/modprobe.log"; then
	bad "reprobe_wifi: leaves ssb, bcma and a working card alone"; sed 's/^/     | /' "$fs/modprobe.log"
else ok "reprobe_wifi: leaves ssb, bcma and a working card alone"; fi
if [ "$(cat "$fs/sys/bus/usb/drivers_probe")" = 1-1:1.0 ]; then
	ok "reprobe_wifi: probes the released vendor-specific USB interface (only)"
else bad "reprobe_wifi: probes the released vendor-specific USB interface (only)"; sed 's/^/     | /' "$fs/sys/bus/usb/drivers_probe"; fi
if [ "$(cat "$fs/sys/bus/pci/drivers_probe")" = 0000:05:00.0 ]; then
	ok "reprobe_wifi: probes the unbound PCI wireless controller (only)"
else bad "reprobe_wifi: probes the unbound PCI wireless controller (only)"; sed 's/^/     | /' "$fs/sys/bus/pci/drivers_probe"; fi

echo "test-scripts: $passes passed, $fails failed"
[ "$fails" -eq 0 ]
