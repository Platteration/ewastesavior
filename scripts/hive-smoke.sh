#!/bin/sh
# hive-smoke.sh - run the hive and savior ctl on this computer the way a user
# does on Linux, macOS or Windows (Git Bash), and check that the hive keeps
# its state across a restart. CI runs it on macOS and Windows runners, where
# the hive is a normal program (DESIGN 1, USER_GUIDE 2).
#
# Usage: scripts/hive-smoke.sh SAVIOR_BINARY [WORK_DIR]
#
#   1. savior hive --data WORK/hive --listen 127.0.0.1:PORT --no-beacon
#      generates its certificate, admin token and swarm key; the API answers.
#   2. savior ctl info --json: persistent = true, data_dir = WORK/hive.
#   3. savior ctl node-config writes a savior.conf with the swarm key and
#      the certificate pin; savior ctl upload stores a blob (the platform's
#      free-space and filesystem checks: fs_windows.go, fs_darwin.go).
#   4. The hive is stopped and started again on the same data directory:
#      same certificate fingerprint (ctl.json pins it), still persistent,
#      the blob is still there.
#
# Needs curl. Exit status: 0 all passed, 1 a check failed, 2 usage.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then echo "usage: hive-smoke.sh SAVIOR_BINARY [WORK_DIR]" >&2; exit 2; fi
SAVIOR=$1
[ -f "$SAVIOR" ] || { echo "hive-smoke: no such file: $SAVIOR" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "hive-smoke: curl not found" >&2; exit 2; }
KEEP=yes
if [ $# -eq 2 ]; then
	W=$2
	mkdir -p "$W"
else
	W=$(mktemp -d "${TMPDIR:-/tmp}/hive-smoke.XXXXXX")
	KEEP=no
fi
PORT=$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
HPID=""
# shellcheck disable=SC2317 # called by the EXIT trap
cleanup() {
	[ -z "$HPID" ] || stop_hive
	[ "$KEEP" = yes ] || rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

FAILS=0
ok() { echo "PASS: $*"; }
bad() { FAILS=$((FAILS + 1)); echo "FAIL: $*"; }
die() { bad "$*"; exit 1; }
# check DESCRIPTION COMMAND...: PASS or FAIL depending on COMMAND.
check() {
	_d=$1
	shift
	if "$@"; then ok "$_d"; else bad "$_d"; fi
}

start_hive() {
	"$SAVIOR" hive --data "$W/hive" --listen "127.0.0.1:$PORT" --no-beacon >>"$W/hive.log" 2>&1 &
	HPID=$!
	_t0=$(date +%s)
	until curl -s -k -m 5 -o /dev/null "https://127.0.0.1:$PORT/api/v1/hello"; do
		if ! kill -0 "$HPID" 2>/dev/null; then
			tail -n 20 "$W/hive.log" >&2
			die "the hive exited"
		fi
		[ $(($(date +%s) - _t0)) -lt 60 ] || { tail -n 20 "$W/hive.log" >&2; die "the hive did not answer on 127.0.0.1:$PORT within 60 s"; }
		sleep 1
	done
	ok "the hive answers on 127.0.0.1:$PORT after $(($(date +%s) - _t0)) s"
}

stop_hive() {
	kill "$HPID" 2>/dev/null || true
	_n=0
	while kill -0 "$HPID" 2>/dev/null && [ "$_n" -lt 20 ]; do
		sleep 0.5
		_n=$((_n + 1))
	done
	kill -9 "$HPID" 2>/dev/null || true
	wait "$HPID" 2>/dev/null || true
	HPID=""
}

# ctl ARGS...: savior ctl against the hive; stdout in $W/ctl.out.
ctl() {
	"$SAVIOR" ctl --config "$W/ctl.json" --hive "127.0.0.1:$PORT" --token "$(cat "$W/hive/admin_token")" "$@" \
		>"$W/ctl.out" 2>"$W/ctl.err" || { cat "$W/ctl.err" >&2; return 1; }
}

# json KEY: the value of "KEY" in $W/ctl.out (ctl prints one key per line).
json() { sed -n "s/^ *\"$1\": *\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" "$W/ctl.out" | head -n 1; }

start_hive
[ -s "$W/hive/admin_token" ] || die "no admin_token in $W/hive"
ctl info --json || die "ctl info failed"
fp1=$(json fingerprint)
p=$(json persistent)
dd=$(json data_dir)
check "ctl info: persistent = true (got '$p')" [ "$p" = true ]
case "$fp1" in sha256:*) ok "certificate fingerprint $fp1" ;; *) bad "no certificate fingerprint in ctl info" ;; esac
check "ctl info: data_dir = '$dd'" [ -n "$dd" ]
if ctl node-config --hive-addr "127.0.0.1:$PORT" -o "$W/savior.conf" && grep -q '^swarm_key = ' "$W/savior.conf" &&
	grep -q "^hive_fingerprint = $fp1\$" "$W/savior.conf"; then
	ok "ctl node-config: savior.conf with the swarm key and the fingerprint pin"
else bad "ctl node-config: savior.conf with the swarm key and the fingerprint pin"; fi
printf 'hive-smoke blob %s\n' "$PORT" >"$W/blob.txt"
if ctl upload "$W/blob.txt"; then ok "ctl upload: blob stored"; else bad "ctl upload failed"; fi
ctl blobs --json || true
n1=$(grep -c '"sha256"' "$W/ctl.out" || true)
check "ctl blobs lists the blob ($n1)" [ "$n1" -ge 1 ]

stop_hive
ok "the hive stopped"
start_hive
if ctl info --json; then
	fp2=$(json fingerprint)
	check "same certificate after the restart ($fp2)" [ "$fp2" = "$fp1" ]
	check "still persistent after the restart" [ "$(json persistent)" = true ]
else
	bad "ctl (pinned to $fp1) cannot talk to the restarted hive"
fi
ctl blobs --json || true
n2=$(grep -c '"sha256"' "$W/ctl.out" || true)
check "the blob survived the restart ($n1 before, $n2 after)" [ "$n2" -eq "$n1" ]
stop_hive

echo "hive-smoke: $FAILS failed"
[ "$FAILS" -eq 0 ]
