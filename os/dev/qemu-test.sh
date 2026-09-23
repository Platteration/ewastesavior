#!/bin/sh
# os/dev/qemu-test.sh - QEMU tests of the SaviorOS dev image (DESIGN 14).
#
# Usage:
#   os/dev/qemu-test.sh [options] <test>...
#
# Tests:
#   boot-bios      USB stick (usb-storage on EHCI), SeaBIOS
#   boot-uefi      USB stick, OVMF (x86_64 UEFI)
#   boot-iso-bios  the ISO as an IDE CD-ROM, SeaBIOS (config from the CD)
#   boot-iso-uefi  the ISO as a CD-ROM, OVMF, plus a plain FAT stick with
#                  savior.conf (a CD-booted machine takes that config)
#   pxe-bios       PXE via QEMU's TFTP (netboot tree, core.0)
#   pxe-uefi       PXE via QEMU's TFTP, OVMF (core.efi)
#   screen         boot, then screendump through the QEMU monitor (PPM)
#   baked-conf     mkimage --conf: the config baked into savior-conf.cpio
#                  (second initrd) is used when the stick has no savior.conf
#   swarm          hive + 2 compute nodes + 1 display node on a private
#                  multicast LAN, driven with `savior ctl` from this host:
#                  job with count=4 and outputs, display text, identify,
#                  duplicate node ID
#   hive-pxe       a hive with netboot = yes PXE-boots diskless VMs (BIOS,
#                  then UEFI) on a private LAN (S65netboot: dnsmasq DHCP +
#                  TFTP, kernels over HTTP); each node joins keyless, is
#                  pending, and gets approved with savior ctl
#   all            every test above except swarm and hive-pxe (--swarm
#                  adds them)
#
# Options:
#   --out DIR          dev image directory (default build/dev); logs go to
#                      DIR/logs/<test>.log, serial consoles to
#                      DIR/logs/<test>[-vm].serial, screendumps to *.ppm
#   --keep             keep the per-run work directory (stick copies etc.)
#   --swarm            include swarm and hive-pxe in "all"
#   --expect-display   screen: also require a picture on the screen
#                      (non-black pixels), for when the display agent works
#   --timeout S        seconds to wait for a VM to boot (default 300)
#   --mem MB           guest RAM for boot tests (default 512)
#
# Every check prints a PASS: or FAIL: line; a summary with timings comes
# last. Exit status: 0 when everything passed, 1 otherwise, 2 usage.
# QEMU runs with TCG only (no KVM needed); expect 30-120 s per boot.
#
# The guests print a status line on their serial console at the end of
# boot (os/rootfs-overlay/usr/libexec/savior/boot-report):
#   SAVIOR-BOOT: rcS done up=.. cfg=yes media=/dev/sda1 key=yes fb=yes
#                net=10.0.2.15 console=yes node=yes ver=.. t=...

# Functions run through trap and check() look unreachable to shellcheck,
# and single-quoted $arch below is GRUB's variable:
# shellcheck disable=SC2317,SC2016
set -u
umask 022

REPO=$(cd "$(dirname "$0")/../.." && pwd)
OUT=$REPO/build/dev
KEEP=no
SWARM=no
EXPECT_DISPLAY=no
BOOT_TIMEOUT=300
MEM=512
TESTS=""
ALL_TESTS="boot-bios boot-uefi boot-iso-bios boot-iso-uefi pxe-bios pxe-uefi screen baked-conf"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }
while [ $# -gt 0 ]; do
	case "$1" in
	--out) OUT=$2; shift 2 ;;
	--keep) KEEP=yes; shift ;;
	--swarm) SWARM=yes; shift ;;
	--expect-display) EXPECT_DISPLAY=yes; shift ;;
	--timeout) BOOT_TIMEOUT=$2; shift 2 ;;
	--mem) MEM=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	-*) echo "qemu-test: unknown option $1" >&2; usage >&2; exit 2 ;;
	*) TESTS="$TESTS $1"; shift ;;
	esac
done
case "$BOOT_TIMEOUT$MEM" in *[!0-9]*) echo "qemu-test: --timeout and --mem take numbers" >&2; exit 2 ;; esac
[ -n "$TESTS" ] || { usage >&2; exit 2; }
expanded=""
for t in $TESTS; do
	case "$t" in
	all)
		expanded="$expanded $ALL_TESTS"
		[ "$SWARM" = no ] || expanded="$expanded swarm hive-pxe"
		;;
	boot-bios | boot-uefi | boot-iso-bios | boot-iso-uefi | pxe-bios | pxe-uefi | screen | baked-conf | swarm | hive-pxe)
		expanded="$expanded $t"
		;;
	*) echo "qemu-test: unknown test $t" >&2; exit 2 ;;
	esac
done
TESTS=$expanded

QEMU=${QEMU:-qemu-system-x86_64}
OVMF=${OVMF:-}
if [ -z "$OVMF" ]; then
	for f in /usr/share/ovmf/OVMF.fd /usr/share/qemu/OVMF.fd /usr/share/OVMF/OVMF.fd; do
		if [ -f "$f" ]; then
			OVMF=$f
			break
		fi
	done
fi
command -v "$QEMU" >/dev/null 2>&1 || { echo "qemu-test: $QEMU not found (apt install qemu-system-x86)" >&2; exit 2; }
[ -d "$OUT" ] || { echo "qemu-test: $OUT does not exist; run os/dev/build.sh first" >&2; exit 2; }
OUT=$(cd "$OUT" && pwd)
MEDIA=$OUT/media
LOGS=$OUT/logs
mkdir -p "$LOGS" "$OUT/qemu-work"
W=$(mktemp -d "$OUT/qemu-work/run.XXXXXX")

PIDS=""
cleanup() {
	for p in $PIDS; do
		kill "$p" 2>/dev/null
	done
	sleep 1
	for p in $PIDS; do
		kill -9 "$p" 2>/dev/null
	done
	if [ "$KEEP" = yes ]; then
		echo "qemu-test: work directory kept: $W"
	else
		rm -rf "$W"
	fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# ---------------------------------------------------------------------------
# Reporting

TOTAL_FAIL=0
SUMMARY=""
CUR=""
TLOG=/dev/null
T_FAIL=0

say() {
	echo "$*"
	echo "$*" >>"$TLOG"
}
pass() { say "PASS: $CUR: $*"; }
fail() {
	say "FAIL: $CUR: $*"
	T_FAIL=$((T_FAIL + 1))
}
info() { say "      $CUR: $*"; }
# check DESCRIPTION COMMAND...: PASS or FAIL depending on COMMAND.
check() {
	_d=$1
	shift
	if "$@"; then pass "$_d"; else fail "$_d"; fi
}

begin_test() {
	CUR=$1
	TLOG=$LOGS/$1.log
	: >"$TLOG"
	T_FAIL=0
	T_START=$(date +%s)
	say "=== $1 ($(date -u +%H:%M:%S) UTC)"
}
end_test() {
	stop_vms
	_t=$(($(date +%s) - T_START))
	if [ "$T_FAIL" -eq 0 ]; then
		_r=PASS
	else
		_r="FAIL ($T_FAIL)"
		TOTAL_FAIL=$((TOTAL_FAIL + 1))
	fi
	say "=== $CUR: $_r in ${_t} s"
	SUMMARY="$SUMMARY$(printf '%-14s %-10s %4s s' "$CUR" "$_r" "$_t")
"
}

# ---------------------------------------------------------------------------
# VMs

VM_PID=""
TEST_PIDS=""

# vm NAME SERIAL QEMU-ARGS...: start a VM in the background (TCG, no
# display, serial to SERIAL, HMP monitor on $W/NAME.mon); sets VM_PID.
vm() {
	_name=$1
	_serial=$2
	shift 2
	: >"$_serial"
	rm -f "$W/$_name.mon"
	echo "$QEMU -accel tcg -display none -no-reboot -name $_name -serial file:$_serial -monitor unix:$W/$_name.mon,server,nowait $*" >>"$TLOG"
	"$QEMU" -accel tcg -display none -no-reboot -name "$_name" \
		-serial "file:$_serial" -monitor "unix:$W/$_name.mon,server,nowait" \
		"$@" >"$W/$_name.qemu.out" 2>&1 &
	VM_PID=$!
	PIDS="$PIDS $VM_PID"
	TEST_PIDS="$TEST_PIDS $VM_PID"
}

stop_vms() {
	for _p in $TEST_PIDS; do
		kill "$_p" 2>/dev/null
	done
	for _p in $TEST_PIDS; do
		_n=0
		while kill -0 "$_p" 2>/dev/null && [ "$_n" -lt 20 ]; do
			sleep 0.5
			_n=$((_n + 1))
		done
		kill -9 "$_p" 2>/dev/null
	done
	TEST_PIDS=""
}

# wait_for SERIAL REGEX TIMEOUT PID: 0 found, 1 timeout, 2 QEMU exited,
# 3 dead end: kernel panic, SAVIOR-FAIL, the firmware fell through to the
# EFI shell, or the netboot menu gave up.
wait_for() {
	_start=$(date +%s)
	while :; do
		_alive=yes
		kill -0 "$4" 2>/dev/null || _alive=no
		if grep -a -q -E -- "$2" "$1" 2>/dev/null; then
			return 0
		fi
		if grep -a -q -E 'Kernel panic|SAVIOR-FAIL|UEFI Interactive Shell|cannot load the system from the hive' "$1" 2>/dev/null; then
			return 3
		fi
		[ "$_alive" = yes ] || return 2
		[ $(($(date +%s) - _start)) -lt "$3" ] || return 1
		sleep 2
	done
}

# wait_boot SERIAL PID [TIMEOUT]: wait for the boot-report line; PASS/FAIL.
wait_boot() {
	_t0=$(date +%s)
	wait_for "$1" 'SAVIOR-BOOT: rcS done' "${3:-$BOOT_TIMEOUT}" "$2"
	_rc=$?
	_took=$(($(date +%s) - _t0))
	case "$_rc" in
	0)
		pass "booted, boot report after ${_took} s"
		BOOTLINE=$(grep -a 'SAVIOR-BOOT: rcS done' "$1" | tail -n 1 | tr -d '\r')
		info "$BOOTLINE"
		return 0
		;;
	1) fail "no boot report within ${3:-$BOOT_TIMEOUT} s" ;;
	2) fail "QEMU exited before the boot report: $(tail -n 3 "$W"/*.qemu.out 2>/dev/null | tr '\n' ' ')" ;;
	3) fail "dead end on the console (kernel panic, SAVIOR-FAIL, EFI shell or netboot gave up)" ;;
	esac
	BOOTLINE=""
	info "last serial lines:"
	tail -n 25 "$1" | tr -d '\r' | sed 's/^/        | /' | tee -a "$TLOG"
	return 1
}

# field NAME: value of NAME=... in $BOOTLINE.
field() { printf '%s\n' "$BOOTLINE" | tr ' ' '\n' | sed -n "s/^$1=//p" | head -n 1; }
is() { [ "$(field "$1")" = "$2" ]; }
differs() { ! cmp -s "$1" "$2"; }
matches() { field "$1" | grep -q -E -- "$2"; }

# check_report EXPECTS...: EXPECTS are NAME=VALUE (exact) or NAME~REGEX.
check_report() {
	for _e in "$@"; do
		case "$_e" in
		*~*) check "${_e%%~*} matches ${_e#*~} (got '$(field "${_e%%~*}")')" matches "${_e%%~*}" "${_e#*~}" ;;
		*=*) check "${_e%%=*} = ${_e#*=} (got '$(field "${_e%%=*}")')" is "${_e%%=*}" "${_e#*=}" ;;
		esac
	done
}

# check_respawn SERIAL: savior programs that exit at once (placeholders,
# crashes) must not be restarted in a tight loop: at most 8 "exited" lines
# from stub commands in 30 s.
check_respawn() {
	_a=$(grep -a -c 'not implemented yet' "$1")
	sleep 30
	_b=$(grep -a -c 'not implemented yet' "$1")
	check "no respawn flood ($((_b - _a)) placeholder exits in 30 s)" [ $((_b - _a)) -le 8 ]
}

# hmp VM COMMAND: run a QEMU monitor command.
hmp() {
	_sock=$W/$1.mon
	if command -v python3 >/dev/null 2>&1; then
		python3 - "$_sock" "$2" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX)
s.settimeout(15)
s.connect(sys.argv[1])
buf = b""
def until(tok, secs=15):
    global buf
    end = time.time() + secs
    while tok not in buf and time.time() < end:
        try:
            d = s.recv(4096)
        except socket.timeout:
            break
        if not d:
            break
        buf += d
until(b"(qemu) ")
buf = b""
s.sendall(sys.argv[2].encode() + b"\n")
until(b"(qemu) ", 60)
sys.stdout.write(buf.decode("utf-8", "replace"))
PY
	else
		printf '%s\n' "$2" | nc -U -q 5 "$_sock"
	fi
}

# screendump VM FILE: PPM screenshot; 0 when the file was written.
screendump() {
	rm -f "$2"
	hmp "$1" "screendump $2" >/dev/null 2>&1
	_n=0
	while [ ! -s "$2" ] && [ "$_n" -lt 20 ]; do
		sleep 0.5
		_n=$((_n + 1))
	done
	[ -s "$2" ]
}

# ppm_stats FILE: print "WIDTH HEIGHT NONZERO_BYTES TOTAL_BYTES" of a P6 PPM.
ppm_stats() {
	_hdr=$(head -n 3 "$1" | wc -c)
	_dim=$(sed -n 2p "$1")
	_tot=$(($(wc -c <"$1") - _hdr))
	_nz=$(tail -c "$_tot" "$1" | tr -d '\000' | wc -c)
	echo "$_dim $_nz $_tot"
}

# ---------------------------------------------------------------------------
# Media

KEY=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
MT_OFF=1048576

# stick_copy DEST: a private copy of the USB image.
stick_copy() {
	cp --sparse=always "$MEDIA/savior.img" "$1" 2>/dev/null || cp "$MEDIA/savior.img" "$1"
}

# stick_conf IMG[@@OFFSET] LINES...: append settings to savior.conf on a
# FAT image (CRLF, like a file edited on Windows).
stick_conf() {
	_img=$1
	shift
	MTOOLS_SKIP_CHECK=1 mtype -i "$_img" ::/savior.conf >"$W/conf.tmp" 2>/dev/null || : >"$W/conf.tmp"
	for _l in "$@"; do
		printf '%s\r\n' "$_l" >>"$W/conf.tmp"
	done
	MTOOLS_SKIP_CHECK=1 mcopy -o -i "$_img" "$W/conf.tmp" ::/savior.conf || return 1
	rm -f "$W/conf.tmp"
}

# boot_options IMG[@@OFFSET]: a /boot-options.cfg that makes the guest copy
# its boot log to the serial console (boot-report, savior_dumplog=1).
boot_options() {
	printf 'set savior_args="savior_dumplog=1"\r\n' >"$W/bo.tmp"
	MTOOLS_SKIP_CHECK=1 mcopy -o -i "$1" "$W/bo.tmp" ::/boot-options.cfg
	rm -f "$W/bo.tmp"
}

fat_has() { MTOOLS_SKIP_CHECK=1 mdir -i "$1" "$2" >/dev/null 2>&1; }

need_file() {
	[ -e "$1" ] && return 0
	fail "missing $1 (run os/dev/build.sh)"
	return 1
}

need_ovmf() {
	[ -n "$OVMF" ] && [ -f "$OVMF" ] && return 0
	fail "OVMF firmware not found (apt install ovmf, or set OVMF=/path/OVMF.fd)"
	return 1
}

usb_stick_args() { echo "-drive if=none,id=stick,format=raw,file=$1 -device usb-ehci,id=ehci -device usb-storage,bus=ehci.0,drive=stick${2:+,bootindex=$2}"; }

# ---------------------------------------------------------------------------
# Tests

# boot_stick TEST FIRMWARE: boot a copy of savior.img with a test swarm key.
boot_stick() {
	need_file "$MEDIA/savior.img" || return
	fw=$2
	[ "$fw" = bios ] || need_ovmf || return
	img=$W/$1.img
	stick_copy "$img"
	stick_conf "$img@@$MT_OFF" "swarm_key = $KEY" || { fail "cannot edit savior.conf on the stick copy"; return; }
	boot_options "$img@@$MT_OFF"
	serial=$LOGS/$1.serial
	# shellcheck disable=SC2046 # word-split the device arguments
	set -- -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 $(usb_stick_args "$img" 0)
	[ "$fw" = bios ] || set -- -bios "$OVMF" "$@"
	vm "$CUR" "$serial" "$@"
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=yes key=yes 'media~^/dev/sd[a-z]+1$' net=10.0.2.15 console=yes fb=yes
	gl=$(grep -a -o 'SaviorOS: boot medium ([^)]*) found by [a-z]*' "$serial" | tail -n 1)
	info "GRUB: ${gl:-no boot medium line}"
	if [ "$fw" = bios ]; then
		check "GRUB root from the core image prefix (,msdos1)" \
			grep -a -q -E 'boot medium \(hd[0-9]+,msdos1\) found by boot device' "$serial"
	else
		check "GRUB found the stick" grep -a -q 'SaviorOS: boot medium' "$serial"
	fi
	check "/boot-options.cfg sourced by GRUB (boot log dumped)" wait_for "$serial" 'SAVIOR-LOG: ' 30 "$VM_PID"
	check_respawn "$serial"
}

t_boot_bios() { boot_stick boot-bios bios; }

# baked-conf: mkimage --conf bakes the config into boot/savior-conf.cpio
# (/etc/savior/baked.conf, a second initrd). The stick's savior.conf is
# removed, so the swarm key can only come from the baked copy.
t_baked_conf() {
	need_file "$OUT/vmlinuz" || return
	need_file "$OUT/initrd" || return
	printf '# baked by qemu-test\nswarm_key = %s\nname = baked-node\n' "$KEY" >"$W/baked.conf"
	if ! sh "$REPO/os/image/mkimage.sh" --out "$W/baked" --formats img --version baked-test \
		--payload "x86_64=$OUT/vmlinuz,$OUT/initrd" --conf "$W/baked.conf" >>"$TLOG" 2>&1; then
		fail "mkimage.sh --conf failed"
		return
	fi
	img=$W/baked/savior.img
	check "boot/savior-conf.cpio is on the stick" fat_has "$img@@$MT_OFF" ::/boot/savior-conf.cpio
	MTOOLS_SKIP_CHECK=1 mtype -i "$img@@$MT_OFF" ::/boot/grub/grub.cfg >"$W/baked-grub.cfg" 2>/dev/null
	check "grub.cfg loads savior-conf.cpio as a second initrd" \
		grep -q -F 'initrd /boot/$arch/initrd /boot/savior-conf.cpio' "$W/baked-grub.cfg"
	MTOOLS_SKIP_CHECK=1 mdel -i "$img@@$MT_OFF" ::/savior.conf || { fail "cannot remove savior.conf"; return; }
	boot_options "$img@@$MT_OFF"
	serial=$LOGS/$CUR.serial
	# shellcheck disable=SC2046
	vm "$CUR" "$serial" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 $(usb_stick_args "$img" 0)
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=no key=yes 'media~^/dev/sd[a-z]+1$' net=10.0.2.15
	check "S08config used /etc/savior/baked.conf" wait_for "$serial" 'SAVIOR-LOG: .*baked.conf' 30 "$VM_PID"
}
t_boot_uefi() { boot_stick boot-uefi uefi; }

t_boot_iso_bios() {
	need_file "$MEDIA/savior.iso" || return
	serial=$LOGS/$CUR.serial
	vm "$CUR" "$serial" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 \
		-drive "if=none,id=cd,media=cdrom,readonly=on,format=raw,file=$MEDIA/savior.iso" \
		-device ide-cd,drive=cd,bootindex=0
	wait_boot "$serial" "$VM_PID" || return
	# The CD holds the commented savior.conf template: config found, no key.
	check_report cfg=yes key=no 'media~^/dev/sr[0-9]+$' net=10.0.2.15 console=yes fb=yes
	check_respawn "$serial"
}

t_boot_iso_uefi() {
	need_file "$MEDIA/savior.iso" || return
	need_ovmf || return
	# A plain FAT stick with only savior.conf: a CD-booted machine takes it.
	conf=$W/conf-stick.img
	rm -f "$conf"
	dd if=/dev/zero of="$conf" bs=1M count=0 seek=16 2>/dev/null
	mkfs.fat -n CONFIG "$conf" >/dev/null || { fail "mkfs.fat failed"; return; }
	stick_conf "$conf" "# config stick for a CD-booted machine" "swarm_key = $KEY" ||
		{ fail "cannot write savior.conf to the config stick"; return; }
	serial=$LOGS/$CUR.serial
	# shellcheck disable=SC2046
	vm "$CUR" "$serial" -bios "$OVMF" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 \
		-drive "if=none,id=cd,media=cdrom,readonly=on,format=raw,file=$MEDIA/savior.iso" \
		-device ide-cd,drive=cd,bootindex=0 $(usb_stick_args "$conf")
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=yes key=yes 'media~^/dev/sd[a-z]+$' net=10.0.2.15 console=yes fb=yes
	check_respawn "$serial"
}

# pxe TEST FIRMWARE BOOTFILE
pxe() {
	need_file "$MEDIA/netboot/boot/grub/grub.cfg" || return
	fw=$2
	bootfile=$3
	[ "$fw" = bios ] || need_ovmf || return
	tftp=$W/$1-tftp
	rm -rf "$tftp"
	cp -R "$MEDIA/netboot" "$tftp"
	# Test only: netbooted nodes get the key on the kernel command line.
	sed -i "s|^set savior_cmdline=\"|set savior_cmdline=\"savior.swarm_key=$KEY savior_dumplog=1 |" "$tftp/boot/grub/grub.cfg"
	grep -q "savior.swarm_key=$KEY" "$tftp/boot/grub/grub.cfg" || { fail "could not patch the netboot grub.cfg"; return; }
	serial=$LOGS/$1.serial
	set -- -m "$MEM" -smp 1 -vga std -netdev "user,id=n0,tftp=$tftp,bootfile=$bootfile" -device e1000,netdev=n0,bootindex=0
	[ "$fw" = bios ] || set -- -bios "$OVMF" "$@"
	vm "$CUR" "$serial" "$@"
	# GRUB's BIOS PXE stack fetches the payload slowly under TCG (minutes).
	wait_boot "$serial" "$VM_PID" $((BOOT_TIMEOUT * 2)) || return
	check_report cfg=no key=yes media=none net=10.0.2.15 console=yes fb=yes
	check_respawn "$serial"
}

t_pxe_bios() { pxe pxe-bios bios boot/grub/i386-pc/core.0; }
t_pxe_uefi() { pxe pxe-uefi uefi boot/grub/x86_64-efi/core.efi; }

t_screen() {
	need_file "$MEDIA/savior.img" || return
	img=$W/screen.img
	stick_copy "$img"
	stick_conf "$img@@$MT_OFF" "swarm_key = $KEY" || { fail "cannot edit savior.conf"; return; }
	serial=$LOGS/screen.serial
	# shellcheck disable=SC2046
	vm screen "$serial" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 $(usb_stick_args "$img" 0)
	wait_boot "$serial" "$VM_PID" || return
	check_report fb=yes
	sleep 15
	ppm=$LOGS/screen.ppm
	if ! screendump screen "$ppm"; then
		fail "screendump through the QEMU monitor failed"
		return
	fi
	# shellcheck disable=SC2046
	set -- $(ppm_stats "$ppm")
	pct=$(($3 * 1000 / $4))
	pass "screendump $1x$2 saved as $ppm ($((pct / 10)).$((pct % 10))% non-zero bytes)"
	if [ "$EXPECT_DISPLAY" = yes ]; then
		check "the screen shows a picture (>= 1% non-black)" [ "$pct" -ge 10 ]
	else
		info "pixel check skipped (--expect-display enables it)"
	fi
}

# ---------------------------------------------------------------------------
# Swarm

# ctl ARGS...: savior ctl against the hive VM, output in $W/ctl.out.
ctl() {
	echo "\$ savior ctl $*" >>"$TLOG"
	"$CTL" ctl --config "$W/ctl.json" --hive "127.0.0.1:$HIVE_PORT" --token "$ADMIN_TOKEN" "$@" >"$W/ctl.out" 2>"$W/ctl.err"
	_rc=$?
	sed 's/^/    > /' "$W/ctl.out" "$W/ctl.err" | head -n 40 >>"$TLOG"
	return "$_rc"
}

# online_nodes: number of online nodes (needs jq).
online_nodes() {
	ctl nodes --all --json || ctl nodes --json || { echo 0; return; }
	jq '[.[] | select(.liveness == "online")] | length' "$W/ctl.out" 2>/dev/null || echo 0
}

rand_byte() { od -An -N1 -tu1 /dev/urandom | tr -d ' '; }

# swarm_vm NAME MAC UUID IMG MEM [EXTRA QEMU ARGS]: a VM on the swarm LAN.
swarm_vm() {
	_n=$1 _mac=$2 _uuid=$3 _img=$4 _mem=$5
	shift 5
	# shellcheck disable=SC2046
	vm "$_n" "$LOGS/${CUR%%/*}-$_n.serial" -m "$_mem" -smp 1 -vga std -uuid "$_uuid" \
		-netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" -device "e1000,netdev=lan,mac=$_mac" \
		$(usb_stick_args "$_img" 0) "$@"
}

t_swarm() {
	need_file "$MEDIA/savior.img" || return
	CTL=$OUT/savior
	need_file "$CTL" || return
	command -v jq >/dev/null 2>&1 || { fail "the swarm test needs jq (apt install jq)"; return; }
	MCAST="230.$(rand_byte).$(rand_byte).$(rand_byte):$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))"
	HIVE_PORT=$((40000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
	ADMIN_TOKEN=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')
	info "LAN mcast=$MCAST, hive API on 127.0.0.1:$HIVE_PORT"

	# Sticks. The hive's copy gets 2 GiB of (sparse) free space for
	# SAVIOR-DATA: blob uploads need max(5%, 512 MiB) free (DESIGN 7.1).
	for n in hive c1 c2 disp dup; do
		stick_copy "$W/$n.img"
	done
	truncate -s +2G "$W/hive.img"
	common="swarm_key = $KEY"
	stick_conf "$W/hive.img@@$MT_OFF" "$common" "name = hive" "roles = hive,compute" "net = static" \
		"ip = 10.77.0.1/24" "dhcp_server = yes" "admin_token = $ADMIN_TOKEN" || { fail "stick setup"; return; }
	stick_conf "$W/c1.img@@$MT_OFF" "$common" "name = c1" "roles = compute" || { fail "stick setup"; return; }
	stick_conf "$W/c2.img@@$MT_OFF" "$common" "name = c2" "roles = compute" || { fail "stick setup"; return; }
	stick_conf "$W/disp.img@@$MT_OFF" "$common" "name = disp" "roles = display" || { fail "stick setup"; return; }
	# Same MAC and SMBIOS UUID as c1 (same node ID), but its own address.
	stick_conf "$W/dup.img@@$MT_OFF" "$common" "name = c1dup" "roles = compute" "net = static" \
		"ip = 10.77.0.250/24" || { fail "stick setup"; return; }
	for n in hive c1 c2 disp dup; do
		boot_options "$W/$n.img@@$MT_OFF"
	done

	U=5a510000-0000-4000-8000-0000000000
	swarm_vm hive 52:54:00:77:00:01 "${U}01" "$W/hive.img" 1024 \
		-netdev "user,id=up,hostfwd=tcp:127.0.0.1:$HIVE_PORT-:7700" -device e1000,netdev=up,mac=52:54:00:77:01:01
	HIVE_PID=$VM_PID
	: >"$W/swarm.pids"
	swarm_vm c1 52:54:00:77:00:02 "${U}02" "$W/c1.img" "$MEM"
	echo "c1 $VM_PID" >>"$W/swarm.pids"
	swarm_vm c2 52:54:00:77:00:03 "${U}03" "$W/c2.img" "$MEM"
	echo "c2 $VM_PID" >>"$W/swarm.pids"
	swarm_vm disp 52:54:00:77:00:04 "${U}04" "$W/disp.img" "$MEM"
	echo "disp $VM_PID" >>"$W/swarm.pids"

	CUR=swarm/hive
	wait_boot "$LOGS/swarm-hive.serial" "$HIVE_PID" || { CUR=swarm; return; }
	check_report 'net~^10\.77\.0\.1$' cfg=yes key=yes
	for n in c1 c2 disp; do
		CUR=swarm/$n
		pid=$(awk -v n="$n" '$1 == n { print $2 }' "$W/swarm.pids")
		# DHCP from the hive's udhcpd (dhcp_server = yes) on the LAN.
		wait_boot "$LOGS/swarm-$n.serial" "$pid" && check_report 'net~^10\.77\.0\.[0-9]+$' key=yes
	done
	CUR=swarm
	sleep 10
	info "hive syslog: $(grep -a -o 'SAVIOR-SYSLOG: .*\(hive data\|SAVIOR-DATA\|init-data\)[^\r]*' "$LOGS/swarm-hive.serial" | tail -n 2 | tr '\n' ' ')"

	# The hive API through the forwarded port (hostfwd to its second NIC).
	if command -v curl >/dev/null 2>&1; then
		t0=$(date +%s)
		until curl -s -k -m 5 -o /dev/null "https://127.0.0.1:$HIVE_PORT/api/v1/hello"; do
			if [ $(($(date +%s) - t0)) -ge 240 ]; then
				fail "the hive API (https://127.0.0.1:$HIVE_PORT -> hive:7700) did not answer within 240 s"
				return
			fi
			sleep 5
		done
		pass "hive API answers on the forwarded port after $(($(date +%s) - t0)) s"
	fi

	# All four nodes online (the hive also runs a compute node).
	t0=$(date +%s)
	n=0
	while [ $(($(date +%s) - t0)) -lt 420 ]; do
		n=$(online_nodes)
		[ "$n" -ge 4 ] && break
		sleep 10
	done
	if [ "$n" -ge 4 ]; then
		pass "4 nodes online after $(($(date +%s) - t0)) s"
	else
		fail "only $n nodes online after $(($(date +%s) - t0)) s ($(tail -n 1 "$W/ctl.err" 2>/dev/null))"
		return
	fi
	cp "$W/ctl.out" "$LOGS/swarm-nodes.json"

	# A job: 4 tasks that write an output file each.
	cat >"$W/job.json" <<'EOF'
{
  "name": "qemu-swarm-test",
  "script": "echo \"task $SAVIOR_TASK_INDEX of $SAVIOR_TASK_COUNT\" > out.txt\nuname -m >> out.txt\n",
  "outputs": ["out.txt"],
  "count": 4,
  "resources": {"cores": 0.5, "mem_mb": 64, "disk_mb": 16},
  "requirements": {},
  "timeout_s": 300
}
EOF
	rm -rf "$W/outputs"
	t0=$(date +%s)
	if ctl --timeout 900s submit "$W/job.json" --fetch "$W/outputs"; then
		pass "job with count=4 succeeded in $(($(date +%s) - t0)) s"
	else
		fail "job failed or timed out: $(tail -n 3 "$W/ctl.err" | tr '\n' ' ')"
	fi
	found=0
	for i in 0 1 2 3; do
		f=$(grep -r -l -x "task $i of 4" "$W/outputs" 2>/dev/null | head -n 1)
		[ -n "$f" ] && found=$((found + 1))
	done
	check "outputs of all 4 tasks fetched with the right contents ($found/4)" [ "$found" -eq 4 ]

	# The display node shows text; the screen changes and is not black.
	disp_id=$(jq -r '[.[] | select(.name == "disp")][0].id // empty' "$LOGS/swarm-nodes.json")
	[ -n "$disp_id" ] || disp_id=disp
	screendump disp "$LOGS/swarm-disp-before.ppm"
	if ctl display "$disp_id" text --text "HELLO SWARM"; then
		pass "display set to text 'HELLO SWARM'"
	else
		fail "ctl display failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	sleep 20
	if screendump disp "$LOGS/swarm-disp.ppm"; then
		# shellcheck disable=SC2046
		set -- $(ppm_stats "$LOGS/swarm-disp.ppm")
		check "display screen has non-black pixels ($3 of $4 bytes)" [ "$3" -gt 0 ]
		check "display screen changed after the text was set" \
			differs "$LOGS/swarm-disp-before.ppm" "$LOGS/swarm-disp.ppm"
	else
		fail "screendump of the display node failed"
	fi

	check "identify all nodes" ctl identify --all --seconds 10

	# Duplicate node ID: a second machine with c1's MAC and UUID.
	swarm_vm dup 52:54:00:77:00:02 "${U}02" "$W/dup.img" "$MEM"
	DUP_PID=$VM_PID
	wait_boot "$LOGS/swarm-dup.serial" "$DUP_PID" || return
	if wait_for "$LOGS/swarm-dup.serial" 'duplicate' 240 "$DUP_PID"; then
		pass "the second machine with the same ID reports the duplicate state"
	else
		fail "no 'duplicate' state on the duplicate machine's console within 240 s"
	fi
	n=$(online_nodes)
	check "still 4 nodes online, the original kept its ID ($n)" [ "$n" -eq 4 ]
}

# hive-pxe: a hive with netboot = yes (S65netboot: dnsmasq DHCP + TFTP,
# generated grub.cfg) boots a diskless machine on the private LAN. The
# netbooted node joins keyless (no swarm key over the network), shows up as
# pending and is approved with savior ctl.
t_hive_pxe() {
	need_file "$MEDIA/savior.img" || return
	CTL=$OUT/savior
	need_file "$CTL" || return
	command -v jq >/dev/null 2>&1 || { fail "this test needs jq (apt install jq)"; return; }
	MCAST="230.$(rand_byte).$(rand_byte).$(rand_byte):$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))"
	HIVE_PORT=$((40000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
	ADMIN_TOKEN=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')
	info "LAN mcast=$MCAST, hive API on 127.0.0.1:$HIVE_PORT"
	stick_copy "$W/nbhive.img"
	truncate -s +2G "$W/nbhive.img"
	stick_conf "$W/nbhive.img@@$MT_OFF" "swarm_key = $KEY" "name = hive" "roles = hive" "net = static" \
		"ip = 10.77.0.1/24" "dhcp_server = yes" "netboot = yes" "admin_token = $ADMIN_TOKEN" ||
		{ fail "stick setup"; return; }
	boot_options "$W/nbhive.img@@$MT_OFF"
	U=5a510000-0000-4000-8000-0000000001
	swarm_vm hive 52:54:00:78:00:01 "${U}01" "$W/nbhive.img" 1024 \
		-netdev "user,id=up,hostfwd=tcp:127.0.0.1:$HIVE_PORT-:7700" -device e1000,netdev=up,mac=52:54:00:78:01:01
	HIVE_PID=$VM_PID
	CUR=hive-pxe/hive
	wait_boot "$LOGS/hive-pxe-hive.serial" "$HIVE_PID" || { CUR=hive-pxe; return; }
	CUR=hive-pxe
	if wait_for "$LOGS/hive-pxe-hive.serial" 'serving netboot files|netboot: serving PXE' 240 "$HIVE_PID"; then
		pass "the hive serves netboot files"
	else
		info "no netboot log line from the hive (checking the client anyway)"
	fi
	# Diskless clients, BIOS then UEFI: PXE from the hive's dnsmasq.
	for fw in bios uefi; do
		if [ "$fw" = uefi ] && ! need_ovmf; then
			continue
		fi
		pxe_client "$fw" || return
	done
}

# pxe_client FIRMWARE: one diskless VM of t_hive_pxe; its node must join
# keyless, be pending, and come online once approved.
pxe_client() {
	serial=$LOGS/hive-pxe-client-$1.serial
	fw=$1
	n=1
	[ "$fw" = bios ] || n=2
	set -- -m "$MEM" -smp 1 -vga std -uuid "${U}1$n" \
		-netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
		-device "e1000,netdev=lan,mac=52:54:00:78:00:1$n,bootindex=0"
	[ "$fw" = bios ] || set -- -bios "$OVMF" "$@"
	vm "client-$fw" "$serial" "$@"
	CUR=hive-pxe/$fw
	if ! wait_boot "$serial" "$VM_PID" $((BOOT_TIMEOUT * 3)); then
		CUR=hive-pxe
		return 1
	fi
	check_report media=none key=no 'net~^10\.77\.0\.[0-9]+$'
	t0=$(date +%s)
	id=""
	while [ $(($(date +%s) - t0)) -lt 300 ]; do
		if ctl nodes --all --json; then
			id=$(jq -r '[.[] | select(.approved == false)][0].id // empty' "$W/ctl.out")
			[ -n "$id" ] && break
		fi
		sleep 10
	done
	if [ -z "$id" ]; then
		fail "no pending (keyless) node on the hive within 300 s"
		CUR=hive-pxe
		return 1
	fi
	pass "the netbooted node joined keyless and is pending approval ($id)"
	check "approve the netbooted node" ctl approve "$id"
	t0=$(date +%s)
	ok=no
	while [ $(($(date +%s) - t0)) -lt 120 ]; do
		if ctl nodes --all --json &&
			[ "$(jq -r --arg id "$id" '.[] | select(.id == $id) | "\(.approved) \(.liveness)"' "$W/ctl.out")" = "true online" ]; then
			ok=yes
			break
		fi
		sleep 5
	done
	check "the approved node is online" [ "$ok" = yes ]
	CUR=hive-pxe
}

# ---------------------------------------------------------------------------

START=$(date +%s)
echo "qemu-test: $QEMU (TCG), media from $MEDIA, logs in $LOGS"
for t in $TESTS; do
	begin_test "$t"
	case "$t" in
	boot-bios) t_boot_bios ;;
	boot-uefi) t_boot_uefi ;;
	boot-iso-bios) t_boot_iso_bios ;;
	boot-iso-uefi) t_boot_iso_uefi ;;
	pxe-bios) t_pxe_bios ;;
	pxe-uefi) t_pxe_uefi ;;
	screen) t_screen ;;
	baked-conf) t_baked_conf ;;
	swarm) t_swarm ;;
	hive-pxe) t_hive_pxe ;;
	esac
	end_test
done

echo
echo "qemu-test summary ($(($(date +%s) - START)) s):"
printf '%s' "$SUMMARY"
if [ "$TOTAL_FAIL" -eq 0 ]; then
	echo "all tests passed"
	exit 0
fi
echo "$TOTAL_FAIL test(s) failed; see $LOGS"
exit 1
