# shellcheck shell=sh
# /usr/libexec/savior/lib.sh - helpers shared by the SaviorOS init scripts.
#
# Sourced (never executed) by /etc/init.d/rc[SK], the S?? scripts and the
# helpers in /usr/libexec/savior. POSIX sh, BusyBox ash compatible.
#
# Conventions: variables from /run/savior/env (SAVIOR_*) are plain shell
# variables and are never exported, so secrets such as the swarm key don't
# leak into child processes. Scripts use ${SAVIOR_X:-default} everywhere so
# they also work when the env file is missing.

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
umask 077

# shellcheck disable=SC2034 # used by the scripts that source this file
{
	RUN_DIR=/run/savior
	BOOT_LOG=/var/log/savior-init.log
	LIBEXEC=/usr/libexec/savior
}

have() { command -v "$1" >/dev/null 2>&1; }

# uptime_s: seconds since boot ("12.34"), or "?" before /proc is mounted.
uptime_s() {
	_up='?'
	{ read -r _up _ </proc/uptime; } 2>/dev/null || _up='?'
	echo "$_up"
}

# log MESSAGE...: to syslog (when available) and the boot log file.
log() {
	if have logger; then
		logger -t savior-init -- "$*" 2>/dev/null || :
	fi
	{ printf '[%s] %s\n' "$(uptime_s)" "$*" >>"$BOOT_LOG"; } 2>/dev/null || :
}

# say MESSAGE...: log plus a short line on the console. Only for foreground
# boot steps; background jobs must not write to the console, which belongs
# to `savior console` once rcS is done.
say() {
	log "$*"
	echo "savior: $*"
}

# serial_mark LINE: log LINE and, when the kernel console includes the
# first serial port (console=ttyS0, as in the QEMU tests or with a serial
# cable), print it there too. For short machine-readable status lines only;
# never pass secrets.
serial_mark() {
	log "$*"
	case " $(cat /proc/cmdline 2>/dev/null) " in
	*" console=ttyS0"*)
		{ printf '%s\r\n' "$*" >/dev/ttyS0; } 2>/dev/null || :
		;;
	esac
}

# log_file FILE: log each line of FILE (e.g. captured stderr) and delete it.
log_file() {
	[ -s "$1" ] || { rm -f "$1"; return 0; }
	while IFS= read -r _l; do
		log "$_l"
	done <"$1"
	rm -f "$1"
}

# load_env: read the merged config written by S08config.
load_env() {
	if [ -r "$RUN_DIR/env" ]; then
		# shellcheck source=/dev/null
		. "$RUN_DIR/env"
	fi
}

is_yes() {
	case "${1:-}" in yes | y | true | 1 | on) return 0 ;; esac
	return 1
}

# is_mounted DIR: DIR is a mount point.
is_mounted() {
	[ -r /proc/mounts ] || return 1
	awk -v m="$1" '$2 == m { f = 1 } END { exit !f }' /proc/mounts
}

# mount_source DIR: print the device mounted on DIR.
mount_source() {
	[ -r /proc/mounts ] || return 1
	awk -v m="$1" '$2 == m { s = $1 } END { if (s == "") exit 1; print s }' /proc/mounts
}

# ---------------------------------------------------------------------------
# Background jobs

# running NAME: /run/savior/NAME.pid names a live process.
running() {
	[ -f "$RUN_DIR/$1.pid" ] || return 1
	_pid=$(cat "$RUN_DIR/$1.pid" 2>/dev/null) || return 1
	[ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null
}

# daemonize NAME PROGRAM [ARGS...]: start PROGRAM detached from the console
# (start-stop-daemon -b when available) with pidfile /run/savior/NAME.pid,
# unless it is already running.
daemonize() {
	_name=$1
	shift
	running "$_name" && return 0
	mkdir -p "$RUN_DIR"
	if have start-stop-daemon; then
		_exe=$1
		shift
		start-stop-daemon -S -q -b -m -p "$RUN_DIR/$_name.pid" -x "$_exe" -- "$@"
	else
		setsid "$@" </dev/null >/dev/null 2>&1 &
		echo "$!" >"$RUN_DIR/$_name.pid"
	fi
}

# stop_job NAME: SIGTERM the process in /run/savior/NAME.pid, wait up to
# 5 s, then SIGKILL; remove the pidfile.
stop_job() {
	if running "$1"; then
		_pid=$(cat "$RUN_DIR/$1.pid")
		kill "$_pid" 2>/dev/null
		_n=0
		while kill -0 "$_pid" 2>/dev/null && [ "$_n" -lt 50 ]; do
			sleep 0.1 2>/dev/null || sleep 1
			_n=$((_n + 1))
		done
		kill -9 "$_pid" 2>/dev/null
	fi
	rm -f "$RUN_DIR/$1.pid"
}

# ---------------------------------------------------------------------------
# Network helpers

is_wireless() { [ -d "/sys/class/net/$1/wireless" ] || [ -e "/sys/class/net/$1/phy80211" ]; }

# is_physical_ether IFACE: a real (not virtual) Ethernet-type interface.
is_physical_ether() {
	[ -e "/sys/class/net/$1/device" ] || return 1
	[ "$(cat "/sys/class/net/$1/type" 2>/dev/null)" = 1 ]
}

# wired_ifaces / wireless_ifaces: print interface names, one per line.
wired_ifaces() {
	for _d in /sys/class/net/*; do
		_i=${_d##*/}
		[ -e "$_d" ] || continue
		[ "$_i" != lo ] || continue
		is_physical_ether "$_i" || continue
		is_wireless "$_i" && continue
		echo "$_i"
	done
}
wireless_ifaces() {
	for _d in /sys/class/net/*; do
		_i=${_d##*/}
		[ -e "$_d" ] && is_wireless "$_i" && echo "$_i"
	done
	return 0
}
first_wired() { wired_ifaces | head -n 1; }
first_wireless() { wireless_ifaces | head -n 1; }

# if_ipv4 IFACE: print "ADDR PREFIX" of the first global IPv4 address.
if_ipv4() {
	ip -4 addr show dev "$1" 2>/dev/null |
		sed -n 's/^ *inet \([0-9.]*\)\/\([0-9]*\).*/\1 \2/p' |
		grep -v '^169\.254\.' | head -n 1
}

# ip2int A.B.C.D / int2ip N / prefix2mask N: IPv4 arithmetic (64-bit shell
# arithmetic, as in BusyBox ash and dash).
ip2int() {
	_o=$IFS
	IFS=.
	# shellcheck disable=SC2086 # splitting the address on dots is the point
	set -- $1
	IFS=$_o
	echo $((($1 << 24) + ($2 << 16) + ($3 << 8) + $4))
}
int2ip() {
	echo "$((($1 >> 24) & 255)).$((($1 >> 16) & 255)).$((($1 >> 8) & 255)).$(($1 & 255))"
}
prefix2mask() {
	if [ "$1" -le 0 ]; then
		echo 0.0.0.0
	else
		int2ip $(((0xffffffff << (32 - $1)) & 0xffffffff))
	fi
}
# network_of ADDR PREFIX: print the network address.
network_of() {
	_m=$(ip2int "$(prefix2mask "$2")")
	int2ip $(($(ip2int "$1") & _m))
}

# dhcp_range ADDR PREFIX: print "FIRST LAST" for a DHCP server: dhcp_range
# from the config, else .100-.200 of the subnet (clamped to small subnets).
dhcp_range() {
	if [ -n "${SAVIOR_DHCP_RANGE:-}" ]; then
		echo "${SAVIOR_DHCP_RANGE%%-*} ${SAVIOR_DHCP_RANGE#*-}"
		return 0
	fi
	_net=$(ip2int "$(network_of "$1" "$2")")
	_size=$((1 << (32 - $2)))
	_first=100
	_last=200
	if [ "$_size" -le 256 ]; then
		# small subnets: the upper part of the range, without broadcast
		[ "$_last" -ge $((_size - 1)) ] && _last=$((_size - 2))
		[ "$_first" -ge "$_last" ] && _first=$((_size / 2))
	fi
	echo "$(int2ip $((_net + _first))) $(int2ip $((_net + _last)))"
}

# write_resolv FILE NAMESERVER... [search DOMAIN]: world-readable resolv.conf
# (tasks with network access get a read-only copy).
write_resolv() {
	_f=$1
	shift
	_tmp="$_f.tmp.$$"
	: >"$_tmp" || return 1
	for _ns in "$@"; do
		[ -n "$_ns" ] && echo "nameserver $_ns" >>"$_tmp"
	done
	chmod 0644 "$_tmp"
	mv -f "$_tmp" "$_f"
}

# ---------------------------------------------------------------------------
# Block devices

# disk_of DEV: print the whole-disk kernel name of /dev/DEV (sdb1 -> sdb).
disk_of() {
	_n=${1##*/}
	if [ -e "/sys/class/block/$_n/partition" ]; then
		_p=$(readlink -f "/sys/class/block/$_n")
		_p=${_p%/*}
		echo "${_p##*/}"
	else
		echo "$_n"
	fi
}

# mkfs_ext4 LABEL DEV [RESERVED_PERCENT]: ext4 with e2fsprogs, else ext2 with
# BusyBox mke2fs. e2fsprogs is recognized by behavior ("-V" works), since
# BusyBox ash may report applets without a path.
mkfs_ext4() {
	for _t in mkfs.ext4 mke2fs; do
		if have "$_t" && "$_t" -V >/dev/null 2>&1; then
			if [ "$_t" = mkfs.ext4 ]; then
				"$_t" -q -F -m "${3:-1}" -L "$1" "$2"
			else
				"$_t" -q -F -t ext4 -m "${3:-1}" -L "$1" "$2"
			fi
			return
		fi
	done
	if have mke2fs; then
		mke2fs -F -m "${3:-1}" -L "$1" "$2"
	else
		busybox mke2fs -F -m "${3:-1}" -L "$1" "$2"
	fi
}
