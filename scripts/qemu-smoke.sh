#!/bin/sh
# qemu-smoke.sh - boot a SaviorOS payload or boot medium in QEMU and wait for
# a marker on the serial console. Used by CI for the Buildroot images (the
# dev-image tests are os/dev/qemu-test.sh).
#
# Usage:
#   scripts/qemu-smoke.sh [options] (--payload-dir DIR | --image FILE | --iso FILE)
#
#   --payload-dir DIR   direct kernel boot of DIR/vmlinuz + DIR/initrd
#   --image FILE        boot a USB disk image (attached as a USB stick on EHCI)
#   --iso FILE          boot an ISO as a CD-ROM
#   --arch A            x86_64 (default) or i686: qemu-system-x86_64 / -i386
#   --firmware F        bios (default, SeaBIOS), uefi (OVMF x64) or uefi32
#                       (OVMF ia32; $OVMF_CODE / $OVMF32_CODE override paths)
#   --cpu MODEL         QEMU CPU model, e.g. "pentium3,-pae" (default: QEMU's)
#   --mem MB            guest RAM (default 512)
#   --smp N             guest CPUs (default 1)
#   --accel A           auto (default: kvm when usable, else tcg), kvm or tcg.
#                       Use tcg to test CPU feature limits: KVM runs SSE2
#                       instructions even when the model hides the flag.
#   --append ARGS       extra kernel arguments (--payload-dir only)
#   --marker TEXT       success string on the serial console (default "SAVIOR-BOOT: rcS done",
#                       which the rootfs overlay prints when the boot scripts finish)
#   --expect TEXT       another string that must also appear (repeatable)
#   --fail TEXT         string that means failure (repeatable; always
#                       includes "Kernel panic" and "SAVIOR-FAIL")
#   --timeout S         give up after S seconds (default 300)
#   --log FILE          serial log (default: a temporary file, shown on failure)
#   --no-net            no network card (default: e1000 with user networking)
#   -- ARGS...          extra QEMU arguments
#
# Exit status: 0 marker (and all --expect strings) seen, 1 failure or
# timeout, 2 usage error.
set -eu

die() { echo "qemu-smoke: $*" >&2; exit 2; }

ARCH=x86_64
FIRMWARE=bios
PAYLOAD=""
IMAGE=""
ISO=""
CPU=""
MEM=512
SMP=1
ACCEL=auto
APPEND=""
MARKER="SAVIOR-BOOT: rcS done"
TIMEOUT=300
LOG=""
NET=yes
NL='
'
EXPECTS=""
FAILS="Kernel panic${NL}SAVIOR-FAIL"

while [ $# -gt 0 ]; do
	case "$1" in
	--payload-dir) PAYLOAD=$2; shift 2 ;;
	--image) IMAGE=$2; shift 2 ;;
	--iso) ISO=$2; shift 2 ;;
	--arch) ARCH=$2; shift 2 ;;
	--firmware) FIRMWARE=$2; shift 2 ;;
	--cpu) CPU=$2; shift 2 ;;
	--mem) MEM=$2; shift 2 ;;
	--smp) SMP=$2; shift 2 ;;
	--accel) ACCEL=$2; shift 2 ;;
	--append) APPEND=$2; shift 2 ;;
	--marker) MARKER=$2; shift 2 ;;
	--expect) EXPECTS="$EXPECTS${EXPECTS:+$NL}$2"; shift 2 ;;
	--fail) FAILS="$FAILS$NL$2"; shift 2 ;;
	--timeout) TIMEOUT=$2; shift 2 ;;
	--log) LOG=$2; shift 2 ;;
	--no-net) NET=no; shift ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	--) shift; break ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

n=0
[ -z "$PAYLOAD" ] || n=$((n + 1))
[ -z "$IMAGE" ] || n=$((n + 1))
[ -z "$ISO" ] || n=$((n + 1))
[ "$n" -eq 1 ] || die "give exactly one of --payload-dir, --image, --iso"
case "$ARCH" in
x86_64) QEMU=${QEMU:-qemu-system-x86_64} ;;
i686) QEMU=${QEMU:-qemu-system-i386} ;;
*) die "--arch must be x86_64 or i686" ;;
esac
case "$TIMEOUT$MEM$SMP" in *[!0-9]*) die "--timeout, --mem and --smp take numbers" ;; esac
command -v "$QEMU" >/dev/null 2>&1 || die "$QEMU not found (apt install qemu-system-x86)"

set -- "$@" -m "$MEM" -smp "$SMP" -display none -monitor none -no-reboot -vga std
[ -z "$CPU" ] || set -- "$@" -cpu "$CPU"
case "$ACCEL" in
auto)
	if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then set -- "$@" -accel kvm -accel tcg; else set -- "$@" -accel tcg; fi ;;
kvm|tcg) set -- "$@" -accel "$ACCEL" ;;
*) die "--accel must be auto, kvm or tcg" ;;
esac
[ "$NET" = no ] || set -- "$@" -netdev user,id=n0 -device e1000,netdev=n0

WORK=$(mktemp -d "${TMPDIR:-/tmp}/qemu-smoke.XXXXXX")
QPID=""
# shellcheck disable=SC2317 # called by the EXIT trap
cleanup() {
	[ -z "$QPID" ] || kill "$QPID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
[ -n "$LOG" ] || LOG="$WORK/serial.log"
mkdir -p "$(dirname "$LOG")"
: >"$LOG"
set -- "$@" -serial "file:$LOG"

case "$FIRMWARE" in
bios) ;;
uefi|uefi32)
	if [ "$FIRMWARE" = uefi ]; then
		code=${OVMF_CODE:-}
		for c in /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd /usr/share/ovmf/OVMF.fd; do
			[ -n "$code" ] || { [ -f "$c" ] && code=$c; } || true
		done
		vars_src=${OVMF_VARS:-/usr/share/OVMF/OVMF_VARS_4M.fd}
	else
		code=${OVMF32_CODE:-/usr/share/OVMF/OVMF32_CODE_4M.fd}
		vars_src=${OVMF32_VARS:-/usr/share/OVMF/OVMF32_VARS_4M.fd}
	fi
	if [ -z "$code" ] || [ ! -f "$code" ]; then
		die "OVMF firmware not found for $FIRMWARE (apt install ovmf ovmf-ia32)"
	fi
	case "$code" in
	*/OVMF.fd) set -- "$@" -bios "$code" ;;
	*)
		[ -f "$vars_src" ] || die "OVMF vars template not found: $vars_src"
		cp "$vars_src" "$WORK/vars.fd"
		set -- "$@" -machine q35 \
			-drive "if=pflash,format=raw,unit=0,readonly=on,file=$code" \
			-drive "if=pflash,format=raw,unit=1,file=$WORK/vars.fd"
		;;
	esac
	;;
*) die "--firmware must be bios, uefi or uefi32" ;;
esac

if [ -n "$PAYLOAD" ]; then
	if [ ! -f "$PAYLOAD/vmlinuz" ] || [ ! -f "$PAYLOAD/initrd" ]; then
		die "$PAYLOAD needs vmlinuz and initrd"
	fi
	set -- "$@" -kernel "$PAYLOAD/vmlinuz" -initrd "$PAYLOAD/initrd" \
		-append "console=ttyS0,115200 console=tty0 consoleblank=0 savior.media=none $APPEND"
elif [ -n "$IMAGE" ]; then
	[ -f "$IMAGE" ] || die "image not found: $IMAGE"
	set -- "$@" -drive "if=none,id=stick,format=raw,snapshot=on,file=$IMAGE" \
		-device usb-ehci,id=ehci -device usb-storage,bus=ehci.0,drive=stick,bootindex=0
else
	[ -f "$ISO" ] || die "ISO not found: $ISO"
	set -- "$@" -drive "if=none,id=cd,media=cdrom,readonly=on,file=$ISO" \
		-device ide-cd,drive=cd,bootindex=0
fi

echo "qemu-smoke: $QEMU $*" >&2
"$QEMU" "$@" >"$WORK/qemu.out" 2>&1 &
QPID=$!

# found TEXT: TEXT appears in the serial log.
found() { grep -a -q -F -- "$1" "$LOG"; }
result=""
missing=""
start=$(date +%s)
while :; do
	# Sample liveness before reading the log: if QEMU is gone, the log we
	# read next is complete.
	alive=yes
	kill -0 "$QPID" 2>/dev/null || alive=no
	OLDIFS=$IFS; IFS=$NL
	for f in $FAILS; do
		if [ -z "$result" ] && found "$f"; then result="found failure string '$f'"; fi
	done
	IFS=$OLDIFS
	if [ -z "$result" ] && found "$MARKER"; then
		missing=""
		OLDIFS=$IFS; IFS=$NL
		for e in $EXPECTS; do found "$e" || missing="$missing '$e'"; done
		IFS=$OLDIFS
		if [ -z "$missing" ]; then
			echo "qemu-smoke: PASS: '$MARKER' after $(($(date +%s) - start)) s" >&2
			exit 0
		fi
	fi
	[ -z "$result" ] || break
	if [ "$alive" = no ]; then
		QPID=""
		if [ -n "$missing" ]; then
			result="QEMU exited; '$MARKER' appeared but not:$missing"
		else
			result="QEMU exited before '$MARKER' appeared"
		fi
		break
	fi
	if [ $(($(date +%s) - start)) -ge "$TIMEOUT" ]; then
		result="timeout after $TIMEOUT s waiting for '$MARKER'${missing:+ and$missing}"
		break
	fi
	sleep 2
done

echo "qemu-smoke: FAIL: $result" >&2
echo "--- QEMU output" >&2
tail -n 20 "$WORK/qemu.out" >&2 || true
echo "--- last 60 lines of the serial console ($LOG)" >&2
tail -n 60 "$LOG" | tr -d '\r' >&2 || true
exit 1
