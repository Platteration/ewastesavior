#!/bin/sh
# check-defconfig.sh - verify that every BR2_ symbol a Buildroot defconfig sets
# survives Kconfig resolution unchanged.
#
# Kconfig silently drops symbols that don't exist in the Buildroot release (a
# typo, a renamed option) or whose dependencies are unmet; the image would
# then quietly lack a package or feature. This script catches that.
#
# Usage:
#   scripts/check-defconfig.sh --br BUILDROOT_SRC --out O_DIR [--strict] DEFCONFIG
#       runs "make olddefconfig" in O_DIR first (the defconfig must already
#       have been loaded there, e.g. make O=O_DIR savior_x86_64_defconfig)
#   scripts/check-defconfig.sh --config DOT_CONFIG [--strict] DEFCONFIG
#       only compares (no Buildroot needed; used by tests)
#
# Rules: "BR2_X=value" (y, strings, numbers) must appear verbatim in .config.
# "# BR2_X is not set" must not be enabled; if the symbol is absent from
# .config (unknown, or its dependencies are unmet) that is a warning, or an
# error with --strict.
#
# Exit status: 0 ok, 1 mismatches, 2 usage error.
set -eu

BR=""
OUT=""
CONFIG=""
STRICT=no
DEFCONFIG=""

die() { echo "check-defconfig: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--br) [ $# -ge 2 ] || die "--br needs a value"; BR=$2; shift 2 ;;
	--out) [ $# -ge 2 ] || die "--out needs a value"; OUT=$2; shift 2 ;;
	--config) [ $# -ge 2 ] || die "--config needs a value"; CONFIG=$2; shift 2 ;;
	--strict) STRICT=yes; shift ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	-*) die "unknown option: $1" ;;
	*) [ -z "$DEFCONFIG" ] || die "only one defconfig may be given"; DEFCONFIG=$1; shift ;;
	esac
done
[ -n "$DEFCONFIG" ] || die "usage: check-defconfig.sh (--br DIR --out DIR | --config FILE) [--strict] DEFCONFIG"
[ -f "$DEFCONFIG" ] || die "defconfig not found: $DEFCONFIG"

if [ -z "$CONFIG" ]; then
	[ -n "$OUT" ] || die "need --out O_DIR (or --config FILE)"
	CONFIG="$OUT/.config"
	if [ -n "$BR" ]; then
		[ -f "$BR/Makefile" ] || die "not a Buildroot tree: $BR"
		[ -f "$CONFIG" ] || die "$CONFIG missing: load the defconfig first"
		# Run the Buildroot make with a clean MAKEFLAGS and no ARCH:
		# variables given on an outer make command line (make image
		# ARCH=...) are exported to us and must not leak into Buildroot.
		o=$(cd "$OUT" && pwd)
		(
			unset ARCH MAKEFLAGS MFLAGS MAKELEVEL MAKEOVERRIDES
			make -s -C "$BR" O="$o" olddefconfig >/dev/null
		) || die "make olddefconfig failed"
	fi
fi
[ -f "$CONFIG" ] || die "config not found: $CONFIG"

LC_ALL=C awk -v strict="$STRICT" -v defconfig="$DEFCONFIG" '
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
FNR == NR {
	line = $0; sub(/\r$/, "", line)
	if (match(line, /^BR2_[A-Za-z0-9_]+=/)) {
		val[substr(line, 1, RLENGTH - 1)] = substr(line, RLENGTH + 1)
	} else if (match(line, /^# BR2_[A-Za-z0-9_]+ is not set$/)) {
		sym = substr(line, 3); sub(/ is not set$/, "", sym); val[sym] = "n"
	}
	next
}
{
	line = trim($0)
	if (line == "") next
	if (match(line, /^# BR2_[A-Za-z0-9_]+ is not set$/)) {
		sym = substr(line, 3); sub(/ is not set$/, "", sym); want = "n"
	} else if (line ~ /^#/) {
		next
	} else if (match(line, /^BR2_[A-Za-z0-9_]+=/)) {
		sym = substr(line, 1, RLENGTH - 1); want = substr(line, RLENGTH + 1)
	} else {
		printf "%s:%d: cannot parse: %s\n", defconfig, FNR, line > "/dev/stderr"
		bad++; next
	}
	if (sym in seen) {
		printf "%s:%d: %s set twice\n", defconfig, FNR, sym > "/dev/stderr"
		bad++
	}
	seen[sym] = 1
	checked++
	if (!(sym in val)) {
		if (want == "n") {
			printf "%s %s: not in .config (unknown symbol or unmet dependencies); effectively off\n", (strict == "yes" ? "FAIL" : "warn"), sym
			if (strict == "yes") bad++
		} else {
			printf "FAIL %s: want %s, but the symbol is not in .config (unknown in this Buildroot release, or its dependencies are unmet)\n", sym, want
			bad++
		}
		next
	}
	if (want == "n") {
		if (val[sym] != "n") { printf "FAIL %s: want not set, got %s\n", sym, val[sym]; bad++ }
	} else if (val[sym] != want) {
		printf "FAIL %s: want %s, got %s\n", sym, want, (val[sym] == "n" ? "not set" : val[sym])
		bad++
	}
}
END {
	printf "check-defconfig: %s: %d symbols checked, %d problems\n", defconfig, checked, bad + 0
	exit bad ? 1 : 0
}' "$CONFIG" "$DEFCONFIG"
