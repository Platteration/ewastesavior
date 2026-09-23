#!/bin/sh
# test-scripts.sh - self-tests for the image build scripts, runnable anywhere
# (no Buildroot, no network): check-kconfig.sh, check-defconfig.sh,
# install-firmware.sh, fetch-buildroot.sh (file:// mirror) and post-build.sh
# (fake Buildroot tree; skipped without mksquashfs).
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
done

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
	printf '# Buildroot 2025.02.5 Configuration\nBR2_ARCH="x86_64"\n' >"$B/br.config"
	tg="$B/target"
	mkdir -p "$tg/bin" "$tg/sbin" "$tg/usr/bin" "$tg/usr/sbin" "$tg/etc/init.d" "$tg/etc/ssl/certs" \
		"$tg/lib/modules/6.12.40-savior/kernel" "$tg/lib/firmware/rtl_nic" "$tg/lib/firmware/junk" "$tg/usr/share/man"
	for c in sh mount umount losetup mdev modprobe insmod udhcpc udhcpd ntpd zcip findfs blkid mkfs.vfat fdisk mkswap \
		swapon sysctl hwclock ip start-stop-daemon logger setsid syslogd klogd setpriv timeout mke2fs dropbear dropbearkey wpa_passphrase \
		dnsmasq wpa_supplicant iw debugfs; do
		: >"$tg/usr/sbin/$c"
	done
	echo certs >"$tg/etc/ssl/certs/ca-certificates.crt"
	echo 'root:*:1:0:99999:7:::' >"$tg/etc/shadow"
	echo '::sysinit:/etc/init.d/rcS' >"$B/ov/etc/inittab"
	cp "$B/ov/etc/inittab" "$tg/etc/inittab"
	for s in S00mounts S05mdev; do printf '#!/bin/sh\n' >"$B/ov/etc/init.d/$s"; cp "$B/ov/etc/init.d/$s" "$tg/etc/init.d/$s"; done
	printf '#!/bin/sh\n' >"$tg/etc/init.d/S50dropbear"
	echo ko >"$tg/lib/modules/6.12.40-savior/kernel/e1000.ko"
	echo 'kernel/e1000.ko:' >"$tg/lib/modules/6.12.40-savior/modules.dep"
	echo fw >"$tg/lib/firmware/rtl_nic/rtl8168d-1.fw"
	echo junk >"$tg/lib/firmware/junk/x.bin"
	elf 2 62 no "$B/bin/linux-amd64/savior"
	printf 'build\tGOAMD64=v1\n' >>"$B/bin/linux-amd64/savior"
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
	if command -v unsquashfs >/dev/null 2>&1; then
		unsquashfs -l "$tg/lib/modloop.sqfs" >"$T/out" 2>&1
		if grep -q 'squashfs-root/modules/6.12.40-savior/kernel/e1000.ko' "$T/out" &&
			grep -q 'squashfs-root/firmware/rtl_nic/rtl8168d-1.fw' "$T/out" && ! grep -q junk "$T/out"; then
			ok "post-build: modloop layout (modules/, firmware/, pruned)"
		else bad "post-build: modloop layout (modules/, firmware/, pruned)"; sed 's/^/     | /' "$T/out"; fi
	fi
	cp "$tg/lib/modloop.sqfs" "$T/modloop.1"
	expect 0 "post-build: second run" pb SAVIOR_BIN_DIR="$B/bin/linux-amd64" sh "$BOARD/post-build.sh" "$tg" x86_64
	if cmp -s "$T/modloop.1" "$tg/lib/modloop.sqfs"; then ok "post-build: idempotent (same modloop)"; else bad "post-build: idempotent (same modloop)"; fi
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
		if [ -f "$p/vmlinuz" ] && [ -f "$p/initrd" ] && [ -f "$p/SHA256SUMS" ] && grep -q '^arch=i686$' "$p/budget.txt"; then
			ok "post-image: payload files"
		else bad "post-image: payload files"; fi
		if xz --robot --list "$p/initrd" | awk -F'\t' '$1 == "file" && $7 == "CRC32" { f = 1 } END { exit !f }' &&
			grep -q '^xz_dict_mib=8$' "$p/budget.txt"; then
			ok "post-image: CRC64 initrd recompressed as CRC32 with a small dictionary"
		else bad "post-image: CRC64 initrd recompressed as CRC32 with a small dictionary"; fi
		rm -f "$tg/init"
		(cd "$tg" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | xz -C crc32 >"$img/rootfs.cpio.xz")
		expect 1 "post-image: rejects an initrd without /init" pi sh "$BOARD/post-image.sh" "$img" i686
	else
		echo "skip post-image tests (need cpio and xz)"
	fi
else
	echo "skip post-build tests (need mksquashfs: apt install squashfs-tools)"
fi

echo "test-scripts: $passes passed, $fails failed"
[ "$fails" -eq 0 ]
