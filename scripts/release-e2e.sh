#!/bin/sh
# release-e2e.sh - end-to-end test of a SaviorOS boot medium in QEMU, driven
# from this host the way a user sets up a swarm (USER_GUIDE, DESIGN 14).
# The images workflow runs it on the Buildroot release media; `make
# release-e2e-dev` runs it on the dev image.
#
# Usage:
#   scripts/release-e2e.sh [options] (--image FILE | --netboot DIR) [-- QEMU-SMOKE-OPTIONS...]
#
# The node test (always):
#   1. A hive runs on this host: savior hive --listen 127.0.0.1:PORT
#      --no-beacon (QEMU's user network reaches it as 10.0.2.2:PORT).
#   2. savior ctl node-config --hive-addr 10.0.2.2:PORT writes the node's
#      savior.conf (swarm key, hive address, certificate pin), plus
#      ssh_key and name. --image: it goes onto a copy of the stick
#      (qemu-smoke --conf). --netboot: the same settings go on the kernel
#      command line (savior.KEY=VALUE, %-escaped) in a copy of grub.cfg.
#   3. qemu-smoke boots it (SSH forwarded, HMP monitor) and waits for the
#      boot report: key=yes, agent=up, arch=ARCH.
#   4. Checks, each within --timeout seconds:
#      - the node joins: online, machine = ARCH, sandbox strict with every
#        isolation feature (namespaces, cgroup2, seccomp, no_new_privs,
#        dropped privileges);
#      - a job (ctl script --wait --fetch) runs in the task sandbox and
#        reports what internal/runner's TestRealSandboxIsolation probes:
#        a slot uid (10000+), no /proc/cmdline, no /media, /run, /sys or
#        /root, a read-only root and /etc, no network, unshare and mount
#        fail, seccomp mode 2; uname -m = ARCH; the CA bundle is there;
#      - the display: ctl display color (the screen turns black), then
#        ctl display text, and a screendump through the QEMU monitor
#        must equal `savior display render` of the same spec, pixel for
#        pixel (needs python3);
#      - SSH: root logs in with the ssh_key key (id -u = 0; with
#        --version, /etc/savior-release names it); another key is refused.
# The hive test (--hive-role; --image only):
#   5. Another copy of the stick with 2 GiB of free space after its
#      partition boots with roles = auto, hive and an admin_token. Through
#      a forwarded port, ctl info must say persistent = true with data_dir
#      on SAVIOR-DATA (/var/lib/savior/data), and the machine's own node
#      must be online in its hive.
#
# Options:
#   --image FILE        USB stick image (savior-*.img)
#   --netboot DIR       netboot tree (mkimage.sh's netboot/), booted from
#                       QEMU's TFTP server
#   --arch A            x86_64 (default) or i686: the payload the machine
#                       must boot; also picks qemu-system-x86_64 / -i386
#   --firmware F        bios (default), uefi or uefi32 (qemu-smoke)
#   --cpu MODEL         QEMU CPU model, e.g. pentium3,-pae (qemu-smoke)
#   --mem MB            guest RAM (default 512; the hive VM gets at least 512)
#   --accel A           auto (default), kvm or tcg (qemu-smoke)
#   --savior PATH       savior binary for this host: hive, ctl, display
#                       render (default build/linux-amd64/savior, else build/savior)
#   --version V         /etc/savior-release on the guest must contain V
#   --timeout S         boot timeout, and the limit for each later check
#                       (default 900)
#   --task-mem MB       memory the probe task asks for (default 16)
#   --hive-role         also run the hive test
#   --out DIR           logs, serial consoles, screendumps, task outputs
#                       (default build/logs/release-e2e)
#   --name NAME         prefix of the files in DIR (default: e2e-ARCH)
#   -- OPTIONS          more qemu-smoke.sh options for every boot, e.g. the
#                       images workflow's $BOOT_EXPECT
#
# Needs: qemu-system-x86, python3, curl, jq, ssh + ssh-keygen (OpenSSH), and
# mtools for --image. Every check prints PASS: or FAIL:. Exit status: 0 all
# passed, 1 a check failed, 2 usage error.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
die() { echo "release-e2e: $*" >&2; exit 2; }

IMAGE=""
NETBOOT=""
ARCH=x86_64
FIRMWARE=bios
CPU=""
MEM=512
ACCEL=auto
SAVIOR=""
VERSION=""
TIMEOUT=900
TASK_MEM=16
HIVE_ROLE=no
OUT=$ROOT/build/logs/release-e2e
NAME=""
while [ $# -gt 0 ]; do
	case "$1" in
	--image) IMAGE=$2; shift 2 ;;
	--netboot) NETBOOT=$2; shift 2 ;;
	--arch) ARCH=$2; shift 2 ;;
	--firmware) FIRMWARE=$2; shift 2 ;;
	--cpu) CPU=$2; shift 2 ;;
	--mem) MEM=$2; shift 2 ;;
	--accel) ACCEL=$2; shift 2 ;;
	--savior) SAVIOR=$2; shift 2 ;;
	--version) VERSION=$2; shift 2 ;;
	--timeout) TIMEOUT=$2; shift 2 ;;
	--task-mem) TASK_MEM=$2; shift 2 ;;
	--hive-role) HIVE_ROLE=yes; shift ;;
	--out) OUT=$2; shift 2 ;;
	--name) NAME=$2; shift 2 ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	--) shift; break ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done
# "$@" now holds the extra qemu-smoke options.

if [ -n "$IMAGE" ] && [ -n "$NETBOOT" ]; then die "give --image or --netboot, not both"; fi
if [ -n "$IMAGE" ]; then
	[ -f "$IMAGE" ] || die "image not found: $IMAGE"
elif [ -n "$NETBOOT" ]; then
	[ -f "$NETBOOT/boot/grub/grub.cfg" ] || die "$NETBOOT is not a netboot tree (no boot/grub/grub.cfg)"
	[ "$HIVE_ROLE" = no ] || die "--hive-role needs --image (a netbooted machine has no stick for SAVIOR-DATA)"
else
	die "give --image FILE or --netboot DIR"
fi
case "$ARCH" in x86_64|i686) ;; *) die "--arch must be x86_64 or i686" ;; esac
case "$TIMEOUT$MEM$TASK_MEM" in *[!0-9]*) die "--timeout, --mem and --task-mem take numbers" ;; esac
if [ -z "$SAVIOR" ]; then
	for s in "$ROOT/build/linux-amd64/savior" "$ROOT/build/savior"; do
		[ -n "$SAVIOR" ] || { [ -x "$s" ] && SAVIOR=$s; } || true
	done
	[ -n "$SAVIOR" ] || die "no savior binary for this host (make build-linux-amd64, or --savior PATH)"
fi
"$SAVIOR" version >/dev/null 2>&1 || die "$SAVIOR does not run on this host"
for c in python3 curl jq ssh ssh-keygen; do
	command -v "$c" >/dev/null 2>&1 || die "$c not found (apt install python3 curl jq openssh-client)"
done
[ -z "$IMAGE" ] || command -v mcopy >/dev/null 2>&1 || die "mcopy not found (apt install mtools)"
[ -n "$NAME" ] || NAME=e2e-$ARCH
case "$NAME" in *[!A-Za-z0-9._-]*|"") die "--name: letters, digits, '.', '_' and '-' only" ;; esac

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
LOG=$OUT/$NAME.log
: >"$LOG"
W=$(mktemp -d "${TMPDIR:-/tmp}/release-e2e.XXXXXX")
HIVE_PID=""
# shellcheck disable=SC2317 # called by the EXIT trap
# The exit status stays the tests' result: nothing here may fail the run.
cleanup() {
	rc=$?
	set +e
	for f in "$W"/*.qpid; do
		[ -f "$f" ] || continue
		kill "$(cat "$f")" 2>/dev/null
	done
	if [ -n "$HIVE_PID" ]; then
		# Wait for the hive: it writes its state while it shuts down, and
		# removing $W under it fails ("Directory not empty").
		kill "$HIVE_PID" 2>/dev/null
		n=0
		while kill -0 "$HIVE_PID" 2>/dev/null && [ "$n" -lt 20 ]; do
			sleep 0.5
			n=$((n + 1))
		done
		kill -9 "$HIVE_PID" 2>/dev/null
		wait "$HIVE_PID" 2>/dev/null
	fi
	rm -rf "$W" 2>/dev/null || { sleep 1; rm -rf "$W" 2>/dev/null; }
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

PASSES=0
FAILS=0
CUR=node
say() { echo "$*" | tee -a "$LOG"; }
pass() { PASSES=$((PASSES + 1)); say "PASS: $CUR: $*"; }
fail() { FAILS=$((FAILS + 1)); say "FAIL: $CUR: $*"; }
info() { say "      $CUR: $*"; }
# check DESCRIPTION COMMAND...: PASS or FAIL depending on COMMAND.
check() {
	_d=$1
	shift
	if "$@" >>"$LOG" 2>&1; then pass "$_d"; else fail "$_d"; fi
}
# rand N: a random number 0..N-1.
rand() { echo $(($(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % $1)); }
hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# ctl ARGS...: savior ctl against $CTL_HIVE with $CTL_TOKEN (ctl.json per
# hive: it pins the certificate); output in $W/ctl.out and $W/ctl.err, both
# copied to the log.
ctl() {
	echo "\$ savior ctl $*" >>"$LOG"
	"$SAVIOR" ctl --config "$W/ctl-${CTL_HIVE##*:}.json" --hive "$CTL_HIVE" --token "$CTL_TOKEN" "$@" >"$W/ctl.out" 2>"$W/ctl.err"
	_rc=$?
	sed 's/^/    > /' "$W/ctl.out" "$W/ctl.err" | head -n 60 >>"$LOG"
	return "$_rc"
}

# wait_api PORT SECONDS: the hive API answers on 127.0.0.1:PORT.
wait_api() {
	_t0=$(date +%s)
	until curl -s -k -m 5 -o /dev/null "https://127.0.0.1:$1/api/v1/hello"; do
		[ $(($(date +%s) - _t0)) -lt "$2" ] || return 1
		sleep 3
	done
}

# boot WHAT SERIAL PIDFILE QEMU-SMOKE-OPTIONS...: qemu-smoke with this
# script's machine options, leaving QEMU running on success.
boot() {
	_what=$1 _serial=$2 _pidfile=$3
	shift 3
	set -- --arch "$ARCH" --firmware "$FIRMWARE" --accel "$ACCEL" --timeout "$TIMEOUT" \
		--log "$_serial" --keep-running "$_pidfile" "$@"
	[ -z "$CPU" ] || set -- --cpu "$CPU" "$@"
	echo "\$ sh scripts/qemu-smoke.sh $*" >>"$LOG"
	_t0=$(date +%s)
	if sh "$HERE/qemu-smoke.sh" "$@" >"$W/smoke.out" 2>&1; then
		pass "$_what booted, boot report after $(($(date +%s) - _t0)) s"
		grep -a -E '^qemu-smoke:   SAVIOR-(BOOT|AGENT): ' "$W/smoke.out" | sed 's/^qemu-smoke:   //' | while IFS= read -r l; do info "$l"; done
		cat "$W/smoke.out" >>"$LOG"
		return 0
	fi
	fail "$_what did not boot: $(grep -a 'qemu-smoke: FAIL' "$W/smoke.out" | head -n 1)"
	tee -a "$LOG" <"$W/smoke.out" >&2
	return 1
}

# hmp SOCKET COMMAND: one QEMU monitor command.
hmp() {
	python3 - "$1" "$2" <<'PY'
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
PY
}

# screendump SOCKET FILE: a PPM of the guest's screen; 0 when written.
screendump() {
	rm -f "$2"
	hmp "$1" "screendump $2" >/dev/null 2>&1 || return 1
	_n=0
	while [ ! -s "$2" ] && [ "$_n" -lt 20 ]; do
		sleep 0.5
		_n=$((_n + 1))
	done
	[ -s "$2" ]
}

# ppm_stats FILE: "WIDTH HEIGHT NONZERO_BYTES TOTAL_BYTES" of a P6 PPM, as
# os/dev/qemu-test.sh computes it.
ppm_stats() {
	_hdr=$(head -n 3 "$1" | wc -c)
	_dim=$(sed -n 2p "$1")
	_tot=$(($(wc -c <"$1") - _hdr))
	_nz=$(tail -c "$_tot" "$1" | tr -d '\000' | wc -c)
	echo "$_dim $_nz $_tot"
}

# img_diff PNG PPM: how many pixels differ between an 8-bit RGB/RGBA PNG
# (savior display render) and a P6 PPM screendump; "size" when the sizes
# differ. The same comparison as os/dev/qemu-test.sh's swarm test.
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

# ssh_to PORT KEY COMMAND: COMMAND as root on the forwarded port with only
# KEY; output in $W/ssh.out and $W/ssh.err.
ssh_to() {
	ssh -p "$1" -i "$2" -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=20 \
		-o LogLevel=ERROR root@127.0.0.1 "$3" </dev/null >"$W/ssh.out" 2>"$W/ssh.err"
}

# below DIR PATH: PATH is inside DIR.
# shellcheck disable=SC2317 # called through check
below() { case "$2" in "$1"/*) return 0 ;; esac; return 1; }

# pct VALUE: VALUE %-escaped for a savior.KEY=VALUE kernel argument (spaces,
# quotes and the characters GRUB would expand).
pct() { printf '%s' "$1" | sed -e 's/%/%25/g' -e 's/ /%20/g' -e 's/"/%22/g' -e 's/\$/%24/g' -e 's/\\/%5C/g' -e "s/'/%27/g"; }

# conf_cmdline FILE: the settings of a savior.conf as savior.KEY=VALUE words.
conf_cmdline() {
	_out=""
	while IFS= read -r _l; do
		_l=$(printf '%s' "$_l" | tr -d '\r')
		case "$_l" in ""|"#"*) continue ;; esac
		_k=$(printf '%s' "${_l%%=*}" | tr -d ' \t')
		_v=$(printf '%s' "${_l#*=}" | sed 's/^[ \t]*//; s/[ \t]*$//')
		_out="$_out${_out:+ }savior.$_k=$(pct "$_v")"
	done <"$1"
	printf '%s\n' "$_out"
}

# finish: the summary; exit 0 when every check passed.
finish() {
	say "release-e2e: $PASSES passed, $FAILS failed (logs in $OUT/$NAME*)"
	[ "$FAILS" -eq 0 ] || exit 1
	exit 0
}

# ---------------------------------------------------------------------------
# 1. The hive on this host

say "release-e2e: $(date -u '+%Y-%m-%dT%H:%M:%SZ') ${IMAGE:-$NETBOOT} as $ARCH (firmware $FIRMWARE${CPU:+, cpu $CPU}, $MEM MB), host savior $("$SAVIOR" version 2>/dev/null | head -n 1)"
HPORT=$((40000 + $(rand 20000)))
SSH_PORT=$((20000 + $(rand 20000)))
"$SAVIOR" hive --data "$W/hive" --listen "127.0.0.1:$HPORT" --no-beacon >"$OUT/$NAME-hive.log" 2>&1 &
HIVE_PID=$!
if ! wait_api "$HPORT" 60; then
	fail "the hive on this host did not answer on 127.0.0.1:$HPORT within 60 s"
	tail -n 20 "$OUT/$NAME-hive.log" | tee -a "$LOG" >&2
	finish
fi
pass "hive on this host answers on 127.0.0.1:$HPORT"
CTL_HIVE=127.0.0.1:$HPORT
CTL_TOKEN=$(cat "$W/hive/admin_token")

# node_test QEMU-SMOKE-OPTIONS...: steps 2-4. 1 when the node never came up
# (the later checks would only repeat that).
node_test() {
	# 2. The node's settings, made the way the user guide says.
	if ! { ssh-keygen -q -t ed25519 -N '' -C release-e2e -f "$W/id" &&
		ssh-keygen -q -t ed25519 -N '' -C not-authorized -f "$W/id-other"; } >>"$LOG" 2>&1; then
		fail "ssh-keygen failed"
		return 1
	fi
	if ! ctl node-config --hive-addr "10.0.2.2:$HPORT" -o "$W/savior.conf"; then
		fail "ctl node-config failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
		return 1
	fi
	NODE_NAME=e2e-node
	printf 'name = %s\nssh_key = %s\n' "$NODE_NAME" "$(cat "$W/id.pub")" >>"$W/savior.conf"
	pass "ctl node-config wrote the node's savior.conf (swarm key, hive 10.0.2.2:$HPORT, fingerprint pin)"

	# 3. Boot.
	SERIAL=$OUT/$NAME-node.serial
	MON=$W/node.mon
	set -- --mem "$MEM" --monitor "$MON" --hostfwd "tcp:127.0.0.1:$SSH_PORT-:22" \
		--expect key=yes --expect agent=up --expect "arch=$ARCH" --fail ver=unknown "$@"
	if [ -n "$IMAGE" ]; then
		set -- --image "$IMAGE" --conf "$W/savior.conf" --append savior_dumplog=1 "$@"
	else
		# Test only: a netbooted machine has no stick, so the settings (and
		# the swarm key) ride on the kernel command line.
		set -- --netboot "$NETBOOT" --append "$(conf_cmdline "$W/savior.conf") savior_dumplog=1" "$@"
	fi
	boot "the ${IMAGE:+stick}${NETBOOT:+netboot tree}" "$SERIAL" "$W/node.qpid" "$@" || return 1

	# 4. The node joins.
	t0=$(date +%s)
	node=""
	while [ $(($(date +%s) - t0)) -lt "$TIMEOUT" ]; do
		if ctl nodes --json; then
			node=$(jq -c --arg n "$NODE_NAME" '[.[] | select(.name == $n and .liveness == "online")][0] // empty' "$W/ctl.out")
			[ -z "$node" ] || break
		fi
		sleep 5
	done
	if [ -z "$node" ]; then
		fail "the node did not come online in the hive within $TIMEOUT s: $(tail -n 1 "$W/ctl.err")"
		return 1
	fi
	printf '%s\n' "$node" >"$OUT/$NAME-node.json"
	pass "the node joined the hive and is online after $(($(date +%s) - t0)) s"
	NODE_ID=$(printf '%s' "$node" | jq -r .id)
	m=$(printf '%s' "$node" | jq -r '.inventory.machine // empty' || true)
	check "the node reports machine $ARCH (got '$m')" [ "$m" = "$ARCH" ]
	sb=$(printf '%s' "$node" | jq -r '"\(.sandbox) full_isolation=\(.full_isolation)"' || true)
	caps=$(printf '%s' "$node" | jq -r '.sandbox_caps // [] | join(" ")' || true)
	check "the node's task sandbox is strict with full isolation (got '$sb')" [ "$sb" = "strict full_isolation=true" ]
	missing=""
	for c in mountns pidns netns ipcns utsns cgroup2 seccomp nnp privdrop; do
		case " $caps " in *" $c "*) ;; *) missing="$missing $c" ;; esac
	done
	check "the kernel gives the sandbox every isolation feature (have: $caps${missing:+; missing:$missing})" [ -z "$missing" ]

	# A job in the task sandbox. The probe is what internal/runner's
	# TestRealSandboxIsolation (and the dev image's swarm test) checks, here
	# with this image's kernel, init and BusyBox.
	cat >"$W/probe.sh" <<-'EOF'
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
		echo "machine=$(uname -m)"
		echo "ca=$(grep -c -e '-----BEGIN CERTIFICATE-----' /etc/ssl/certs/ca-certificates.crt 2>/dev/null)"
	} > probe.txt
	EOF
	rm -rf "$OUT/$NAME-task"
	t0=$(date +%s)
	if ctl --timeout 60s script "$W/probe.sh" --name release-e2e-probe --cores 0.5 --mem "$TASK_MEM" --disk 16 \
		--timeout "$TIMEOUT" --output probe.txt --fetch "$OUT/$NAME-task"; then
		pass "a sandboxed job ran and its output was fetched after $(($(date +%s) - t0)) s"
	else
		fail "the job failed: $(tail -n 3 "$W/ctl.err" | tr '\n' ' ')"
	fi
	probe=$(find "$OUT/$NAME-task" -name probe.txt -type f 2>/dev/null | head -n 1)
	if [ -n "$probe" ]; then
		info "the task said: $(tr '\n' ' ' <"$probe")"
		u=$(sed -n 's/^uid=//p' "$probe")
		check "sandbox: the task runs as a slot uid >= 10000 (uid=$u)" [ "${u:-0}" -ge 10000 ]
		for kv in 'cmdline=[]' media=absent run=absent sys=absent root=absent rootwrite=fail etcwrite=fail \
			extranet=absent unshare=fail mount=fail seccomp=2 "machine=$ARCH"; do
			check "sandbox: $kv (got '$(grep "^${kv%%=*}=" "$probe")')" grep -q -x -F -- "$kv" "$probe"
		done
		ca=$(sed -n 's/^ca=//p' "$probe")
		check "the task sees the CA bundle ($ca certificates)" [ "${ca:-0}" -ge 100 ]
	else
		fail "no probe.txt among the job's outputs"
	fi

	# The display: black first (the status screen redraws by itself), then text
	# that must match savior display render pixel for pixel.
	CUR=node/display
	if ctl display "$NODE_ID" color --bg 000000; then
		pass "ctl display color #000000"
	else
		fail "ctl display color failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	nz=-
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt "$TIMEOUT" ]; do
		sleep 3
		screendump "$MON" "$OUT/$NAME-black.ppm" || continue
		# shellcheck disable=SC2046 # WIDTH HEIGHT NONZERO TOTAL
		set -- $(ppm_stats "$OUT/$NAME-black.ppm")
		nz=$3
		[ "$nz" -eq 0 ] && break
	done
	check "the screen turned black ($nz non-zero bytes)" [ "$nz" = 0 ]
	TEXT="SAVIOR $ARCH OK"
	printf '{"mode": "text", "text": "%s"}\n' "$TEXT" >"$W/text-spec.json"
	if ctl display "$NODE_ID" text --text "$TEXT"; then
		pass "ctl display text '$TEXT'"
	else
		fail "ctl display text failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
	fi
	pxdiff=-
	ref=$OUT/$NAME-text-ref.png
	rm -f "$ref" "$OUT/$NAME-text.ppm"
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt "$TIMEOUT" ]; do
		sleep 3
		screendump "$MON" "$OUT/$NAME-text.ppm" || continue
		# shellcheck disable=SC2046
		set -- $(ppm_stats "$OUT/$NAME-text.ppm")
		[ "$3" -gt 0 ] || continue
		if [ ! -s "$ref" ]; then
			"$SAVIOR" display render --spec "$W/text-spec.json" --size "$1x$2" --out "$ref" >>"$LOG" 2>&1 ||
				{ fail "savior display render failed"; break; }
		fi
		pxdiff=$(img_diff "$ref" "$OUT/$NAME-text.ppm" 2>>"$LOG" || echo error)
		[ "$pxdiff" = 0 ] && break
	done
	check "the screen shows exactly what savior display render draws for the spec ($pxdiff pixels differ; $OUT/$NAME-text.ppm vs $ref)" \
		[ "$pxdiff" = 0 ]

	# SSH with the ssh_key key.
	CUR=node/ssh
	t0=$(date +%s)
	ok=no
	while [ $(($(date +%s) - t0)) -lt "$TIMEOUT" ]; do
		if ssh_to "$SSH_PORT" "$W/id" 'id -u; cat /etc/savior-release' && [ "$(head -n 1 "$W/ssh.out")" = 0 ]; then
			ok=yes
			break
		fi
		sleep 5
	done
	sed 's/^/    ssh> /' "$W/ssh.out" "$W/ssh.err" >>"$LOG"
	if [ "$ok" = yes ]; then
		pass "root logs in over SSH with the ssh_key key after $(($(date +%s) - t0)) s"
		if [ -n "$VERSION" ]; then
			check "/etc/savior-release names $VERSION ($(sed -n '2,$p' "$W/ssh.out" | tr '\n' ' '))" \
				grep -q -F -- "$VERSION" "$W/ssh.out"
		fi
		ssh_to "$SSH_PORT" "$W/id-other" 'id -u' || true
		check "a key that is not in ssh_key is refused ($(tr '\n' ' ' <"$W/ssh.err"))" grep -q 'Permission denied' "$W/ssh.err"
	else
		fail "no root login over SSH within $TIMEOUT s: $(tail -n 2 "$W/ssh.err" | tr '\n' ' ')"
	fi
	kill "$(cat "$W/node.qpid")" 2>/dev/null || true
	rm -f "$W/node.qpid"
}

# ---------------------------------------------------------------------------
# 5. The hive role on this medium, with SAVIOR-DATA on the stick.

# hive_test QEMU-SMOKE-OPTIONS...
hive_test() {
	CUR=hive
	HT=$(hex 20)
	HAPI=$((40000 + $(rand 20000)))
	[ "$HAPI" != "$HPORT" ] || HAPI=$((HAPI + 1))
	HMEM=$MEM
	[ "$HMEM" -ge 512 ] || HMEM=512
	printf 'swarm_key = %s\nname = e2e-hive\nroles = auto, hive\nadmin_token = %s\n' "$(hex 16)" "$HT" >"$W/hive.conf"
	# The free space after the partition is where the hive makes SAVIOR-DATA.
	if boot "the stick as a hive" "$OUT/$NAME-hive-vm.serial" "$W/hive.qpid" --image "$IMAGE" --conf "$W/hive.conf" \
		--grow 2048 --append savior_dumplog=1 --mem "$HMEM" --hostfwd "tcp:127.0.0.1:$HAPI-:7700" \
		--expect key=yes --expect agent=up --expect "arch=$ARCH" --fail ver=unknown "$@"; then
		CTL_HIVE=127.0.0.1:$HAPI
		CTL_TOKEN=$HT
		if wait_api "$HAPI" "$TIMEOUT"; then
			pass "the hive API answers through the forwarded port"
			if ctl info --json; then
				cp "$W/ctl.out" "$OUT/$NAME-hive-info.json"
				p=$(jq -r .persistent "$W/ctl.out")
				dd=$(jq -r '.data_dir // empty' "$W/ctl.out")
				check "the hive keeps its state on disk (persistent = $p)" [ "$p" = true ]
				check "... on SAVIOR-DATA (data_dir $dd below /var/lib/savior/data)" \
					below /var/lib/savior/data "$dd"
			else
				fail "ctl info failed: $(tail -n 2 "$W/ctl.err" | tr '\n' ' ')"
			fi
			t0=$(date +%s)
			n=0
			while [ $(($(date +%s) - t0)) -lt "$TIMEOUT" ]; do
				if ctl nodes --json; then
					n=$(jq '[.[] | select(.liveness == "online")] | length' "$W/ctl.out")
					[ "$n" -ge 1 ] && break
				fi
				sleep 5
			done
			check "the hive machine's own node is online in its hive ($n online)" [ "$n" -ge 1 ]
		else
			fail "the hive API (127.0.0.1:$HAPI -> :7700) did not answer within $TIMEOUT s"
		fi
		grep -a 'SAVIOR-SYSLOG: .*\(hive data\|SAVIOR-DATA\|init-data\)' "$OUT/$NAME-hive-vm.serial" | tr -d '\r' |
			tail -n 5 | while IFS= read -r l; do info "$l"; done
	fi
	kill "$(cat "$W/hive.qpid" 2>/dev/null)" 2>/dev/null || true
	rm -f "$W/hive.qpid"
}

# (Called in an || list, the tests handle their own failures: set -e is off.)
node_test "$@" || true
[ "$HIVE_ROLE" = no ] || hive_test "$@" || true
finish
