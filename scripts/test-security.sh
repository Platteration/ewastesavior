#!/bin/sh
# Offline regressions for firmware hash coverage and private image creation.
# Image tools are fakes: this checks the real assembler's permission/lifecycle
# behavior, not media format correctness or bootability.
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
T=$(mktemp -d "${TMPDIR:-/tmp}/savior-security.XXXXXX")
trap 'rm -rf "$T"' EXIT INT TERM
fail() { echo "test-security: $*" >&2; exit 1; }

# Evaluate the real package with a minimal upstream Buildroot definition.
mkdir -p "$T/pkg/package/savior-firmware"
cp "$ROOT/os/buildroot/package/savior-firmware/"* "$T/pkg/package/savior-firmware/"
cat >"$T/Makefile" <<EOF2
BR2_EXTERNAL_SAVIOR_PATH = $T/pkg
BR2_PACKAGE_SAVIOR_FIRMWARE = y
LINUX_FIRMWARE_VERSION = 20250211
LINUX_FIRMWARE_SOURCE = linux-firmware-\$(LINUX_FIRMWARE_VERSION).tar.xz
LINUX_FIRMWARE_SITE = https://example.invalid
include $T/pkg/package/savior-firmware/savior-firmware.mk
all:
	@true
EOF2
make -sf "$T/Makefile" || fail 'reviewed firmware hashes rejected'
if make -sf "$T/Makefile" LINUX_FIRMWARE_VERSION=next >"$T/out" 2>&1; then
	fail 'unknown firmware archive accepted without hash'
fi
grep -q 'missing SHA-256' "$T/out" || fail 'missing archive hash not reported'
sed '/WHENCE$/d' "$T/pkg/package/savior-firmware/savior-firmware.hash" >"$T/hash"
cp "$T/hash" "$T/pkg/package/savior-firmware/savior-firmware.hash"
if make -sf "$T/Makefile" >"$T/out" 2>&1; then fail 'missing WHENCE hash accepted'; fi
grep -q 'missing WHENCE' "$T/out" || fail 'missing license hash not reported'
echo 'ok firmware hash coverage fails closed'

# Fake only the imaging utilities. dd remains real and records output modes
# before forwarding the first write; xorriso records its first-write mode too.
mkdir -p "$T/bin" "$T/grub/i386-pc" "$T/grub/x86_64-efi" "$T/output"
printf '%0440d' 0 >"$T/grub/i386-pc/boot.img"
printf boot >"$T/grub/i386-pc/boot_hybrid.img"
printf kernel >"$T/kernel"
printf initrd >"$T/initrd"
printf 'swarm_key = dummy-secret-for-permission-regression\n' >"$T/conf"
chmod 0600 "$T/conf"
cat >"$T/bin/grub-mkimage" <<'STUB'
#!/bin/sh
set -eu
while [ $# -gt 0 ]; do
	case "$1" in -o) dest=$2; shift 2 ;; *) shift ;; esac
done
printf core >"$dest"
STUB
cat >"$T/bin/mkfs.fat" <<'STUB'
#!/bin/sh
set -eu
if [ "$1" = -C ]; then : >"$2"; fi
STUB
cat >"$T/bin/mcopy" <<'STUB'
#!/bin/sh
set -eu
while [ $# -gt 0 ]; do
	case "$1" in -i) dest=$2; shift 2 ;; *) shift ;; esac
done
cat "$SECURITY_TEST_CONF" >>"$dest"
STUB
cat >"$T/bin/mmd" <<'STUB'
#!/bin/sh
exit 0
STUB
cat >"$T/bin/cpio" <<'STUB'
#!/bin/sh
cat >/dev/null
cat etc/savior/baked.conf
STUB
cat >"$T/bin/dd" <<'STUB'
#!/bin/sh
set -eu
dest=""
for arg in "$@"; do case "$arg" in of=*) dest=${arg#of=} ;; esac; done
if [ -n "$dest" ]; then
	[ -e "$dest" ] || : >"$dest"
	printf 'dd %s %s\n' "$(stat -c %a "$dest")" "$dest" >>"$SECURITY_TEST_MODES"
fi
exec /bin/dd "$@"
STUB
cat >"$T/bin/xorriso" <<'STUB'
#!/bin/sh
set -eu
if [ "$1" = -indev ]; then
	echo 'MBR partition : 1 0x80 '
	exit 0
fi
while [ $# -gt 0 ]; do
	case "$1" in -o) dest=$2; shift 2 ;; *) shift ;; esac
done
: >"$dest"
printf 'xorriso %s %s\n' "$(stat -c %a "$dest")" "$dest" >>"$SECURITY_TEST_MODES"
cat "$SECURITY_TEST_CONF" >>"$dest"
STUB
# Keep optional host GRUB utilities from affecting the mocked-media tests.
cat >"$T/bin/grub-script-check" <<'STUB'
#!/bin/sh
exit 0
STUB
cat >"$T/bin/grub-mknetdir" <<'STUB'
#!/bin/sh
set -eu
while [ $# -gt 0 ]; do
	case "$1" in --net-directory=*) dest=${1#*=} ;; -d) plat=${2##*/}; shift ;; esac
	shift
done
mkdir -p "$dest/boot/grub/$plat"
printf net >"$dest/boot/grub/$plat/core.0"
STUB
chmod 0755 "$T/bin/"*
chmod 0755 "$T/output"
(
	umask 022
	PATH="$T/bin:$PATH" SECURITY_TEST_CONF="$T/conf" SECURITY_TEST_MODES="$T/modes" \
		sh "$ROOT/os/image/mkimage.sh" --out "$T/output" --conf "$T/conf" \
		--formats img,iso --ipxe none --grub-lib "$T/grub" \
		--payload "x86_64=$T/kernel,$T/initrd" >"$T/build.log" 2>&1
) || { cat "$T/build.log" >&2; fail 'mocked image build failed'; }
[ "$(stat -c %a "$T/output")" = 700 ] || fail 'secret output directory is not private'
for ext in img iso; do
	f="$T/output/savior.$ext"
	[ "$(stat -c %a "$f")" = 600 ] || fail "$ext final artifact is not private"
	grep -a -q 'dummy-secret-for-permission-regression' "$f" || fail "$ext fake failed to carry dummy config"
done
[ "$(wc -l <"$T/modes")" -ge 9 ] || fail 'first-write permission observations missing'
awk '$2 != "600" { exit 1 }' "$T/modes" || fail 'secret-bearing temporary output was readable before writing'
[ -z "$(find "$T/output" -name '.savior-artifacts.*' -print)" ] || fail 'artifact staging directory leaked'
echo 'ok IMG/ISO first writes and completed artifacts are private under umask 022'
