#!/bin/sh
# qemu-smoke.sh - boot a SaviorOS payload or boot medium in QEMU and wait for
# a marker on the serial console. Used by CI for the Buildroot images, and by
# scripts/release-e2e.sh, which keeps the VM running (--keep-running) to test
# it end to end (the dev-image tests are os/dev/qemu-test.sh).
#
# Usage:
#   scripts/qemu-smoke.sh [options] (--payload-dir DIR | --image FILE | --iso FILE | --netboot DIR)
#
#   --payload-dir DIR   direct kernel boot of DIR/vmlinuz + DIR/initrd
#   --image FILE        boot a USB disk image (attached as a USB stick on EHCI;
#                       writes go to a throwaway snapshot unless --conf,
#                       --grow or --append made a private copy)
#   --iso FILE          boot an ISO as a CD-ROM
#   --netboot DIR       PXE-boot a netboot tree (mkimage.sh's netboot/)
#                       from QEMU's built-in DHCP and TFTP server; the NIC
#                       is the only boot device
#   --bootfile PATH     --netboot: the file the DHCP reply names, relative to
#                       DIR (default by --firmware: bios boot/grub/i386-pc/core.0,
#                       uefi boot/grub/x86_64-efi/core.efi, uefi32
#                       boot/grub/i386-efi/core.efi)
#   --arch A            x86_64 (default) or i686: qemu-system-x86_64 / -i386
#   --firmware F        bios (default, SeaBIOS), uefi (OVMF x64) or uefi32
#                       (OVMF ia32; $OVMF_CODE / $OVMF32_CODE override paths)
#   --cpu MODEL         QEMU CPU model, e.g. "pentium3,-pae" (default: QEMU's)
#   --mem MB            guest RAM (default 512)
#   --smp N             guest CPUs (default 1)
#   --accel A           auto (default: kvm when usable, else tcg), kvm or tcg.
#                       Use tcg to test CPU feature limits: KVM runs SSE2
#                       instructions even when the model hides the flag.
#   --append ARGS       extra kernel arguments: --payload-dir; --netboot (added
#                       to savior_cmdline in the copy's grub.cfg); --image
#                       (savior_args in the copy's /boot-options.cfg). No
#                       '"', '$' or '\' (GRUB would expand them).
#   --conf FILE         --image: the copy's SAVIOR partition gets FILE as
#                       savior.conf (mtools; the partition start is read from
#                       the MBR)
#   --grow MB           --image: the copy gets MB of free (sparse) space
#                       after its partition, e.g. 2048 for a hive's SAVIOR-DATA
#   --hostfwd SPEC      forward a host port to the guest through QEMU's user
#                       network, e.g. tcp:127.0.0.1:2222-:22 (repeatable)
#   --monitor PATH      QEMU's HMP monitor on the UNIX socket PATH (e.g. for
#                       screendump; default: no monitor). unix:PATH works too.
#   --keep-running PIDFILE
#                       on success, leave QEMU running and write its PID to
#                       PIDFILE; the caller stops it (needs --log). The work
#                       directory is removed anyway: QEMU keeps its open disk
#                       copy, and a netbooted guest has finished its TFTP
#                       transfers by then.
#   --marker TEXT       success string on the serial console (default "SAVIOR-BOOT: rcS done",
#                       which the rootfs overlay prints when the boot scripts finish)
#   --expect TEXT       another string that must also appear, before or
#                       after the marker (repeatable)
#   --expect-min NAME=N the last SAVIOR-AGENT line must carry NAME=<integer>
#                       with a value of at least N, e.g. mem_avail=100
#                       (repeatable; a lower or unknown value fails at once)
#   --fail TEXT         string that means failure (repeatable; always
#                       includes "Kernel panic", "SAVIOR-FAIL" and the
#                       SAVIOR-AGENT failure states "agent=crashing" and
#                       "agent=down")
#   --timeout S         give up after S seconds (default 300)
#   --log FILE          serial log (default: a temporary file, shown on failure)
#   --no-net            no network card (default: e1000 with user networking)
#   -- ARGS...          extra QEMU arguments
#
# The rootfs overlay's boot-report prints two lines (boot-report explains
# the fields): the marker line "SAVIOR-BOOT: rcS done ... fb= net= node= ver=",
# then "SAVIOR-AGENT: agent=up|crashing|down ... arch= ver=" once the node
# agent has run for 10 s (or has not). A release check therefore looks like
#   --expect net=10.0.2. --expect agent=up --expect arch=i686 --fail ver=unknown
# Both lines are printed at the end, pass or fail.
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
NETBOOT=""
BOOTFILE=""
CONF=""
GROW=""
HOSTFWDS=""
MONITOR=""
KEEP=""
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
MINS=""
FAILS="Kernel panic${NL}SAVIOR-FAIL${NL}agent=crashing${NL}agent=down"

while [ $# -gt 0 ]; do
	case "$1" in
	--payload-dir) PAYLOAD=$2; shift 2 ;;
	--image) IMAGE=$2; shift 2 ;;
	--iso) ISO=$2; shift 2 ;;
	--netboot) NETBOOT=$2; shift 2 ;;
	--bootfile) BOOTFILE=$2; shift 2 ;;
	--conf) CONF=$2; shift 2 ;;
	--grow) GROW=$2; shift 2 ;;
	--hostfwd) HOSTFWDS="$HOSTFWDS${HOSTFWDS:+ }$2"; shift 2 ;;
	--monitor) MONITOR=${2#unix:}; shift 2 ;;
	--keep-running) KEEP=$2; shift 2 ;;
	--arch) ARCH=$2; shift 2 ;;
	--firmware) FIRMWARE=$2; shift 2 ;;
	--cpu) CPU=$2; shift 2 ;;
	--mem) MEM=$2; shift 2 ;;
	--smp) SMP=$2; shift 2 ;;
	--accel) ACCEL=$2; shift 2 ;;
	--append) APPEND=$2; shift 2 ;;
	--marker) MARKER=$2; shift 2 ;;
	--expect) EXPECTS="$EXPECTS${EXPECTS:+$NL}$2"; shift 2 ;;
	--expect-min)
		case $2 in
		[a-z_]*=[0-9]*) ;;
		*) die "--expect-min wants NAME=NUMBER, got '$2'" ;;
		esac
		MINS="$MINS${MINS:+ }$2"; shift 2 ;;
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
[ -z "$NETBOOT" ] || n=$((n + 1))
[ "$n" -eq 1 ] || die "give exactly one of --payload-dir, --image, --iso, --netboot"
case "$ARCH" in
x86_64) QEMU=${QEMU:-qemu-system-x86_64} ;;
i686) QEMU=${QEMU:-qemu-system-i386} ;;
*) die "--arch must be x86_64 or i686" ;;
esac
case "$TIMEOUT$MEM$SMP$GROW" in *[!0-9]*) die "--timeout, --mem, --smp and --grow take numbers" ;; esac
[ -z "$BOOTFILE" ] || [ -n "$NETBOOT" ] || die "--bootfile needs --netboot"
if [ -n "$CONF$GROW" ] && [ -z "$IMAGE" ]; then die "--conf and --grow need --image"; fi
[ -z "$APPEND" ] || [ -z "$ISO" ] || die "--append does not work with --iso"
# GRUB expands these inside the double-quoted savior_cmdline/savior_args.
case "$APPEND" in *[\"\$\\]*) die "--append must not contain '\"', '\$' or '\\'" ;; esac
[ -z "$CONF" ] || [ -f "$CONF" ] || die "--conf: no such file: $CONF"
if [ "$NET" = no ] && [ -n "$NETBOOT$HOSTFWDS" ]; then die "--netboot and --hostfwd need the network (drop --no-net)"; fi
for f in $HOSTFWDS; do
	case "$f" in *,*|"") die "--hostfwd: bad forward '$f'" ;; esac
done
case "$MONITOR" in *,*) die "--monitor: the socket path must not contain ','" ;; esac
[ -z "$KEEP" ] || [ -n "$LOG" ] || die "--keep-running needs --log (the serial log outlives this script)"
command -v "$QEMU" >/dev/null 2>&1 || die "$QEMU not found (apt install qemu-system-x86)"

if [ -n "$MONITOR" ]; then
	mkdir -p "$(dirname "$MONITOR")"
	[ ! -S "$MONITOR" ] || rm -f "$MONITOR"
	set -- "$@" -monitor "unix:$MONITOR,server,nowait"
else
	set -- "$@" -monitor none
fi
set -- "$@" -m "$MEM" -smp "$SMP" -display none -no-reboot -vga std
[ -z "$CPU" ] || set -- "$@" -cpu "$CPU"
case "$ACCEL" in
auto)
	if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then set -- "$@" -accel kvm -accel tcg; else set -- "$@" -accel tcg; fi ;;
kvm|tcg) set -- "$@" -accel "$ACCEL" ;;
*) die "--accel must be auto, kvm or tcg" ;;
esac

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

# fat_offset IMG: byte offset of the first MBR partition (the SAVIOR FAT).
fat_offset() {
	od -An -tu1 -j 454 -N 4 "$1" | awk '{ print ($1 + $2 * 256 + $3 * 65536 + $4 * 16777216) * 512 }'
}

NETDEV=user,id=n0
NIC=e1000,netdev=n0
for f in $HOSTFWDS; do NETDEV="$NETDEV,hostfwd=$f"; done
if [ -n "$PAYLOAD" ]; then
	if [ ! -f "$PAYLOAD/vmlinuz" ] || [ ! -f "$PAYLOAD/initrd" ]; then
		die "$PAYLOAD needs vmlinuz and initrd"
	fi
	set -- "$@" -kernel "$PAYLOAD/vmlinuz" -initrd "$PAYLOAD/initrd" \
		-append "console=ttyS0,115200 console=tty0 consoleblank=0 savior.media=none $APPEND"
elif [ -n "$IMAGE" ]; then
	[ -f "$IMAGE" ] || die "image not found: $IMAGE"
	if [ -n "$CONF$GROW$APPEND" ]; then
		# A private copy: the settings go onto its SAVIOR partition.
		img=$WORK/stick.img
		cp --sparse=always "$IMAGE" "$img" 2>/dev/null || cp "$IMAGE" "$img"
		# Sparse growth: dd extends the file to the seek offset.
		[ -z "$GROW" ] || dd if=/dev/null of="$img" bs=1 count=0 seek=$(($(wc -c <"$img") + GROW * 1048576)) 2>/dev/null ||
			die "cannot grow the image copy"
		off=$(fat_offset "$img")
		[ "$off" -gt 0 ] || die "$IMAGE has no partition in its MBR"
		command -v mcopy >/dev/null 2>&1 || die "--conf and --append need mtools (apt install mtools)"
		if [ -n "$CONF" ]; then
			MTOOLS_SKIP_CHECK=1 mcopy -o -i "$img@@$off" "$CONF" ::/savior.conf || die "cannot write savior.conf to the image copy"
		fi
		if [ -n "$APPEND" ]; then
			printf 'set savior_args="%s"\r\n' "$APPEND" >"$WORK/boot-options.cfg"
			MTOOLS_SKIP_CHECK=1 mcopy -o -i "$img@@$off" "$WORK/boot-options.cfg" ::/boot-options.cfg ||
				die "cannot write boot-options.cfg to the image copy"
		fi
		set -- "$@" -drive "if=none,id=stick,format=raw,file=$img"
	else
		set -- "$@" -drive "if=none,id=stick,format=raw,snapshot=on,file=$IMAGE"
	fi
	set -- "$@" -device usb-ehci,id=ehci -device usb-storage,bus=ehci.0,drive=stick,bootindex=0
elif [ -n "$ISO" ]; then
	[ -f "$ISO" ] || die "ISO not found: $ISO"
	set -- "$@" -drive "if=none,id=cd,media=cdrom,readonly=on,file=$ISO" \
		-device ide-cd,drive=cd,bootindex=0
else
	[ -f "$NETBOOT/boot/grub/grub.cfg" ] || die "$NETBOOT is not a netboot tree (no boot/grub/grub.cfg)"
	if [ -z "$BOOTFILE" ]; then
		case "$FIRMWARE" in
		bios) BOOTFILE=boot/grub/i386-pc/core.0 ;;
		uefi) BOOTFILE=boot/grub/x86_64-efi/core.efi ;;
		uefi32) BOOTFILE=boot/grub/i386-efi/core.efi ;;
		esac
	fi
	case "$BOOTFILE" in /*|*..*|*,*) die "--bootfile must be a relative path inside the netboot tree" ;; esac
	[ -f "$NETBOOT/$BOOTFILE" ] || die "no $BOOTFILE in $NETBOOT"
	tftp=$WORK/tftp
	cp -R "$NETBOOT" "$tftp"
	if [ -n "$APPEND" ]; then
		esc=$(printf '%s\n' "$APPEND" | sed 's/[|&\\]/\\&/g')
		sed "s|^set savior_cmdline=\"|set savior_cmdline=\"$esc |" "$NETBOOT/boot/grub/grub.cfg" >"$tftp/boot/grub/grub.cfg"
		grep -q -F "set savior_cmdline=\"$APPEND " "$tftp/boot/grub/grub.cfg" ||
			die "cannot add --append to $NETBOOT/boot/grub/grub.cfg (no savior_cmdline line?)"
	fi
	NETDEV="$NETDEV,tftp=$tftp,bootfile=$BOOTFILE"
	NIC="$NIC,bootindex=0"
fi
[ "$NET" = no ] || set -- "$@" -netdev "$NETDEV" -device "$NIC"

echo "qemu-smoke: $QEMU $*" >&2
"$QEMU" "$@" >"$WORK/qemu.out" 2>&1 &
QPID=$!

# found TEXT: TEXT appears in the serial log.
found() { grep -a -q -F -- "$1" "$LOG"; }
# agent_field NAME: NAME's value on the last SAVIOR-AGENT line ("" if none).
agent_field() {
	grep -a 'SAVIOR-AGENT: ' "$LOG" | tail -n 1 | tr -d '\r' | tr ' ' '\n' | sed -n "s/^$1=//p" | head -n 1
}
# check_mins: for every --expect-min, "" while the line is missing, "ok"
# when all values reach their minimum, else a failure message.
check_mins() {
	for m in $MINS; do
		name=${m%%=*} min=${m#*=}
		grep -a -q 'SAVIOR-AGENT: ' "$LOG" || return 0
		v=$(agent_field "$name")
		case $v in
		''|*[!0-9]*) echo "$name='$v' on the SAVIOR-AGENT line is not a number (want >= $min)"; return 0 ;;
		esac
		[ "$v" -ge "$min" ] || { echo "$name=$v is below the minimum $min"; return 0; }
	done
	echo ok
}
# report: the boot-report status lines seen so far.
report() {
	grep -a -E 'SAVIOR-(BOOT|AGENT): ' "$LOG" | tr -d '\r' | sed 's/^/qemu-smoke:   /' >&2 || true
}
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
	if [ -z "$result" ] && [ -n "$MINS" ]; then
		mins=$(check_mins)
		case $mins in ''|ok) ;; *) result=$mins ;; esac
	fi
	if [ -z "$result" ] && found "$MARKER"; then
		missing=""
		OLDIFS=$IFS; IFS=$NL
		for e in $EXPECTS; do found "$e" || missing="$missing '$e'"; done
		IFS=$OLDIFS
		[ -z "$MINS" ] || [ "$(check_mins)" = ok ] || missing="$missing --expect-min $MINS"
		if [ -z "$missing" ]; then
			echo "qemu-smoke: PASS: '$MARKER' after $(($(date +%s) - start)) s" >&2
			report
			if [ -n "$KEEP" ]; then
				echo "$QPID" >"$KEEP"
				echo "qemu-smoke: QEMU keeps running as PID $QPID (--keep-running $KEEP)" >&2
				QPID=""
			fi
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
report
echo "--- QEMU output" >&2
tail -n 20 "$WORK/qemu.out" >&2 || true
echo "--- last 60 lines of the serial console ($LOG)" >&2
tail -n 60 "$LOG" | tr -d '\r' >&2 || true
exit 1
