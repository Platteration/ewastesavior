#!/bin/sh
# lint-sh.sh - run shellcheck (POSIX sh dialect) on every shell script in os/
# and scripts/: *.sh files plus extensionless files whose first line is a sh
# shebang or that carry a "# shellcheck shell=sh" directive (init scripts,
# /usr/libexec/savior helpers, lib.sh).
#
# Usage: scripts/lint-sh.sh [--list] [shellcheck options...]
#   --list   print the files that would be checked and exit
# Environment: SHELLCHECK (default: shellcheck).
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
SHELLCHECK=${SHELLCHECK:-shellcheck}
LIST=no
case "${1:-}" in
--list) LIST=yes; shift ;;
-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
esac

cd "$ROOT"
files=$(mktemp "${TMPDIR:-/tmp}/lint-sh.XXXXXX")
trap 'rm -f "$files"' EXIT INT TERM

for top in os scripts; do
	[ -d "$top" ] || continue
	find "$top" -type f ! -path '*/.git/*' -print
done | LC_ALL=C sort | while IFS= read -r f; do
	case "$f" in
	*.sh) echo "$f"; continue ;;
	*.go|*.md|*.txt|*.conf|*.config|*.fragment|*.list|*.desc|*.mk|*.in|*.hash|*_defconfig|*.cfg|*.template) continue ;;
	esac
	first=$(head -n 1 "$f" 2>/dev/null | tr -d '\r') || continue
	case "$first" in
	'#!/bin/sh'*|'#!/usr/bin/env sh'*|'#!/bin/ash'*|'#!/bin/busybox sh'*|'# shellcheck shell=sh'*) echo "$f" ;;
	*)
		# A directive may follow a comment header (e.g. sourced libraries).
		if head -n 5 "$f" 2>/dev/null | grep -q '^# shellcheck shell=sh'; then echo "$f"; fi
		;;
	esac
done >"$files"

if [ "$LIST" = yes ]; then
	cat "$files"
	exit 0
fi
n=$(wc -l <"$files" | tr -d ' ')
[ "$n" -gt 0 ] || { echo "lint-sh: no shell scripts found" >&2; exit 1; }
command -v "$SHELLCHECK" >/dev/null 2>&1 || { echo "lint-sh: $SHELLCHECK not found (apt install shellcheck)" >&2; exit 1; }
echo "lint-sh: checking $n scripts with $("$SHELLCHECK" --version | sed -n 's/^version: //p')"
# -x follows ". file" includes; SCRIPTDIR resolves "# shellcheck source="
# paths relative to each script.
# shellcheck disable=SC2046 # one file per line, no spaces in repository paths
"$SHELLCHECK" -s sh -x -P SCRIPTDIR "$@" $(cat "$files")
echo "lint-sh: ok"
