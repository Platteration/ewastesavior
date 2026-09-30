#!/bin/sh
# os/dev/qemu-test.sh - QEMU tests of the SaviorOS dev image (DESIGN 14).
#
# Usage:
#   os/dev/qemu-test.sh [options] <test>...
#
# Tests:
#   image          the payload without booting it: every soft dependency of
#                  a module in the modloop is there too (r8169 -> realtek,
#                  and BusyBox modprobe finds realtek for a Realtek PHY),
#                  radeon has all its firmware, and the CA bundle is
#                  Mozilla's set (not the build host's)
#   boot-bios      USB stick (usb-storage on EHCI), SeaBIOS; the stick sets
#                  ssh_key: root logs in over SSH with that key (dropbear,
#                  port 22 forwarded), another key is refused
#   boot-uefi      USB stick, OVMF (x86_64 UEFI), the same SSH checks
#   boot-iso-bios  the ISO as an IDE CD-ROM, SeaBIOS (config from the CD)
#   boot-iso-uefi  the ISO as a CD-ROM, OVMF, plus a plain FAT stick with
#                  savior.conf (a CD-booted machine takes that config)
#   pxe-bios       PXE via QEMU's TFTP (netboot tree, core.0)
#   pxe-uefi       PXE via QEMU's TFTP, OVMF (core.efi)
#   screen         boot, then screendump through the QEMU monitor (PPM)
#   baked-conf     mkimage --conf: the config baked into savior-conf.cpio
#                  (second initrd) is used when the stick has no savior.conf
#   mem-256        the stick on a 256 MB machine with a screen (roles auto:
#                  compute + display): MemAvailable at idle and the memory
#                  the agent offers for tasks (boot-report's mem_avail= and
#                  offer_mem=) must reach MEM256_MIN_AVAIL and
#                  MEM256_MIN_OFFER (environment; defaults 100 and 24 MB for
#                  the dev image, which measures 111-113 and 30-32)
#   wifi-conf      no VM: write_wpa_conf from S30network, run with the
#                  image's BusyBox: hex SSID, the hashed PSK (checked
#                  against PBKDF2), the quoted fallback without
#                  wpa_passphrase and its refusal of '"', open networks,
#                  country, mode 0600 (needs wpa_passphrase and python3)
#   wifi           Wi-Fi join with mac80211_hwsim: an access point (hostapd
#                  + udhcpd from this host, in its own network namespace on
#                  the second simulated radio) and the node on the first,
#                  WPA2 with an SSID and passphrase full of quotes, then an
#                  open network; the node must get a DHCP lease over the
#                  air (needs hostapd, wpa_supplicant, iw on this host)
#   swarm          hive + 2 compute nodes + 1 display node on a private
#                  multicast LAN, driven with `savior ctl` from this host:
#                  job with count=4 and outputs (the tasks must see the CA
#                  bundle and the sandbox must hold: task uid, no
#                  /proc/cmdline, /run, /media or /sys, unshare and mount
#                  fail, a seccomp filter), display black then text
#                  (compared pixel for pixel with savior display render),
#                  identify, a reboot of the hive (its state on SAVIOR-DATA,
#                  certificate, jobs, outputs, names and SSH host key must
#                  survive), duplicate node ID
#   hive-pxe       a hive with netboot = yes PXE-boots diskless VMs (BIOS,
#                  then UEFI) on a private LAN (S65netboot: dnsmasq DHCP +
#                  TFTP, kernels over HTTP); each node joins keyless, is
#                  pending, and gets approved with savior ctl
#   hive-pxe-proxy netboot next to the LAN's own DHCP server (proxy DHCP,
#                  the default): a "router" VM hands out the addresses
#                  (SaviorOS udhcpd, then a dnsmasq whose replies name
#                  itself as next-server, like OpenWrt), the hive answers
#                  PXE only; BIOS and UEFI clients boot and join. A PXE
#                  probe on the LAN checks that plain PXE ROMs get iPXE
#                  (undionly.kpxe) and iPXE gets savior.ipxe; QEMU's own
#                  NIC ROMs are iPXE, so no VM takes the undionly path.
#                  Last, a hive with dhcp_server = yes but net = dhcp
#                  must stay a proxy and say why.
#   all            every test above except swarm, hive-pxe and
#                  hive-pxe-proxy (--swarm adds them)
#
# Options:
#   --out DIR          dev image directory (default build/dev); logs go to
#                      DIR/logs/<test>.log, serial consoles to
#                      DIR/logs/<test>[-vm].serial, screendumps to *.ppm
#   --keep             keep the per-run work directory (stick copies etc.)
#   --swarm            include swarm, hive-pxe and hive-pxe-proxy in "all"
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
# and, once savior node has kept running for 10 s (or has not):
#   SAVIOR-AGENT: up=.. agent=up age=.. starts=1 arch=x86_64 ver=..
#                 mem_avail=.. offer_mem=..
# savior node's own log (stderr) also goes to the serial console, so the
# harness sees every start ('savior node starting') and crash.

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
ALL_TESTS="image boot-bios boot-uefi boot-iso-bios boot-iso-uefi pxe-bios pxe-uefi screen baked-conf mem-256 wifi-conf wifi"
MEM256_MIN_AVAIL=${MEM256_MIN_AVAIL:-100}
MEM256_MIN_OFFER=${MEM256_MIN_OFFER:-24}

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
case "$BOOT_TIMEOUT$MEM$MEM256_MIN_AVAIL$MEM256_MIN_OFFER" in
*[!0-9]*) echo "qemu-test: --timeout, --mem, MEM256_MIN_AVAIL and MEM256_MIN_OFFER take numbers" >&2; exit 2 ;;
esac
[ -n "$TESTS" ] || { usage >&2; exit 2; }
expanded=""
for t in $TESTS; do
	case "$t" in
	all)
		expanded="$expanded $ALL_TESTS"
		[ "$SWARM" = no ] || expanded="$expanded swarm hive-pxe hive-pxe-proxy"
		;;
	image | boot-bios | boot-uefi | boot-iso-bios | boot-iso-uefi | pxe-bios | pxe-uefi | screen | baked-conf | \
		mem-256 | wifi-conf | wifi | swarm | hive-pxe | hive-pxe-proxy)
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

# node_starts SERIAL: how often savior node has started. The run wrapper
# sends the agent's stderr (its log) to the serial console when the kernel
# console is ttyS0, as in every SaviorOS menu entry.
node_starts() { grep -a -c 'msg="savior node starting"' "$1"; }

# NODE_EXIT_RE: signs on the serial console that savior node exited or
# crashed: its last log line, a startup error, a Go crash (SIGILL on a CPU
# without SSE2, panics), or a restart logged by the run wrapper (in the
# syslog that savior_dumplog copies to the console).
NODE_EXIT_RE='msg="(node agent stopped|cannot start the node agent)"|^savior node: |^(panic|fatal error): |^SIG[A-Z]+: |savior-run: node exited'
node_exits() { grep -a -c -E "$NODE_EXIT_RE" "$1"; }

# check_agent SERIAL PID: boot-report's SAVIOR-AGENT line must say that
# savior node wrote its status and kept running for 10 s without a restart
# (agent=up, starts=1), on x86_64, and that /usr/bin/savior runs (ver=).
# node=yes in the boot line only means the run wrapper started it once.
check_agent() {
	if ! wait_for "$1" 'SAVIOR-AGENT: ' 150 "$2"; then
		fail "no SAVIOR-AGENT line (boot-report) within 150 s of the boot report"
		return 1
	fi
	_bl=$BOOTLINE
	BOOTLINE=$(grep -a 'SAVIOR-AGENT: ' "$1" | tail -n 1 | tr -d '\r')
	AGENTLINE=$BOOTLINE
	info "$BOOTLINE"
	check_report agent=up starts=1 arch=x86_64
	_v=$(field ver)
	check "savior runs on the guest (ver = '$_v')" [ "${_v:-unknown}" != unknown ]
	BOOTLINE=$_bl
}

# check_respawn SERIAL PID: savior node came up (check_agent), and 30 s
# later it has still started only once and nothing on the console says it
# exited or crashed. A crashing agent is restarted every 5-15 s by the run
# wrapper, so a crash loop shows up as more starts and exit lines.
check_respawn() {
	check_agent "$1" "$2"
	_s0=$(node_starts "$1")
	sleep 30
	_s1=$(node_starts "$1")
	_e1=$(node_exits "$1")
	check "no respawn: savior node started once ($_s1 starts, $((_s1 - _s0)) in the last 30 s)" [ "$_s1" -eq 1 ]
	check "no respawn: savior node never exited or crashed ($_e1 exit or crash lines)" [ "$_e1" -eq 0 ]
	if [ "$_e1" -gt 0 ]; then
		grep -a -E "$NODE_EXIT_RE" "$1" | head -n 5 | tr -d '\r' | sed 's/^/        | /' | tee -a "$TLOG"
	fi
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

# img_diff PNG PPM: the number of pixels that differ between an 8-bit RGB
# or RGBA PNG (savior display render) and a P6 PPM screendump, or "size"
# when their sizes differ (needs python3).
img_diff() {
	python3 - "$1" "$2" <<'PY'
import struct, sys, zlib

def png_rgb(path):
    d = open(path, "rb").read()
    if d[:8] != b"\x89PNG\r\n\x1a\n":
        sys.exit("not a PNG: " + path)
    i, idat = 8, b""
    while i < len(d):
        n, t = struct.unpack("!I4s", d[i:i + 8])
        c = d[i + 8:i + 8 + n]
        if t == b"IHDR":
            w, h, depth, ctype, _, _, lace = struct.unpack("!IIBBBBB", c)
            if depth != 8 or ctype not in (2, 6) or lace:
                sys.exit("unsupported PNG: depth %d, color type %d, interlace %d" % (depth, ctype, lace))
            bpp = 3 if ctype == 2 else 4
        elif t == b"IDAT":
            idat += c
        i += 12 + n
    raw = zlib.decompress(idat)
    stride = w * bpp
    out = bytearray()
    prev = bytearray(stride)
    p = 0
    for _ in range(h):
        f = raw[p]
        line = bytearray(raw[p + 1:p + 1 + stride])
        p += 1 + stride
        if f:
            for x in range(stride):
                a = line[x - bpp] if x >= bpp else 0
                b = prev[x]
                c = prev[x - bpp] if x >= bpp else 0
                if f == 1:
                    line[x] = (line[x] + a) & 255
                elif f == 2:
                    line[x] = (line[x] + b) & 255
                elif f == 3:
                    line[x] = (line[x] + ((a + b) >> 1)) & 255
                elif f == 4:
                    pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
                    line[x] = (line[x] + (a if pa <= pb and pa <= pc else b if pb <= pc else c)) & 255
        prev = line
        if bpp == 3:
            out += line
        else:
            for x in range(0, stride, 4):
                out += line[x:x + 3]
    return w, h, bytes(out)

def ppm_rgb(path):
    d = open(path, "rb").read()
    parts, i = [], 0
    while len(parts) < 4:
        while d[i:i + 1].isspace():
            i += 1
        j = i
        while not d[j:j + 1].isspace():
            j += 1
        parts.append(d[i:j])
        i = j
    if parts[0] != b"P6" or parts[3] != b"255":
        sys.exit("not a P6 PPM: " + path)
    return int(parts[1]), int(parts[2]), d[i + 1:]

pw, ph, a = png_rgb(sys.argv[1])
sw, sh, b = ppm_rgb(sys.argv[2])
if (pw, ph) != (sw, sh) or len(a) != len(b):
    print("size")
    sys.exit(0)
print(sum(1 for k in range(0, len(a), 3) if a[k:k + 3] != b[k:k + 3]))
PY
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

# rand_port: a TCP port on this host for a hostfwd, 20000-39999 (the hive
# API ports are 40000-59999).
rand_port() { echo $((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000)); }

# ---------------------------------------------------------------------------
# SSH (ssh_key, S50sshd, dropbear)

# ssh_keys: two ed25519 key pairs, $W/id (goes into ssh_key) and
# $W/id-other (must be refused); 1 without OpenSSH's client.
ssh_keys() {
	[ -s "$W/id.pub" ] && [ -s "$W/id-other.pub" ] && return 0
	if ! command -v ssh >/dev/null 2>&1 || ! command -v ssh-keygen >/dev/null 2>&1; then
		fail "the SSH checks need ssh and ssh-keygen (apt install openssh-client)"
		return 1
	fi
	rm -f "$W/id" "$W/id.pub" "$W/id-other" "$W/id-other.pub"
	ssh-keygen -q -t ed25519 -N '' -C qemu-test -f "$W/id" >>"$TLOG" 2>&1 &&
		ssh-keygen -q -t ed25519 -N '' -C not-authorized -f "$W/id-other" >>"$TLOG" 2>&1 &&
		return 0
	fail "ssh-keygen failed"
	return 1
}

# ssh_to PORT KEY COMMAND: run COMMAND as root on 127.0.0.1:PORT (a
# hostfwd to a guest's port 22) with only KEY, no agent, no password
# prompt; stdout and stderr in $W/ssh.out and $W/ssh.err.
ssh_to() {
	ssh -p "$1" -i "$2" -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=20 \
		-o LogLevel=ERROR "root@127.0.0.1" "$3" </dev/null >"$W/ssh.out" 2>"$W/ssh.err"
}

# check_ssh PORT: within 120 s, root logs in with $W/id (the stick's
# ssh_key) and `id -u` says 0; $W/id-other is refused, and the server
# offers no method but publickey.
check_ssh() {
	t0=$(date +%s)
	ok=no
	while [ $(($(date +%s) - t0)) -lt 120 ]; do
		if ssh_to "$1" "$W/id" 'id -u; cat /etc/savior-release' && [ "$(head -n 1 "$W/ssh.out")" = 0 ]; then
			ok=yes
			break
		fi
		sleep 5
	done
	sed 's/^/    ssh> /' "$W/ssh.out" "$W/ssh.err" >>"$TLOG"
	if [ "$ok" = yes ]; then
		pass "root logs in over SSH with the ssh_key key after $(($(date +%s) - t0)) s ($(sed -n 2p "$W/ssh.out"))"
	else
		fail "no root login over SSH with the ssh_key key within 120 s: $(tail -n 2 "$W/ssh.err" | tr '\n' ' ')"
		return 1
	fi
	ssh_to "$1" "$W/id-other" 'id -u'
	_rc=$?
	sed 's/^/    ssh(other key)> /' "$W/ssh.out" "$W/ssh.err" >>"$TLOG"
	check "a key that is not in ssh_key is refused, and only publickey is offered (exit $_rc: $(tr '\n' ' ' <"$W/ssh.err"))" \
		grep -q 'Permission denied (publickey)' "$W/ssh.err"
}

# host_key PORT: the SSH server's ed25519 host key on 127.0.0.1:PORT.
host_key() {
	ssh-keyscan -p "$1" -t ed25519 -T 20 127.0.0.1 2>/dev/null | awk '$2 == "ssh-ed25519" { print $3; exit }'
}

# ---------------------------------------------------------------------------
# Tests

# image: the payload, checked without booting it. The initrd's
# /lib/modloop.sqfs must hold every soft dependency of its modules (modprobe
# loads them, and r8169 finds no PHY driver without realtek), and the
# image's own BusyBox modprobe must find realtek for a Realtek PHY (the
# kernel asks for "mdio:<PHY ID in binary>"; run with -D, which loads
# nothing, in a chroot). radeon must have every firmware file it asks for:
# it removes the firmware framebuffer before it loads them. The CA bundle
# must be Mozilla's set and hold none of the build host's own CAs
# (/usr/local/share/ca-certificates, e.g. a proxy's).
t_image() {
	need_file "$OUT/initrd" || return
	for _t in unsquashfs modinfo cpio; do
		command -v "$_t" >/dev/null 2>&1 || { fail "this test needs $_t (squashfs-tools, kmod, cpio)"; return; }
	done
	x=$W/image
	rm -rf "$x"
	mkdir -p "$x/root" "$x/chroot/bin"
	(cd "$x/root" && xz -dc "$OUT/initrd" | cpio -id --quiet 'lib/modloop.sqfs' 'bin/busybox' 'etc/ssl/*') 2>>"$TLOG"
	if [ ! -s "$x/root/lib/modloop.sqfs" ]; then
		fail "the initrd has no /lib/modloop.sqfs"
		return
	fi
	unsquashfs -q -n -d "$x/chroot/lib" "$x/root/lib/modloop.sqfs" >>"$TLOG" 2>&1 || { fail "unsquashfs failed"; return; }
	kv=""
	for _d in "$x/chroot/lib/modules"/*; do
		kv=${_d##*/}
		break
	done
	md=$x/chroot/lib/modules/$kv
	info "modloop: kernel $kv, $(find "$md" -name '*.ko' | wc -l) modules, $(find "$x/chroot/lib/firmware" -type f | wc -l) firmware files"

	# Soft dependencies: depmod wrote those of the modloop's modules to its
	# modules.softdep. Each must be a module in the modloop (by name or
	# exact alias) or built in.
	find "$md" -name '*.ko' | sed 's|.*/||; s|\.ko$||; s|-|_|g' | sort -u >"$x/names"
	sed 's|.*/||; s|\.ko$||; s|-|_|g' "$md/modules.builtin" >>"$x/names"
	awk '$1 == "alias" { a = $2; gsub(/-/, "_", a); print a }' "$md/modules.alias" >>"$x/names"
	awk '$1 == "softdep" { m = $2; on = 0; for (i = 3; i <= NF; i++) { if ($i == "pre:" || $i == "post:") on = 1; else if (on) print m, $i } }' \
		"$md/modules.softdep" >"$x/softdeps"
	missing=""
	while read -r _m _d; do
		grep -q -x -F "$(printf '%s\n' "$_d" | tr - _)" "$x/names" || missing="$missing $_m->$_d"
	done <"$x/softdeps"
	info "soft dependencies: $(awk '{ printf "%s->%s ", $1, $2 }' "$x/softdeps")"
	check "every soft dependency of a module in the modloop is in it (missing:${missing:- none})" [ -z "$missing" ]
	if [ -n "$(find "$md" -name r8169.ko)" ]; then
		check "r8169's PHY driver realtek.ko is in the modloop" [ -n "$(find "$md" -name realtek.ko)" ]
		# RTL8211E, PHY ID 0x001cc915.
		cp "$x/root/bin/busybox" "$x/chroot/bin/busybox"
		[ "$kv" = "$(uname -r)" ] || ln -s "$kv" "$x/chroot/lib/modules/$(uname -r)"
		set -- chroot
		[ "$(id -u)" = 0 ] || set -- unshare -r chroot
		if "$@" "$x/chroot" /bin/busybox true 2>/dev/null; then
			out=$("$@" "$x/chroot" /bin/busybox modprobe -D mdio:00000000000111001100100100010101 2>&1)
			printf '%s\n' "$out" | sed 's/^/        | /' >>"$TLOG"
			ok=no
			case "$out" in *"/realtek.ko"*) ok=yes ;; esac
			check "BusyBox modprobe finds realtek.ko for a Realtek PHY (mdio: alias): $out" [ "$ok" = yes ]
		else
			info "cannot chroot here (not root, no unshare -r): BusyBox modprobe not tried"
		fi
	fi

	# radeon's firmware.
	ko=$(find "$md" -name radeon.ko | head -n 1)
	if [ -n "$ko" ]; then
		n=0
		absent=0
		for f in $(modinfo -F firmware "$ko"); do
			n=$((n + 1))
			[ -s "$x/chroot/lib/firmware/$f" ] || absent=$((absent + 1))
		done
		check "radeon has all the firmware it asks for ($((n - absent)) of $n files in firmware/)" [ $((n > 0 && absent == 0)) -eq 1 ]
	else
		info "no radeon.ko in the modloop"
	fi

	# The CA bundle.
	ca=$x/root/etc/ssl/certs/ca-certificates.crt
	if [ ! -s "$ca" ]; then
		fail "no /etc/ssl/certs/ca-certificates.crt in the initrd"
		return
	fi
	n=$(grep -c -e '-----BEGIN CERTIFICATE-----' "$ca")
	check "the CA bundle holds Mozilla's set ($n certificates, want >= 100)" [ "$n" -ge 100 ]
	check "the CA bundle is readable by everyone (mode $(stat -c %a "$ca"))" [ "$(stat -c %a "$ca")" = 644 ]
	# pem_bodies FILE...: one line per certificate, its base64 text.
	pem_bodies() {
		awk '/-----BEGIN CERTIFICATE-----/ { b = ""; on = 1; next }
			/-----END CERTIFICATE-----/ { if (on) print b; on = 0; next }
			on { gsub(/[ \t\r]/, ""); b = b $0 }' "$@"
	}
	pem_bodies "$ca" | sort -u >"$x/ca.bodies"
	set -- /usr/local/share/ca-certificates/*.crt /usr/local/share/ca-certificates/*/*.crt
	local_n=0
	leaked=0
	for f in "$@"; do
		[ -f "$f" ] || continue
		local_n=$((local_n + 1))
		pem_bodies "$f" | sort -u | comm -12 - "$x/ca.bodies" | grep -q . && leaked=$((leaked + 1))
	done
	check "none of the build host's $local_n local CAs is in the image's CA bundle ($leaked are)" [ "$leaked" -eq 0 ]
}

# boot_stick TEST FIRMWARE: boot a copy of savior.img with a test swarm key
# and a test ssh_key (the guest's port 22 is forwarded to SSH_PORT).
boot_stick() {
	need_file "$MEDIA/savior.img" || return
	fw=$2
	[ "$fw" = bios ] || need_ovmf || return
	ssh_keys || return
	img=$W/$1.img
	stick_copy "$img"
	stick_conf "$img@@$MT_OFF" "swarm_key = $KEY" "ssh_key = $(cat "$W/id.pub")" ||
		{ fail "cannot edit savior.conf on the stick copy"; return; }
	boot_options "$img@@$MT_OFF"
	serial=$LOGS/$1.serial
	SSH_PORT=$(rand_port)
	# shellcheck disable=SC2046 # word-split the device arguments
	set -- -m "$MEM" -smp 1 -vga std -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" -device e1000,netdev=n0 \
		$(usb_stick_args "$img" 0)
	[ "$fw" = bios ] || set -- -bios "$OVMF" "$@"
	vm "$CUR" "$serial" "$@"
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=yes key=yes 'media~^/dev/sd[a-z]+1$' net=10.0.2.15 console=yes fb=yes node=yes
	gl=$(grep -a -o 'SaviorOS: boot medium ([^)]*) found by [a-z]*' "$serial" | tail -n 1)
	info "GRUB: ${gl:-no boot medium line}"
	if [ "$fw" = bios ]; then
		check "GRUB root from the core image prefix (,msdos1)" \
			grep -a -q -E 'boot medium \(hd[0-9]+,msdos1\) found by boot device' "$serial"
	else
		check "GRUB found the stick" grep -a -q 'SaviorOS: boot medium' "$serial"
	fi
	check "/boot-options.cfg sourced by GRUB (boot log dumped)" wait_for "$serial" 'SAVIOR-LOG: ' 30 "$VM_PID"
	check_respawn "$serial" "$VM_PID"
	check_ssh "$SSH_PORT"
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
	# The stick copy warns that commenting out a line keeps the baked value.
	MTOOLS_SKIP_CHECK=1 mtype -i "$img@@$MT_OFF" ::/savior.conf >"$W/baked-stick.conf" 2>/dev/null
	check "the stick's savior.conf says commenting out does not undo a built-in setting" \
		grep -q -F 'does NOT undo a built-in setting' "$W/baked-stick.conf"
	check "the stick's savior.conf holds the --conf settings" grep -q '^name = baked-node' "$W/baked-stick.conf"
	MTOOLS_SKIP_CHECK=1 mdel -i "$img@@$MT_OFF" ::/savior.conf || { fail "cannot remove savior.conf"; return; }
	boot_options "$img@@$MT_OFF"
	serial=$LOGS/$CUR.serial
	# shellcheck disable=SC2046
	vm "$CUR" "$serial" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 $(usb_stick_args "$img" 0)
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=no key=yes 'media~^/dev/sd[a-z]+1$' net=10.0.2.15
	check "S08config used /etc/savior/baked.conf" wait_for "$serial" 'SAVIOR-LOG: .*baked.conf' 30 "$VM_PID"
	check_agent "$serial" "$VM_PID"
}
t_boot_uefi() { boot_stick boot-uefi uefi; }

# num_ge A B: A is a whole number >= B.
num_ge() {
	case "$1" in '' | *[!0-9]*) return 1 ;; esac
	[ "$1" -ge "$2" ]
}

# mem-256: DESIGN 13.2's smallest machine, 256 MB, with a screen (roles =
# auto: compute and display). boot-report's SAVIOR-AGENT line says how much
# memory is available at idle with every role running (mem_avail) and how
# much the agent offers for tasks (offer_mem, DESIGN 10.3: MemAvailable at
# start minus 48 MB for the agent, the framebuffers and the frame cache,
# times max_mem_percent). A task fits when its mem_mb + disk_mb (scratch in
# RAM) is at most offer_mem. DESIGN's target for the production image is
# MemAvailable >= 150 MB, which offers about 50 MB (a --mem 32 --disk 16
# task). The dev image is heavier: the Ubuntu kernel leaves MemTotal at
# 207 MiB of 256, and its initramfs keeps 37 MiB in RAM; it measures
# mem_avail 111-113 and offer_mem 30-32. The defaults of MEM256_MIN_AVAIL
# and MEM256_MIN_OFFER sit about 10 MB below that, so a regression that
# eats memory at idle, or a node that offers nothing, fails.
t_mem_256() {
	need_file "$MEDIA/savior.img" || return
	img=$W/mem-256.img
	stick_copy "$img"
	stick_conf "$img@@$MT_OFF" "swarm_key = $KEY" || { fail "cannot edit savior.conf on the stick copy"; return; }
	boot_options "$img@@$MT_OFF"
	serial=$LOGS/$CUR.serial
	# shellcheck disable=SC2046
	vm "$CUR" "$serial" -m 256 -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 $(usb_stick_args "$img" 0)
	wait_boot "$serial" "$VM_PID" || return
	check_report cfg=yes key=yes fb=yes node=yes
	AGENTLINE=""
	check_agent "$serial" "$VM_PID" || return
	_bl=$BOOTLINE
	BOOTLINE=$AGENTLINE
	avail=$(field mem_avail)
	offer=$(field offer_mem)
	BOOTLINE=$_bl
	# savior node logs roles="[compute display]" when it starts.
	roles=$(grep -a 'msg="savior node starting"' "$serial" | head -n 1 | grep -o 'roles="[^"]*"')
	check "the display role runs (fb=yes, roles = auto): ${roles:-no roles= in the agent log}" \
		eval 'printf "%s\n" "$roles" | grep -q -E "roles=\"\\[([a-z]+ )*display[] ]"'
	check "MemAvailable at idle with the display role >= $MEM256_MIN_AVAIL MB on a 256 MB machine (mem_avail=${avail:-?})" \
		num_ge "$avail" "$MEM256_MIN_AVAIL"
	check "the agent offers >= $MEM256_MIN_OFFER MB for tasks: mem_mb + disk_mb up to that fit (offer_mem=${offer:-?})" \
		num_ge "$offer" "$MEM256_MIN_OFFER"
}

# ---------------------------------------------------------------------------
# Wi-Fi

# WIFI_SSID, WIFI_PSK: an SSID with quotes, spaces and a non-ASCII letter,
# longer than the 16 bytes od prints per line, and a passphrase with every
# character a shell or a config file could mangle. The quote in the
# passphrase rules out S30network's quoted fallback: only a correctly
# hashed psk= (wpa_passphrase) can join.
WIFI_SSID='Lab "5" Ü, 2nd floor'
WIFI_PSK='a"b$c\d e`x`'"'"'f'

# host_prog NAME: the host's NAME, also in the sbin directories.
host_prog() {
	for _d in /usr/sbin /usr/bin /sbin /bin; do
		[ -x "$_d/$1" ] && { echo "$_d/$1"; return 0; }
	done
	command -v "$1" 2>/dev/null
}

# wifi-conf: write_wpa_conf, cut out of the real S30network and run by the
# image's own BusyBox (sh, od, tr, sed, chmod, mv) with log and have
# stubbed. With this host's wpa_passphrase the psk= must be
# PBKDF2-HMAC-SHA1(passphrase, SSID, 4096, 32), the WPA2 PSK (IEEE
# 802.11i), computed here with python3.
t_wifi_conf() {
	need_file "$OUT/initrd" || return
	command -v python3 >/dev/null 2>&1 || { fail "this test needs python3"; return; }
	wpp=$(host_prog wpa_passphrase)
	[ -n "$wpp" ] || { fail "this test needs wpa_passphrase (apt install wpasupplicant)"; return; }
	x=$W/wifi-conf
	rm -rf "$x"
	mkdir -p "$x/root" "$x/bin" "$x/wpp"
	(cd "$x/root" && xz -dc "$OUT/initrd" | cpio -id --quiet bin/busybox) 2>>"$TLOG"
	bb=$x/root/bin/busybox
	[ -x "$bb" ] || { fail "no /bin/busybox in the initrd"; return; }
	for a in sh od tr sed chmod mv cat printf; do
		ln -s "$bb" "$x/bin/$a"
	done
	ln -s "$wpp" "$x/wpp/wpa_passphrase"
	sed -n '/^write_wpa_conf() {$/,/^}$/p' "$REPO/os/rootfs-overlay/etc/init.d/S30network" >"$x/fn.sh"
	check "write_wpa_conf is in S30network ($(wc -l <"$x/fn.sh") lines)" [ "$(wc -l <"$x/fn.sh")" -gt 10 ]
	cat >"$x/run.sh" <<'EOF'
# run.sh CONF PATH SSID PSK COUNTRY: write_wpa_conf with this environment.
WPA_CONF=$1
PATH=$2
SAVIOR_WIFI_SSID=$3
SAVIOR_WIFI_PSK=$4
SAVIOR_WIFI_COUNTRY=$5
SCRIPT=/etc/init.d/S30network
log() { printf '%s\n' "$*" >>"$WPA_CONF.log"; }
have() { command -v "$1" >/dev/null 2>&1; }
umask 022
. "${0%/*}/fn.sh"
write_wpa_conf
EOF
	# wconf NAME WPP(yes|no) SSID PSK [COUNTRY]: $x/NAME.conf, status in RC.
	wconf() {
		_p=$x/bin
		[ "$2" = no ] || _p=$_p:$x/wpp
		rm -f "$x/$1.conf" "$x/$1.conf.log"
		"$bb" sh "$x/run.sh" "$x/$1.conf" "$_p" "$3" "$4" "${5:-}" >>"$TLOG" 2>&1
		RC=$?
		sed "s/^/    $1.conf> /" "$x/$1.conf" "$x/$1.conf.log" >>"$TLOG" 2>/dev/null
	}
	has() { grep -q -x -F -- "$2" "$x/$1.conf" 2>/dev/null; }
	hex=$(python3 -c 'import os, sys; print(os.fsencode(sys.argv[1]).hex())' "$WIFI_SSID")
	want=$(python3 -c 'import hashlib, os, sys; print(hashlib.pbkdf2_hmac("sha1", os.fsencode(sys.argv[1]), os.fsencode(sys.argv[2]), 4096, 32).hex())' "$WIFI_PSK" "$WIFI_SSID")
	tab=$(printf '\t')

	wconf wpa yes "$WIFI_SSID" "$WIFI_PSK"
	check "WPA2: write_wpa_conf succeeds (status $RC)" [ "$RC" -eq 0 ]
	check "the SSID is hex-encoded UTF-8 (ssid=$hex)" has wpa "${tab}ssid=$hex"
	check "psk= is the PBKDF2 hash of the passphrase ($want)" has wpa "${tab}psk=$want"
	check "... and the passphrase itself is not in the file" eval '! grep -q -F -- "$WIFI_PSK" "$x/wpa.conf"'
	check "key_mgmt=WPA-PSK and country=US by default" eval 'has wpa "${tab}key_mgmt=WPA-PSK" && has wpa country=US'
	check "the file is mode 0600 under umask 022 ($(stat -c %a "$x/wpa.conf" 2>/dev/null))" \
		[ "$(stat -c %a "$x/wpa.conf" 2>/dev/null)" = 600 ]
	check "no temporary file is left" [ ! -e "$x/wpa.conf.tmp" ]

	wconf open yes "$WIFI_SSID" "" DE
	check "open network: key_mgmt=NONE, no psk= (status $RC)" \
		eval '[ "$RC" -eq 0 ] && has open "${tab}key_mgmt=NONE" && ! grep -q psk= "$x/open.conf"'
	check "wifi_country = DE gives country=DE" has open country=DE

	wconf quoted no "lab" 'pass word\1$x'
	check "without wpa_passphrase the passphrase is quoted as is (status $RC)" \
		eval '[ "$RC" -eq 0 ] && has quoted "${tab}psk=\"pass word\\1\$x\"" && has quoted "${tab}ssid=6c6162"'
	wconf refused no "$WIFI_SSID" "$WIFI_PSK"
	check "without wpa_passphrase a passphrase with a double quote is refused (status $RC, no file)" \
		eval '[ "$RC" -ne 0 ] && [ ! -e "$x/refused.conf" ] && grep -q "double quote" "$x/refused.conf.log"'
}

# wifi_initrd KIND: the dev initrd plus a cpio with this host's hostapd,
# wpa_supplicant, wpa_passphrase and iw (and the libraries the image lacks),
# the network config (/etc/savior/baked.conf) and an S35dhcpd that sets up
# the access point: mac80211_hwsim with two radios, the second (wlan1)
# moved into its own network namespace with iw (so the node sees only
# wlan0 and the AP's address is not the node's), hostapd there (its log on
# the serial console) and udhcpd for 10.66.0.100-200. KIND wpa: WPA2 with
# $WIFI_SSID / $WIFI_PSK; open: no encryption. The hook also reports the
# station's address as "SAVIOR-WIFIAP: station IFACE addr=A" (up to 240 s,
# boot-report waits only 40 s). Sets WIFI_INITRD.
wifi_initrd() {
	st=$W/wifi-$1
	rm -rf "$st"
	for d in etc etc/init.d etc/savior etc/qemu-wifi lib lib/x86_64-linux-gnu lib64 usr usr/bin usr/sbin; do
		mkdir -p "$st/$d"
		chmod 0755 "$st/$d"
	done
	[ -s "$W/initrd.list" ] || xz -dc "$OUT/initrd" | cpio -t --quiet 2>/dev/null >"$W/initrd.list"
	for p in hostapd wpa_supplicant wpa_passphrase iw; do
		src=$(host_prog "$p")
		if [ -z "$src" ]; then
			fail "this test needs $p on this host (apt install hostapd wpasupplicant iw)"
			return 1
		fi
		case "$src" in /usr/sbin/* | /usr/bin/* | /sbin/* | /bin/*) dst=$src ;; *) dst=/usr/sbin/$p ;; esac
		cp "$src" "$st$dst"
		chmod 0755 "$st$dst"
		for lib in $(ldd "$src" 2>/dev/null | awk '$2 == "=>" && $3 ~ /^\// { print $3 } $1 ~ /^\// { print $1 }'); do
			# Never replace a library of the image under test.
			grep -q -x -F "${lib#/}" "$W/initrd.list" && continue
			[ -e "$st$lib" ] && continue
			mkdir -p "$st${lib%/*}"
			cp -L "$lib" "$st$lib"
			chmod 0755 "$st$lib"
		done
	done
	{
		echo "interface=wlan1"
		echo "driver=nl80211"
		echo "ssid2=$(printf '%s' "$WIFI_SSID" | od -An -v -tx1 | tr -d ' \n')"
		echo "hw_mode=g"
		echo "channel=1"
		if [ "$1" = wpa ]; then
			echo "wpa=2"
			echo "wpa_key_mgmt=WPA-PSK"
			echo "rsn_pairwise=CCMP"
			echo "wpa_passphrase=$WIFI_PSK"
		fi
	} >"$st/etc/qemu-wifi/hostapd.conf"
	printf '%s\n' "interface wlan1" "start 10.66.0.100" "end 10.66.0.200" "max_leases 101" \
		"lease_file /run/qemu-wifi.leases" "option subnet 255.255.255.0" "option lease 3600" \
		>"$st/etc/qemu-wifi/udhcpd.conf"
	{
		echo "# qemu-test wifi ($1)"
		echo "name = wifi-$1"
		echo "wifi_ssid = $WIFI_SSID"
		[ "$1" != wpa ] || echo "wifi_psk = $WIFI_PSK"
	} >"$st/etc/savior/baked.conf"
	chmod 0600 "$st/etc/savior/baked.conf"
	cat >"$st/etc/init.d/S35dhcpd" <<'EOF'
#!/bin/sh
# qemu-test wifi: the access point on the second mac80211_hwsim radio, in
# its own network namespace (for this node it is another machine).
. /usr/libexec/savior/lib.sh
[ "${1:-}" = start ] || exit 0
ap() { printf 'SAVIOR-WIFIAP: %s\r\n' "$*" >/dev/ttyS0; }
if ! modprobe mac80211_hwsim radios=2; then
	ap "modprobe mac80211_hwsim failed"
	exit 0
fi
n=0
while [ ! -e /sys/class/net/wlan1/phy80211/name ] && [ "$n" -lt 30 ]; do
	sleep 1
	n=$((n + 1))
done
phy=$(cat /sys/class/net/wlan1/phy80211/name 2>/dev/null) || { ap "no wlan1"; exit 0; }
unshare -n sleep 2147483647 </dev/null >/dev/null 2>&1 &
ns=$!
n=0
while [ "$(readlink "/proc/$ns/ns/net")" = "$(readlink /proc/self/ns/net)" ] && [ "$n" -lt 100 ]; do
	sleep 0.1
	n=$((n + 1))
done
if ! iw phy "$phy" set netns "$ns"; then
	ap "iw phy $phy set netns $ns failed"
	exit 0
fi
in_ap() { nsenter -n -t "$ns" "$@"; }
in_ap ip link set lo up
in_ap ip addr add 10.66.0.1/24 dev wlan1
in_ap ip link set wlan1 up
# stdout is the serial console, a terminal: line buffered.
in_ap hostapd -t /etc/qemu-wifi/hostapd.conf </dev/null >/dev/ttyS0 2>&1 &
: >/run/qemu-wifi.leases
in_ap udhcpd -S /etc/qemu-wifi/udhcpd.conf
ap "access point: $phy (wlan1, 10.66.0.1/24) in the network namespace of pid $ns; left here: $(wireless_ifaces | tr '\n' ' ')"
(
	n=0
	while [ "$n" -lt 240 ]; do
		w=$(first_wireless)
		a=$(if_ipv4 "${w:-none}")
		if [ -n "$a" ]; then
			ap "station $w addr=${a%% *}"
			exit 0
		fi
		sleep 1
		n=$((n + 1))
	done
	ap "station ${w:-none}: no address within 240 s"
) </dev/null >/dev/null 2>&1 &
exit 0
EOF
	chmod 0755 "$st/etc/init.d/S35dhcpd"
	(cd "$st" && find . -mindepth 1 | sed 's|^\./||' | LC_ALL=C sort | cpio -o -H newc -R 0:0 --quiet) >"$W/wifi-$1.cpio" ||
		{ fail "cpio failed"; return 1; }
	# The kernel reads concatenated archives at 4-byte boundaries.
	WIFI_INITRD=$W/wifi-$1.initrd
	cp "$OUT/initrd" "$WIFI_INITRD"
	_pad=$(((4 - $(wc -c <"$WIFI_INITRD") % 4) % 4))
	[ "$_pad" -eq 0 ] || head -c "$_pad" /dev/zero >>"$WIFI_INITRD"
	cat "$W/wifi-$1.cpio" >>"$WIFI_INITRD"
}

# wifi_boot KIND: boot the dev payload without any wired NIC, with
# wifi_initrd KIND, and check that the node joined the access point.
wifi_boot() {
	wifi_initrd "$1" || return 1
	serial=$LOGS/$CUR-$1.serial
	vm "wifi-$1" "$serial" -m "$MEM" -smp 1 -vga std -nic none \
		-kernel "$OUT/vmlinuz" -initrd "$WIFI_INITRD" \
		-append "consoleblank=0 quiet loglevel=3 console=ttyS0,115200 console=tty0 savior.media=none savior_dumplog=1"
	_cur=$CUR
	CUR=$_cur/$1
	if wait_boot "$serial" "$VM_PID"; then
		check_report cfg=no media=none node=yes
		wait_for "$serial" 'SAVIOR-WIFIAP: access point' 60 "$VM_PID"
		info "$(grep -a 'SAVIOR-WIFIAP: access point' "$serial" | tail -n 1 | tr -d '\r')"
		check "hostapd is up on the second radio (AP-ENABLED)" grep -a -q 'wlan1: AP-ENABLED' "$serial"
		check "S30network: \"wifi: joining '$WIFI_SSID' on wlan0\"" \
			wait_for "$serial" "SAVIOR-LOG: .*wifi: joining '$WIFI_SSID' on wlan0" 60 "$VM_PID"
		check "the node associated (hostapd: AP-STA-CONNECTED)" wait_for "$serial" 'wlan1: AP-STA-CONNECTED ' 180 "$VM_PID"
		if [ "$1" = wpa ]; then
			check "... after a WPA2 4-way handshake with the hashed PSK (EAPOL-4WAY-HS-COMPLETED)" \
				grep -a -q 'wlan1: EAPOL-4WAY-HS-COMPLETED ' "$serial"
		fi
		if wait_for "$serial" 'SAVIOR-WIFIAP: station ' 240 "$VM_PID"; then
			sta=$(grep -a 'SAVIOR-WIFIAP: station ' "$serial" | tail -n 1 | tr -d '\r' | sed 's/.*SAVIOR-WIFIAP: //')
			info "$sta"
			check "the node got a DHCP lease over Wi-Fi from the access point's udhcpd" \
				eval 'printf "%s\n" "$sta" | grep -q -E "station wlan0 addr=10\.66\.0\.(1[0-9][0-9]|200)\$"'
		else
			fail "no station address line within 240 s"
		fi
		# net= is the first address boot-report saw within its 40 s: the
		# lease, or none when the join took longer (never the AP's .1).
		case "$(field net)" in
		none) info "the lease came after boot-report's 40 s wait (net=none)" ;;
		*) check_report 'net~^10\.66\.0\.(1[0-9][0-9]|200)$' ;;
		esac
		check_agent "$serial" "$VM_PID"
		grep -a -E 'AP-STA-POSSIBLE-PSK-MISMATCH|CTRL-EVENT-SSID-TEMP-DISABLED|wifi: ' "$serial" | tr -d '\r' |
			head -n 5 | sed 's/^/        | /' >>"$TLOG"
	fi
	stop_vms
	CUR=$_cur
}

# wifi: WPA2, then an open network (key_mgmt=NONE).
t_wifi() {
	need_file "$OUT/vmlinuz" || return
	need_file "$OUT/initrd" || return
	wifi_boot wpa
	wifi_boot open
}

t_boot_iso_bios() {
	need_file "$MEDIA/savior.iso" || return
	serial=$LOGS/$CUR.serial
	vm "$CUR" "$serial" -m "$MEM" -smp 1 -vga std -netdev user,id=n0 -device e1000,netdev=n0 \
		-drive "if=none,id=cd,media=cdrom,readonly=on,format=raw,file=$MEDIA/savior.iso" \
		-device ide-cd,drive=cd,bootindex=0
	wait_boot "$serial" "$VM_PID" || return
	# The CD holds the commented savior.conf template: config found, no key.
	check_report cfg=yes key=no 'media~^/dev/sr[0-9]+$' net=10.0.2.15 console=yes fb=yes node=yes
	check_respawn "$serial" "$VM_PID"
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
	check_report cfg=yes key=yes 'media~^/dev/sd[a-z]+$' net=10.0.2.15 console=yes fb=yes node=yes
	check_respawn "$serial" "$VM_PID"
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
	check_report cfg=no key=yes media=none net=10.0.2.15 console=yes fb=yes node=yes
	check_respawn "$serial" "$VM_PID"
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
	check_agent "$serial" "$VM_PID"
	sleep 5
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

# online_nodes [SINCE]: number of online nodes (needs jq); with SINCE (an
# RFC 3339 time), only those last seen after it.
online_nodes() {
	ctl nodes --all --json || ctl nodes --json || { echo 0; return; }
	jq --arg t "${1:-}" 'def secs: sub("\\.[0-9]+"; "") | fromdate;
		[.[] | select(.liveness == "online" and ($t == "" or (.last_seen | secs) > ($t | secs)))] | length' \
		"$W/ctl.out" 2>/dev/null || echo 0
}

# wait_online N [SINCE]: N nodes online within 420 s (last seen after
# SINCE, see online_nodes); the node list stays in $W/ctl.out.
wait_online() {
	t0=$(date +%s)
	n=0
	while [ $(($(date +%s) - t0)) -lt 420 ]; do
		n=$(online_nodes "${2:-}")
		[ "$n" -ge "$1" ] && break
		sleep 10
	done
	if [ "$n" -ge "$1" ]; then
		pass "$1 nodes online${2:+ and seen since $2} after $(($(date +%s) - t0)) s"
		return 0
	fi
	fail "only $n nodes online after $(($(date +%s) - t0)) s ($(tail -n 1 "$W/ctl.err" 2>/dev/null))"
	return 1
}

# wait_hive_api: the hive API answers on 127.0.0.1:$HIVE_PORT within 240 s
# (with curl; without it, ctl's own retries have to do).
wait_hive_api() {
	command -v curl >/dev/null 2>&1 || return 0
	t0=$(date +%s)
	until curl -s -k -m 5 -o /dev/null "https://127.0.0.1:$HIVE_PORT/api/v1/hello"; do
		if [ $(($(date +%s) - t0)) -ge 240 ]; then
			fail "the hive API (https://127.0.0.1:$HIVE_PORT -> hive:7700) did not answer within 240 s"
			return 1
		fi
		sleep 5
	done
	pass "hive API answers on the forwarded port after $(($(date +%s) - t0)) s"
}

# wait_host_key PORT: the SSH host key on PORT, retried for 120 s (on a
# hive, S50sshd starts dropbear once SAVIOR-DATA is mounted).
wait_host_key() {
	_t0=$(date +%s)
	_k=$(host_key "$1")
	while [ -z "$_k" ] && [ $(($(date +%s) - _t0)) -lt 120 ]; do
		sleep 5
		_k=$(host_key "$1")
	done
	echo "$_k"
}

# out_sums DIR: the sorted SHA-256 sums of the files below DIR.
out_sums() { find "$1" -type f -exec sha256sum {} + 2>/dev/null | awk '{ print $1 }' | sort; }

# swarm_hive_reboot: DESIGN 13.4 / USER_GUIDE 2: a hive keeps its state on
# SAVIOR-DATA across reboots. Before: ctl info says persistent, c2 is
# renamed, the SSH host key is noted. The hive's own node agent reboots it
# (ctl reboot: BusyBox init runs rcK, which stops the hive and unmounts
# SAVIOR-DATA; QEMU runs with -no-reboot, so the VM exits), and the same
# stick boots again with the same MAC and UUID. After: the same
# certificate (ctl.json pins it, so ctl fails on a new one), still
# persistent, the job and its outputs (blobs) are there, the 4 nodes come
# back and c2 keeps its new name, and the SSH host key is the same (S50sshd
# keeps it in /var/lib/savior/data/ssh). A hive that formats SAVIOR-DATA
# again or falls back to RAM fails every one of these.
swarm_hive_reboot() {
	if ! ctl info --json; then
		fail "ctl info failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
		return 1
	fi
	cp "$W/ctl.out" "$LOGS/swarm-hive-info.json"
	fp1=$(jq -r '.fingerprint // empty' "$W/ctl.out")
	p=$(jq -r '.persistent' "$W/ctl.out")
	dd=$(jq -r '.data_dir // empty' "$W/ctl.out")
	check "the hive keeps its state on disk (ctl info: persistent = $p, data_dir = $dd)" [ "$p" = true ]
	check "... on SAVIOR-DATA (data_dir below /var/lib/savior/data)" \
		eval 'case "$dd" in /var/lib/savior/data/*) true ;; *) false ;; esac'
	job=""
	ctl jobs --json && job=$(jq -r '[.[] | select(.name == "qemu-swarm-test")][0].id // empty' "$W/ctl.out")
	check "rename c2 to c2-renamed" ctl rename c2 c2-renamed
	hk1=$(wait_host_key "$HIVE_SSH_PORT")
	check "the hive's SSH server answers (ed25519 host key ${hk1:-missing})" [ -n "$hk1" ]
	[ -z "$hk1" ] || check_ssh "$HIVE_SSH_PORT"
	hive_id=$(jq -r '[.[] | select(.name == "hive")][0].id // empty' "$LOGS/swarm-nodes.json")
	check "ctl reboot the hive's machine" ctl reboot "${hive_id:-hive}"
	t0=$(date +%s)
	while kill -0 "$HIVE_PID" 2>/dev/null && [ $(($(date +%s) - t0)) -lt 180 ]; do
		sleep 2
	done
	if kill -0 "$HIVE_PID" 2>/dev/null; then
		fail "the hive did not reboot within 180 s of ctl reboot; switching it off"
		stop_pid "$HIVE_PID"
	else
		pass "the hive's machine rebooted $(($(date +%s) - t0)) s after ctl reboot"
	fi

	swarm_vm hive-2 52:54:00:77:00:01 "${U}01" "$W/hive.img" 1024 \
		-netdev "$HIVE_UP" -device e1000,netdev=up,mac=52:54:00:77:01:01
	HIVE_PID=$VM_PID
	hs=$LOGS/swarm-hive-2.serial
	CUR=swarm/hive-2
	wait_boot "$hs" "$HIVE_PID" || { CUR=swarm; return 1; }
	check_report 'net~^10\.77\.0\.1$' cfg=yes key=yes
	check_agent "$hs" "$HIVE_PID"
	CUR=swarm
	wait_hive_api || return 1
	started=""
	if ctl info --json; then
		cp "$W/ctl.out" "$LOGS/swarm-hive-2-info.json"
		started=$(jq -r '.started_at // empty' "$W/ctl.out")
		fp2=$(jq -r '.fingerprint // empty' "$W/ctl.out")
		check "same hive certificate after the reboot ($fp2)" eval '[ -n "$fp2" ] && [ "$fp2" = "$fp1" ]'
		check "still persistent after the reboot" [ "$(jq -r '.persistent' "$W/ctl.out")" = true ]
	else
		fail "ctl (pinned to $fp1) cannot talk to the rebooted hive: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	st=""
	ctl jobs --json && st=$(jq -r --arg id "$job" '.[] | select(.id == $id) | .state' "$W/ctl.out")
	check "the job survived the reboot (${job:-no job}: ${st:-missing})" eval '[ -n "$job" ] && [ "$st" = succeeded ]'
	rm -rf "$W/outputs2"
	if [ -n "$job" ] && ctl outputs "$job" -o "$W/outputs2"; then
		check "its outputs survived the reboot with the same contents ($(find "$W/outputs2" -type f | wc -l) files)" \
			[ "$(out_sums "$W/outputs")" = "$(out_sums "$W/outputs2")" ]
	else
		fail "ctl outputs ${job:-(no job)} failed after the reboot: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	# Every node heartbeats to the new hive process (not just a last_seen
	# from before the reboot).
	if wait_online 4 "$started"; then
		cp "$W/ctl.out" "$LOGS/swarm-nodes-2.json"
		check "c2's new name survived the reboot ($(jq -r '[.[].name] | join(" ")' "$W/ctl.out"))" \
			jq -e '[.[] | select(.name == "c2-renamed")] | length == 1' "$W/ctl.out"
	fi
	hk2=$(wait_host_key "$HIVE_SSH_PORT")
	check "the hive's SSH host key survived the reboot (${hk2:-no host key})" eval '[ -n "$hk1" ] && [ "$hk2" = "$hk1" ]'
	info "hive syslog (second boot): $(grep -a 'SAVIOR-SYSLOG: .*\(hive data\|SAVIOR-DATA\|init-data\|sshd\)' "$hs" | tail -n 3 | tr -d '\r' | tr '\n' ' ')"
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
	ssh_keys || return
	MCAST="230.$(rand_byte).$(rand_byte).$(rand_byte):$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))"
	HIVE_PORT=$((40000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
	HIVE_SSH_PORT=$(rand_port)
	ADMIN_TOKEN=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')
	info "LAN mcast=$MCAST, hive API on 127.0.0.1:$HIVE_PORT, hive SSH on 127.0.0.1:$HIVE_SSH_PORT"

	# Sticks. The hive's copy gets 2 GiB of (sparse) free space for
	# SAVIOR-DATA: blob uploads need max(5%, 512 MiB) free (DESIGN 7.1).
	for n in hive c1 c2 disp dup; do
		stick_copy "$W/$n.img"
	done
	truncate -s +2G "$W/hive.img"
	common="swarm_key = $KEY"
	# ssh_key on the hive: S50sshd keeps its host key on SAVIOR-DATA.
	stick_conf "$W/hive.img@@$MT_OFF" "$common" "name = hive" "roles = hive,compute" "net = static" \
		"ip = 10.77.0.1/24" "dhcp_server = yes" "admin_token = $ADMIN_TOKEN" "ssh_key = $(cat "$W/id.pub")" ||
		{ fail "stick setup"; return; }
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
	HIVE_UP="user,id=up,hostfwd=tcp:127.0.0.1:$HIVE_PORT-:7700,hostfwd=tcp:127.0.0.1:$HIVE_SSH_PORT-:22"
	swarm_vm hive 52:54:00:77:00:01 "${U}01" "$W/hive.img" 1024 \
		-netdev "$HIVE_UP" -device e1000,netdev=up,mac=52:54:00:77:01:01
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
	check_agent "$LOGS/swarm-hive.serial" "$HIVE_PID"
	for n in c1 c2 disp; do
		CUR=swarm/$n
		pid=$(awk -v n="$n" '$1 == n { print $2 }' "$W/swarm.pids")
		# DHCP from the hive's udhcpd (dhcp_server = yes) on the LAN.
		if wait_boot "$LOGS/swarm-$n.serial" "$pid"; then
			check_report 'net~^10\.77\.0\.[0-9]+$' key=yes
			check_agent "$LOGS/swarm-$n.serial" "$pid"
		fi
	done
	CUR=swarm
	sleep 10
	info "hive syslog: $(grep -a 'SAVIOR-SYSLOG: .*\(hive data\|SAVIOR-DATA\|init-data\|sshd\)' "$LOGS/swarm-hive.serial" | tail -n 3 | tr -d '\r' | tr '\n' ' ')"

	# The hive API through the forwarded port (hostfwd to its second NIC).
	wait_hive_api || return
	# All four nodes online (the hive also runs a compute node).
	wait_online 4 || return
	cp "$W/ctl.out" "$LOGS/swarm-nodes.json"

	# A job: 4 tasks that write an output file each: their index, the
	# number of CA certificates the sandbox sees (the runner binds
	# /etc/ssl/certs), and what internal/runner's TestRealSandboxIsolation
	# probes from inside a task, here with SaviorOS's kernel, init and
	# BusyBox.
	cat >"$W/job.sh" <<'EOF'
echo "task $SAVIOR_TASK_INDEX of $SAVIOR_TASK_COUNT" > out.txt
uname -m >> out.txt
echo "ca $(grep -c -e '-----BEGIN CERTIFICATE-----' /etc/ssl/certs/ca-certificates.crt 2>&1 | head -n 1)" >> out.txt
{
	echo "uid=$(id -u)"
	echo "cmdline=[$(cat /proc/cmdline 2>/dev/null)]"
	for d in media run sys root; do [ -e "/$d" ] && echo "$d=present" || echo "$d=absent"; done
	(echo hi > /oops.txt) 2>/dev/null && echo rootwrite=ok || echo rootwrite=fail
	(echo hi > /etc/x) 2>/dev/null && echo etcwrite=ok || echo etcwrite=fail
	grep -qE 'eth|ens|enp|wl' /proc/net/dev && echo extranet=present || echo extranet=absent
	mkdir -p m
	unshare -Un true 2>/dev/null && echo unshare=ok || echo unshare=fail
	mount -t tmpfs none m 2>/dev/null && echo mount=ok || echo mount=fail
	echo "seccomp=$(grep '^Seccomp:' /proc/self/status | tr -d '\t ' | cut -d: -f2)"
} >> out.txt
EOF
	jq -n --rawfile script "$W/job.sh" '{name: "qemu-swarm-test", script: $script, outputs: ["out.txt"], count: 4,
		resources: {cores: 0.5, mem_mb: 64, disk_mb: 16}, requirements: {}, timeout_s: 300}' >"$W/job.json" ||
		{ fail "jq could not write the job spec"; return; }
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
	# Mozilla's CA set has well over 100 certificates.
	ca=$(grep -r -h -E '^ca [0-9]+$' "$W/outputs" 2>/dev/null | awk '$2 >= 100 { n++ } END { print n + 0 }')
	check "all 4 tasks see the CA bundle /etc/ssl/certs/ca-certificates.crt ($ca/4)" [ "$ca" -eq 4 ]
	[ "$ca" -eq 4 ] || info "the tasks said: $(grep -r -h '^ca ' "$W/outputs" 2>/dev/null | sort | uniq -c | tr -s ' \n' ' ')"
	# The sandbox held in every task: a slot uid (the VMs have one CPU, so
	# two slots: 10000-10001), no kernel command line (the swarm key can be
	# on it), no /run (/run/savior/env holds the key), /media, /sys or
	# /root, a read-only root and /etc, no network (the job asks for none),
	# no new namespaces or mounts, and a seccomp filter (mode 2).
	n=$(grep -r -h -x -E 'uid=1000[01]' "$W/outputs" 2>/dev/null | wc -l)
	check "sandbox: all 4 tasks run as a slot uid 10000-10001 ($n/4: $(grep -r -h '^uid=' "$W/outputs" 2>/dev/null | sort | uniq -c | tr -s ' \n' ' '))" \
		[ "$n" -eq 4 ]
	for kv in 'cmdline=[]' media=absent run=absent sys=absent root=absent rootwrite=fail etcwrite=fail \
		extranet=absent unshare=fail mount=fail seccomp=2; do
		n=$(grep -r -h -x -F -- "$kv" "$W/outputs" 2>/dev/null | wc -l)
		check "sandbox: all 4 tasks say $kv ($n/4: $(grep -r -h -- "^${kv%%=*}=" "$W/outputs" 2>/dev/null | sort | uniq -c | tr -s ' \n' ' '))" \
			[ "$n" -eq 4 ]
	done

	# The display node: first a known black screen (mode color), then the
	# text, which must match savior display render of the same spec pixel
	# for pixel. (The status scene is not black and redraws by itself, so
	# "the screen changed" alone would pass without the text.)
	disp_id=$(jq -r '[.[] | select(.name == "disp")][0].id // empty' "$LOGS/swarm-nodes.json")
	[ -n "$disp_id" ] || disp_id=disp
	if ctl display "$disp_id" color --bg 000000; then
		pass "display set to color #000000"
	else
		fail "ctl display color failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	nz=-
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt 90 ]; do
		sleep 3
		screendump disp "$LOGS/swarm-disp-before.ppm" || continue
		# shellcheck disable=SC2046
		set -- $(ppm_stats "$LOGS/swarm-disp-before.ppm")
		nz=$3
		[ "$nz" -eq 0 ] && break
	done
	check "the display node's screen turned all black ($nz non-zero bytes)" [ "$nz" = 0 ]
	printf '{"mode": "text", "text": "HELLO SWARM"}\n' >"$W/text-spec.json"
	if ctl display "$disp_id" text --text "HELLO SWARM"; then
		pass "display set to text 'HELLO SWARM'"
	else
		fail "ctl display failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	ref=""
	if command -v python3 >/dev/null 2>&1; then
		ref=$W/text-ref.png
	else
		info "no python3: the screen is not compared with savior display render"
	fi
	pxdiff=-
	rm -f "$LOGS/swarm-disp.ppm"
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt 90 ]; do
		sleep 3
		screendump disp "$LOGS/swarm-disp.ppm" || continue
		# shellcheck disable=SC2046
		set -- $(ppm_stats "$LOGS/swarm-disp.ppm")
		[ "$3" -gt 0 ] || continue
		[ -n "$ref" ] || break
		if [ ! -s "$ref" ]; then
			"$CTL" display render --spec "$W/text-spec.json" --size "$1x$2" --out "$ref" >>"$TLOG" 2>&1 ||
				{ fail "savior display render failed"; ref=""; break; }
		fi
		pxdiff=$(img_diff "$ref" "$LOGS/swarm-disp.ppm")
		[ "$pxdiff" = 0 ] && break
	done
	if [ -s "$LOGS/swarm-disp.ppm" ]; then
		# shellcheck disable=SC2046
		set -- $(ppm_stats "$LOGS/swarm-disp.ppm")
		check "the text screen is not black ($3 of $4 bytes non-zero, want >= 1%)" [ $(($3 * 100)) -ge "$4" ]
		check "display screen changed after the text was set" \
			differs "$LOGS/swarm-disp-before.ppm" "$LOGS/swarm-disp.ppm"
		if [ -n "$ref" ]; then
			cp "$ref" "$LOGS/swarm-disp-ref.png" 2>/dev/null
			check "the screen shows exactly what savior display render draws for the spec ($pxdiff pixels differ)" [ "$pxdiff" = 0 ]
		fi
	else
		fail "screendump of the display node failed"
	fi

	check "identify all nodes" ctl identify --all --seconds 10

	# The hive reboots and keeps its state (SAVIOR-DATA).
	swarm_hive_reboot

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

# lan_setup: a fresh multicast LAN, hive API port and admin token.
lan_setup() {
	MCAST="230.$(rand_byte).$(rand_byte).$(rand_byte):$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))"
	HIVE_PORT=$((40000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
	ADMIN_TOKEN=$(od -An -N20 -tx1 /dev/urandom | tr -d ' \n')
	info "LAN mcast=$MCAST, hive API on 127.0.0.1:$HIVE_PORT"
}

# stop_pid PID: stop one VM of the running test.
stop_pid() {
	kill "$1" 2>/dev/null
	_n=0
	while kill -0 "$1" 2>/dev/null && [ "$_n" -lt 20 ]; do
		sleep 0.5
		_n=$((_n + 1))
	done
	kill -9 "$1" 2>/dev/null
}

# hive-pxe: a hive with netboot = yes and dhcp_server = yes (S65netboot:
# dnsmasq DHCP + TFTP, generated grub.cfg) boots a diskless machine on the
# private LAN. The netbooted node joins keyless (no swarm key over the
# network), shows up as pending and is approved with savior ctl.
t_hive_pxe() {
	need_file "$MEDIA/savior.img" || return
	CTL=$OUT/savior
	need_file "$CTL" || return
	command -v jq >/dev/null 2>&1 || { fail "this test needs jq (apt install jq)"; return; }
	lan_setup
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
	if wait_for "$LOGS/hive-pxe-hive.serial" 'SAVIOR-NETBOOT: ' 240 "$HIVE_PID"; then
		info "$(grep -a 'SAVIOR-NETBOOT: ' "$LOGS/hive-pxe-hive.serial" | tail -n 1 | tr -d '\r')"
		check "the hive serves PXE as the LAN's DHCP server (net = static, dhcp_server = yes)" \
			grep -a -q -E 'SAVIOR-NETBOOT: mode=full .* bios=grub uefi=yes' "$LOGS/hive-pxe-hive.serial"
	else
		fail "no SAVIOR-NETBOOT line from the hive within 240 s (checking the clients anyway)"
	fi
	# Diskless clients, BIOS then UEFI: PXE from the hive's dnsmasq.
	PXE_T=hive-pxe
	PXE_MAC=52:54:00:78:00
	PXE_NET='^10\.77\.0\.[0-9]+$'
	pxe_client bios 1 bios || return
	need_ovmf || return
	pxe_client uefi 2 uefi
}

# pxe_client FIRMWARE N LABEL: diskless VM number N (MAC $PXE_MAC:1N, UUID
# ${U}1N) on the LAN of test $PXE_T, serial console in
# $LOGS/$PXE_T-client-LABEL.serial. Its address must match $PXE_NET, and
# its node must join keyless, be pending, and come online once approved.
# VM_PID stays the client's.
pxe_client() {
	fw=$1
	n=$2
	label=$3
	serial=$LOGS/$PXE_T-client-$label.serial
	set -- -m "$MEM" -smp 1 -vga std -uuid "${U}1$n" \
		-netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
		-device "e1000,netdev=lan,mac=$PXE_MAC:1$n,bootindex=0"
	[ "$fw" = bios ] || set -- -bios "$OVMF" "$@"
	vm "client-$label" "$serial" "$@"
	CUR=$PXE_T/$label
	if ! wait_boot "$serial" "$VM_PID" $((BOOT_TIMEOUT * 3)); then
		CUR=$PXE_T
		return 1
	fi
	check_report media=none key=no "net~$PXE_NET"
	check_agent "$serial" "$VM_PID"
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
		CUR=$PXE_T
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
	CUR=$PXE_T
}

# router_vm KIND: the LAN's DHCP server at 10.79.0.254 for hive-pxe-proxy,
# booted straight from the dev payload (no stick) with net = static and
# dhcp_server = yes. KIND udhcpd: SaviorOS's own S35dhcpd (udhcpd, next-server
# 0.0.0.0) hands out .20-.59. KIND dnsmasq: S35dhcpd is replaced (an extra
# cpio after the initrd) by a dnsmasq DHCP server for .60-.99 whose replies
# name the router as next-server, as OpenWrt's and Pi-hole's do. Sets
# ROUTER_PID.
router_vm() {
	_initrd=$OUT/initrd
	_range=10.79.0.20-10.79.0.59
	if [ "$1" = dnsmasq ]; then
		_range=10.79.0.60-10.79.0.99
		rm -rf "$W/router-ov"
		mkdir -p "$W/router-ov/etc/init.d"
		cat >"$W/router-ov/etc/init.d/S35dhcpd" <<'EOF'
#!/bin/sh
# qemu-test: the LAN router's DHCP server, dnsmasq (siaddr = itself).
. /usr/libexec/savior/lib.sh
[ "${1:-}" = start ] || exit 0
load_env
iface=$(first_wired)
dnsmasq --port=0 --interface="$iface" --bind-interfaces --dhcp-authoritative \
	--dhcp-range="${SAVIOR_DHCP_RANGE%-*},${SAVIOR_DHCP_RANGE#*-},255.255.255.0,1h" \
	--dhcp-leasefile="$RUN_DIR/router.leases" --pid-file="$RUN_DIR/router.pid" &&
	log "router: dnsmasq serves DHCP $SAVIOR_DHCP_RANGE on $iface"
EOF
		chmod 0755 "$W/router-ov/etc" "$W/router-ov/etc/init.d" "$W/router-ov/etc/init.d/S35dhcpd"
		(cd "$W/router-ov" && printf '%s\n' etc etc/init.d etc/init.d/S35dhcpd |
			cpio -o -H newc -R 0:0 --quiet) >"$W/router-ov.cpio" || { fail "cpio failed"; return 1; }
		# The kernel reads concatenated archives at 4-byte boundaries.
		_initrd=$W/router-initrd
		cp "$OUT/initrd" "$_initrd"
		_pad=$(((4 - $(wc -c <"$_initrd") % 4) % 4))
		[ "$_pad" -eq 0 ] || head -c "$_pad" /dev/zero >>"$_initrd"
		cat "$W/router-ov.cpio" >>"$_initrd"
	fi
	_serial=$LOGS/$PXE_T-router-$1.serial
	vm "router-$1" "$_serial" -m "$MEM" -smp 1 -vga std \
		-netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" -device e1000,netdev=lan,mac=52:54:00:79:00:fe \
		-kernel "$OUT/vmlinuz" -initrd "$_initrd" \
		-append "consoleblank=0 quiet loglevel=3 console=ttyS0,115200 console=tty0 savior.media=none savior.net=static savior.ip=10.79.0.254/24 savior.dhcp_server=yes savior.dhcp_range=$_range savior.roles=compute savior.hive=127.0.0.1:9 savior_dumplog=1"
	ROUTER_PID=$VM_PID
	CUR=$PXE_T/router-$1
	wait_boot "$_serial" "$ROUTER_PID"
	_rc=$?
	CUR=$PXE_T
	return "$_rc"
}

# pxe_probe MAC IP USERCLASS: a PXE client on the LAN that answers ARP for
# IP and never takes a lease: DHCPDISCOVER as a BIOS PXE ROM
# (PXEClient:Arch:00000, with user class USERCLASS unless it is "none"),
# then a boot server request (port 4011) for the first PXE menu item of the
# proxy offer. Prints one "offer ..." line per plain DHCP offer and a last
# line "proxy=SERVER menu=TYPE:TEXT file=BOOTFILE siaddr=NEXTSERVER".
pxe_probe() {
	python3 - "$MCAST" "$@" <<'PY'
import os, socket, struct, sys, time
grp, port = sys.argv[1].rsplit(":", 1)
mac = bytes.fromhex(sys.argv[2].replace(":", ""))
myip = socket.inet_aton(sys.argv[3])
ucls = b"" if sys.argv[4] == "none" else sys.argv[4].encode()
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_UDP)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((grp, int(port)))
s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, socket.inet_aton(grp) + socket.inet_aton("127.0.0.1"))
s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, socket.inet_aton("127.0.0.1"))
s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_LOOP, 1)
s.settimeout(0.2)
dst = (grp, int(port))
xid = os.urandom(4)
zero = b"\0" * 4

def csum(b):
    t = sum(struct.unpack("!10H", b))
    while t >> 16:
        t = (t & 0xFFFF) + (t >> 16)
    return ~t & 0xFFFF

def frame(dmac, src, dstip, sport, dport, payload):
    udp = struct.pack("!HHHH", sport, dport, 8 + len(payload), 0) + payload
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0, src, dstip)
    ip = ip[:10] + struct.pack("!H", csum(ip)) + ip[12:]
    return dmac + mac + b"\x08\x00" + ip + udp

def opt(code, data):
    return bytes([code, len(data)]) + data

def bootp(msgtype, ciaddr, extra=b""):
    b = struct.pack("!BBBB4sHH4s4s4s4s16s64s128s", 1, 1, 6, 0, xid, 0,
                    0x8000 if ciaddr == zero else 0, ciaddr, zero, zero, zero, mac, b"", b"")
    o = b"\x63\x82\x53\x63" + opt(53, bytes([msgtype]))
    o += opt(60, b"PXEClient:Arch:00000:UNDI:002001") + opt(93, b"\0\0") + opt(94, b"\1\2\1")
    o += opt(97, b"\0" + mac + mac + b"\0\0\0\0") + opt(55, bytes([1, 3, 43, 54, 60, 66, 67]))
    if ucls:
        o += opt(77, ucls)
    return b + o + extra + b"\xff"

def options(b):
    res, i = {}, 240
    while i < len(b) and b[i] != 255:
        if b[i] == 0:
            i += 1
            continue
        res[b[i]] = b[i + 2:i + 2 + b[i + 1]]
        i += 2 + b[i + 1]
    return res

def subopts(b):
    res, i = {}, 0
    while i + 1 < len(b) and b[i] != 255:
        res[b[i]] = b[i + 2:i + 2 + b[i + 1]]
        i += 2 + b[i + 1]
    return res

def replies(until, want):
    # Answer ARP for our address; return the first BOOTP reply for our xid
    # that want(ip_src, eth_src, bootp, options) accepts.
    while time.time() < until:
        try:
            f = s.recv(2048)
        except socket.timeout:
            continue
        if len(f) < 42 or f[6:12] == mac:
            continue
        et = f[12:14]
        if et == b"\x08\x06" and f[20:22] == b"\0\1" and f[38:42] == myip:
            arp = struct.pack("!HHBBH6s4s6s4s", 1, 0x0800, 6, 4, 2, mac, myip, f[22:28], f[28:32])
            s.sendto(f[6:12] + mac + b"\x08\x06" + arp, dst)
            continue
        if et != b"\x08\x00" or f[23] != 17:
            continue
        ihl = (f[14] & 15) * 4
        u = 14 + ihl
        if struct.unpack("!H", f[u + 2:u + 4])[0] != 68:
            continue
        b = f[u + 8:]
        if len(b) < 240 or b[0] != 2 or b[4:8] != xid:
            continue
        o = options(b)
        if want(f[26:30], f[6:12], b, o):
            return f[26:30], f[6:12], b, o
    return None

ip = socket.inet_ntoa
proxy = None
seen = set()

def offer(src, emac, b, o):
    global proxy
    if o.get(53) != b"\x02":
        return False
    if o.get(60, b"").startswith(b"PXEClient"):
        proxy = proxy or (src, emac, b, o)
    elif o.get(54) not in seen:
        seen.add(o.get(54))
        print("offer server=%s yiaddr=%s siaddr=%s file=%r" % (ip(o.get(54, zero)), ip(b[16:20]), ip(b[20:24]), b[108:236].rstrip(b"\0").decode()))
    return False

end = time.time() + 30
while proxy is None and time.time() < end:
    s.sendto(frame(b"\xff" * 6, zero, b"\xff" * 4, 68, 67, bootp(1, zero)), dst)
    # DHCP servers may check the address (ping, ARP) before they offer it:
    # listen long enough for their offers too.
    replies(time.time() + 5, offer)
if proxy is None:
    print("error: no proxy DHCP offer (PXEClient) within 30 s")
    sys.exit(1)
psrc, pmac, pb, po = proxy
server = po.get(54, psrc)
vend = subopts(po.get(43, b""))
menu = vend.get(9, b"")
if len(menu) < 3:
    print("proxy=%s menu=none file=%r siaddr=%s" % (ip(server), pb[108:236].rstrip(b"\0").decode(), ip(pb[20:24])))
    sys.exit(0)
item = menu[0:2]
text = menu[3:3 + menu[2]].decode(errors="replace")
req = bootp(3, myip, opt(43, bytes([71, 4]) + item + b"\0\0\xff"))
ack = None
end = time.time() + 20
while ack is None and time.time() < end:
    s.sendto(frame(pmac, myip, server, 68, 4011, req), dst)
    ack = replies(time.time() + 3, lambda src, emac, b, o: o.get(53) == b"\x05")
if ack is None:
    print("proxy=%s menu=%d:%s error: no boot server reply from %s:4011" % (ip(server), struct.unpack("!H", item)[0], text, ip(server)))
    sys.exit(1)
b = ack[2]
print("proxy=%s menu=%d:%s file=%s siaddr=%s" % (ip(server), struct.unpack("!H", item)[0], text, b[108:236].rstrip(b"\0").decode(), ip(b[20:24])))
PY
}

# probe_file OUTPUT: the file= value of pxe_probe's last line.
probe_file() { printf '%s\n' "$1" | tail -n 1 | sed -n 's/.* file=\([^ ]*\).*/\1/p'; }
# out_has REGEX: a line of $out (pxe_probe output) matches REGEX.
out_has() { printf '%s\n' "$out" | grep -q -E -- "$1"; }

# hive-pxe-proxy: netboot = yes next to the LAN's own DHCP server (the
# default: S65netboot answers PXE as a proxy DHCP server, dhcp_server = no).
# BOOT-01: GRUB's BIOS core.0 cannot boot from a plain PXE ROM there, so
# such ROMs get iPXE (undionly.kpxe) and iPXE gets savior.ipxe, which points
# next-server at the hive and chains core.0. QEMU's NIC ROMs are iPXE, so
# the VMs take the savior.ipxe path; pxe_probe checks what a plain PXE ROM
# is offered.
t_hive_pxe_proxy() {
	need_file "$MEDIA/savior.img" || return
	need_file "$OUT/vmlinuz" || return
	need_file "$OUT/initrd" || return
	CTL=$OUT/savior
	need_file "$CTL" || return
	command -v jq >/dev/null 2>&1 || { fail "this test needs jq (apt install jq)"; return; }
	command -v python3 >/dev/null 2>&1 || { fail "this test needs python3"; return; }
	need_ovmf || return
	PXE_T=hive-pxe-proxy
	PXE_MAC=52:54:00:79:00
	U=5a510000-0000-4000-8000-0000000002
	lan_setup
	NB_PORT=$((HIVE_PORT + 1))
	MTOOLS_SKIP_CHECK=1 mcopy -n -i "$MEDIA/savior.img@@$MT_OFF" ::/boot/netboot/boot/ipxe/undionly.kpxe "$W/stick-undionly.kpxe" 2>/dev/null ||
		fail "the stick has no /boot/netboot/boot/ipxe/undionly.kpxe (mkimage --ipxe; install ipxe)"

	# The router first, so the hive's clients find a DHCP server.
	router_vm udhcpd || return
	stick_copy "$W/pxhive.img"
	truncate -s +2G "$W/pxhive.img"
	stick_conf "$W/pxhive.img@@$MT_OFF" "swarm_key = $KEY" "name = hive" "roles = hive" "net = static" \
		"ip = 10.79.0.1/24" "dhcp_server = no" "netboot = yes" "admin_token = $ADMIN_TOKEN" ||
		{ fail "stick setup"; return; }
	boot_options "$W/pxhive.img@@$MT_OFF"
	swarm_vm hive 52:54:00:79:00:01 "${U}01" "$W/pxhive.img" 1024 \
		-netdev "user,id=up,hostfwd=tcp:127.0.0.1:$HIVE_PORT-:7700,hostfwd=tcp:127.0.0.1:$NB_PORT-:7702" \
		-device e1000,netdev=up,mac=52:54:00:79:01:01
	HIVE_PID=$VM_PID
	hserial=$LOGS/$PXE_T-hive.serial
	CUR=$PXE_T/hive
	wait_boot "$hserial" "$HIVE_PID" || { CUR=$PXE_T; return; }
	CUR=$PXE_T
	if ! wait_for "$hserial" 'SAVIOR-NETBOOT: ' 240 "$HIVE_PID"; then
		fail "no SAVIOR-NETBOOT line from the hive within 240 s"
		return
	fi
	info "$(grep -a 'SAVIOR-NETBOOT: ' "$hserial" | tail -n 1 | tr -d '\r')"
	check "the hive answers PXE as a proxy DHCP server, BIOS through iPXE (undionly.kpxe)" \
		grep -a -q -E 'SAVIOR-NETBOOT: mode=proxy iface=[a-z0-9]+ addr=10\.79\.0\.1 bios=undionly uefi=yes' "$hserial"
	if command -v curl >/dev/null 2>&1; then
		# The hive's netboot HTTP server serves the same tree as TFTP.
		curl -s -m 30 -o "$W/undionly.kpxe" "http://127.0.0.1:$NB_PORT/boot/ipxe/undionly.kpxe"
		check "the hive serves the stick's undionly.kpxe" cmp -s "$W/undionly.kpxe" "$W/stick-undionly.kpxe"
		curl -s -m 30 -o "$W/savior.ipxe" "http://127.0.0.1:$NB_PORT/boot/ipxe/savior.ipxe"
		sed 's/^/        | /' "$W/savior.ipxe" >>"$TLOG"
		check "savior.ipxe points next-server at the hive and chains core.0" \
			grep -q -x -F 'set netX/next-server 10.79.0.1' "$W/savior.ipxe"
		check "savior.ipxe chains tftp://10.79.0.1/boot/grub/i386-pc/core.0" \
			grep -q -F 'chain tftp://10.79.0.1/boot/grub/i386-pc/core.0' "$W/savior.ipxe"
	fi

	# What PXE ROMs are offered. Plain ROMs must get iPXE, and iPXE (user
	# class "iPXE", after undionly.kpxe or as the NIC ROM) the script,
	# never iPXE again (no chain loop).
	for uc in none iPXE; do
		want=boot/ipxe/undionly.kpxe
		[ "$uc" = none ] || want=boot/ipxe/savior.ipxe
		out=$(pxe_probe 52:54:00:79:00:f0 10.79.0.240 "$uc" 2>&1)
		printf '%s\n' "$out" | sed "s/^/      $CUR: probe ($uc): /" | tee -a "$TLOG"
		check "a BIOS PXE client with user class $uc gets $want from the hive" \
			[ "$(probe_file "$out")" = "$want" ]
		check "... in the hive's proxy offer and boot server reply (next-server 10.79.0.1)" \
			out_has '^proxy=10\.79\.0\.1 .* siaddr=10\.79\.0\.1$'
	done

	# Diskless VMs (iPXE NIC ROMs): BIOS runs savior.ipxe, UEFI the
	# firmware's own PXE; addresses from the router's range.
	PXE_NET='^10\.79\.0\.[2-5][0-9]$'
	pxe_client bios 1 bios || return
	c1=$VM_PID
	pxe_client uefi 2 uefi || return
	stop_pid "$c1"
	stop_pid "$VM_PID"

	# A router whose DHCP replies name itself as next-server (dnsmasq):
	# GRUB must still find the hive (savior.ipxe sets next-server; GRUB's
	# efinet takes the proxy offer's server).
	stop_pid "$ROUTER_PID"
	router_vm dnsmasq || return
	out=$(pxe_probe 52:54:00:79:00:f1 10.79.0.241 iPXE 2>&1)
	printf '%s\n' "$out" | sed "s/^/      $CUR: probe (iPXE): /" | tee -a "$TLOG"
	check "the dnsmasq router's offers name itself as next-server" \
		out_has '^offer server=10\.79\.0\.254 .*siaddr=10\.79\.0\.254 '
	check "... and the hive still offers savior.ipxe" [ "$(probe_file "$out")" = boot/ipxe/savior.ipxe ]
	PXE_NET='^10\.79\.0\.[6-9][0-9]$'
	pxe_client bios 3 bios-siaddr || return
	c1=$VM_PID
	pxe_client uefi 4 uefi-siaddr || return
	stop_pid "$c1"
	stop_pid "$VM_PID"
	stop_pid "$HIVE_PID"

	# BOOT-03: dhcp_server = yes without net = static must not make the
	# hive a second DHCP server on the router's LAN.
	stick_copy "$W/pxhive2.img"
	truncate -s +2G "$W/pxhive2.img"
	stick_conf "$W/pxhive2.img@@$MT_OFF" "swarm_key = $KEY" "name = hive2" "roles = hive" \
		"dhcp_server = yes" "netboot = yes" || { fail "stick setup"; return; }
	boot_options "$W/pxhive2.img@@$MT_OFF"
	swarm_vm hive2 52:54:00:79:00:02 "${U}02" "$W/pxhive2.img" 1024
	h2=$VM_PID
	CUR=$PXE_T/hive2
	wait_boot "$LOGS/$PXE_T-hive2.serial" "$h2" || { CUR=$PXE_T; return; }
	check_report 'net~^10\.79\.0\.[6-9][0-9]$'
	check "the console says dhcp_server = yes needs net = static" \
		wait_for "$LOGS/$PXE_T-hive2.serial" 'SAVIOR-LOG: .*dhcp_server = yes needs net = static' 60 "$h2"
	if wait_for "$LOGS/$PXE_T-hive2.serial" 'SAVIOR-NETBOOT: ' 240 "$h2"; then
		info "$(grep -a 'SAVIOR-NETBOOT: ' "$LOGS/$PXE_T-hive2.serial" | tail -n 1 | tr -d '\r')"
		check "the hive answers PXE only as a proxy DHCP server" \
			grep -a -q 'SAVIOR-NETBOOT: mode=proxy ' "$LOGS/$PXE_T-hive2.serial"
	else
		fail "no SAVIOR-NETBOOT line from the hive within 240 s"
	fi
	CUR=$PXE_T
}

# ---------------------------------------------------------------------------

START=$(date +%s)
echo "qemu-test: $QEMU (TCG), media from $MEDIA, logs in $LOGS"
for t in $TESTS; do
	begin_test "$t"
	case "$t" in
	image) t_image ;;
	boot-bios) t_boot_bios ;;
	boot-uefi) t_boot_uefi ;;
	boot-iso-bios) t_boot_iso_bios ;;
	boot-iso-uefi) t_boot_iso_uefi ;;
	pxe-bios) t_pxe_bios ;;
	pxe-uefi) t_pxe_uefi ;;
	screen) t_screen ;;
	baked-conf) t_baked_conf ;;
	mem-256) t_mem_256 ;;
	wifi-conf) t_wifi_conf ;;
	wifi) t_wifi ;;
	swarm) t_swarm ;;
	hive-pxe) t_hive_pxe ;;
	hive-pxe-proxy) t_hive_pxe_proxy ;;
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
