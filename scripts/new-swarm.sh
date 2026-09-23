#!/bin/sh
# new-swarm.sh - start a new swarm: generate a swarm key and write a
# savior.conf to copy onto the SAVIOR partition of every stick.
#
# Usage:
#   scripts/new-swarm.sh [options]
#
#   --out FILE          where to write (default ./savior.conf; "-" = stdout)
#   --key KEY           use this swarm key instead of generating one
#   --hive ADDR         hive address for the nodes (host, host:port, https://...)
#   --fingerprint FP    hive certificate pin (sha256:<64 hex>), strongly
#                       recommended; "savior ctl node-config" on a running
#                       hive writes a complete file including it
#   --wifi-ssid SSID    Wi-Fi network (with --wifi-psk, optional --wifi-country)
#   --wifi-psk PSK
#   --wifi-country CC   two-letter regulatory domain (default US)
#   --force             overwrite an existing file
#
# The key is 32 Crockford base32 characters (160 bits), the same format as
# "savior ctl genkey". The file is created with mode 0600: it holds a secret.
set -eu

die() { echo "new-swarm: $*" >&2; exit 1; }

OUT=savior.conf
KEY=""
HIVE=""
FP=""
SSID=""
PSK=""
COUNTRY=""
FORCE=no
while [ $# -gt 0 ]; do
	case "$1" in
	--out) OUT=$2; shift 2 ;;
	--key) KEY=$2; shift 2 ;;
	--hive) HIVE=$2; shift 2 ;;
	--fingerprint) FP=$2; shift 2 ;;
	--wifi-ssid) SSID=$2; shift 2 ;;
	--wifi-psk) PSK=$2; shift 2 ;;
	--wifi-country) COUNTRY=$2; shift 2 ;;
	--force) FORCE=yes; shift ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

if [ -z "$KEY" ]; then
	# 256 is a multiple of 32, so filtering bytes to the 32-letter alphabet is unbiased.
	KEY=$(LC_ALL=C tr -dc '0-9A-HJKMNP-TV-Z' </dev/urandom | dd bs=32 count=1 2>/dev/null)
	[ "${#KEY}" -eq 32 ] || die "could not read /dev/urandom"
fi
[ "${#KEY}" -ge 16 ] || die "swarm key must be at least 16 characters"
case "$KEY" in *[!A-Za-z0-9._~+/=-]*) die "swarm key: use letters, digits and ._~+/=- only" ;; esac
if [ -n "$FP" ]; then
	# Same normalization as the config parser: lower case, optional
	# "sha256:" prefix, colons between bytes allowed.
	FP=$(printf '%s' "$FP" | LC_ALL=C tr '[:upper:]' '[:lower:]' | sed 's/^sha256://' | tr -d ': ')
	FP="sha256:$FP"
	printf '%s\n' "$FP" | grep -q '^sha256:[0-9a-f]\{64\}$' || die "fingerprint must look like sha256:<64 hex digits>"
fi
case "$HIVE" in *[[:space:]\"]*) die "hive address must not contain spaces or quotes" ;; esac
if [ -n "$COUNTRY" ]; then
	printf '%s\n' "$COUNTRY" | grep -q '^[A-Z][A-Z]$' || die "--wifi-country takes two capital letters, e.g. DE"
fi
if [ -n "$PSK" ] && [ -z "$SSID" ]; then die "--wifi-psk needs --wifi-ssid"; fi
if [ -n "$PSK" ] && { [ "${#PSK}" -lt 8 ] || [ "${#PSK}" -gt 63 ]; }; then
	die "WPA2 passphrases are 8 to 63 characters"
fi

# conf_value V: savior.conf value syntax (quoted when it has leading/trailing
# blanks or quotes; \ and " escaped inside quotes).
conf_value() {
	case "$1" in
	" "*|*" "|"	"*|*"	"|\"*|*\")
		printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')" ;;
	*) printf '%s' "$1" ;;
	esac
}

write_conf() {
	cat <<EOF
# SaviorOS savior.conf - made by scripts/new-swarm.sh on $(date -u +%Y-%m-%d).
# Copy this file to the root of the SAVIOR partition of every stick (any OS
# can edit it). It contains the swarm key: anyone who has it can join the
# swarm, so keep it private. See "savior config sample" for every key.

swarm_key = $(conf_value "$KEY")
EOF
	if [ -n "$FP" ]; then
		echo "hive_fingerprint = $FP"
	else
		cat <<'EOF'
# Pin the hive's certificate once the hive runs (it shows the fingerprint on
# its screen; "savior ctl node-config" writes it for you):
#hive_fingerprint = sha256:...
EOF
	fi
	if [ -n "$HIVE" ]; then
		echo "hive = $HIVE"
	else
		echo "# The hive is found automatically on the local network (hive = auto)."
	fi
	if [ -n "$SSID" ]; then
		echo ""
		echo "wifi_ssid = $(conf_value "$SSID")"
		[ -z "$PSK" ] || echo "wifi_psk = $(conf_value "$PSK")"
		[ -z "$COUNTRY" ] || echo "wifi_country = $COUNTRY"
	fi
	cat <<'EOF'

# Optional: what this machine does and how much of it the swarm may use.
#roles = auto
#max_cpu_percent = 100
#ssh_key = ssh-ed25519 AAAA... you@laptop
EOF
}

if [ "$OUT" = - ]; then
	write_conf
	exit 0
fi
if [ -e "$OUT" ] && [ "$FORCE" != yes ]; then
	die "$OUT exists; use --force to overwrite it"
fi
umask 077
tmp="$OUT.tmp.$$"
trap 'rm -f "$tmp"' EXIT INT TERM
write_conf >"$tmp"
chmod 0600 "$tmp"
mv -f "$tmp" "$OUT"
trap - EXIT INT TERM
echo "new-swarm: wrote $OUT (mode 0600, contains the swarm key). Copy it to the SAVIOR partition of each stick." >&2
