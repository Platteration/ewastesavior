#!/bin/sh
# check-kconfig.sh - check a built kernel .config against SaviorOS's required
# kernel symbols (os/buildroot/board/savior/linux/required.txt, DESIGN 14).
#
# Usage:
#   scripts/check-kconfig.sh [--arch x86_64|i686] [--required FILE] [--quiet] KERNEL_CONFIG
#
# --arch defaults to x86_64 when the config has CONFIG_64BIT=y, else i686.
# required.txt syntax (see that file): [all]/[x86_64]/[i686] sections and
#   CONFIG_X=y | =m | =y|m | ="text" | =123 | "# CONFIG_X is not set".
# A "not set" requirement also accepts a symbol that is absent from the
# config (Kconfig omits symbols whose dependencies are unmet).
#
# Exit status: 0 all requirements met, 1 some failed, 2 usage or syntax error.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REQUIRED="$HERE/../os/buildroot/board/savior/linux/required.txt"
ARCH=""
QUIET=no
CONFIG=""

usage() { awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; }
die() { echo "check-kconfig: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--arch) [ $# -ge 2 ] || die "--arch needs a value"; ARCH=$2; shift 2 ;;
	--required) [ $# -ge 2 ] || die "--required needs a value"; REQUIRED=$2; shift 2 ;;
	--quiet|-q) QUIET=yes; shift ;;
	-h|--help) usage; exit 0 ;;
	-*) die "unknown option: $1" ;;
	*) [ -z "$CONFIG" ] || die "only one kernel config may be given"; CONFIG=$1; shift ;;
	esac
done
[ -n "$CONFIG" ] || { usage >&2; exit 2; }
[ -f "$CONFIG" ] || die "kernel config not found: $CONFIG"
[ -f "$REQUIRED" ] || die "required list not found: $REQUIRED"

if [ -z "$ARCH" ]; then
	if grep -q '^CONFIG_64BIT=y$' "$CONFIG"; then ARCH=x86_64; else ARCH=i686; fi
fi
case "$ARCH" in x86_64|i686) ;; *) die "--arch must be x86_64 or i686, got $ARCH" ;; esac

# Pass 1 reads the kernel config, pass 2 the requirements.
LC_ALL=C awk -v arch="$ARCH" -v quiet="$QUIET" -v reqfile="$REQUIRED" '
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
FNR == NR {
	line = $0; sub(/\r$/, "", line)
	if (match(line, /^CONFIG_[A-Za-z0-9_]+=/)) {
		sym = substr(line, 1, RLENGTH - 1)
		val[sym] = substr(line, RLENGTH + 1)
	} else if (match(line, /^# CONFIG_[A-Za-z0-9_]+ is not set$/)) {
		sym = substr(line, 3); sub(/ is not set$/, "", sym)
		val[sym] = "n"
	}
	next
}
{
	line = trim($0)
	if (line == "") next
	if (line ~ /^\[[A-Za-z0-9_]+\]$/) {
		section = substr(line, 2, length(line) - 2)
		if (section != "all" && section != "x86_64" && section != "i686") {
			printf "%s:%d: unknown section [%s]\n", reqfile, FNR, section > "/dev/stderr"
			syntax = 1
		}
		next
	}
	if (line ~ /^# CONFIG_[A-Za-z0-9_]+ is not set$/) {
		sym = substr(line, 3); sub(/ is not set$/, "", sym); want = "n"
	} else if (line ~ /^#/) {
		next
	} else if (match(line, /^CONFIG_[A-Za-z0-9_]+=/)) {
		sym = substr(line, 1, RLENGTH - 1); want = substr(line, RLENGTH + 1)
		if (want == "") {
			printf "%s:%d: empty value for %s\n", reqfile, FNR, sym > "/dev/stderr"
			syntax = 1; next
		}
	} else {
		printf "%s:%d: cannot parse: %s\n", reqfile, FNR, line > "/dev/stderr"
		syntax = 1; next
	}
	if (section == "") {
		printf "%s:%d: requirement before any [section]\n", reqfile, FNR > "/dev/stderr"
		syntax = 1; next
	}
	if (section != "all" && section != arch) next
	checked++
	have = (sym in val) ? val[sym] : ""
	ok = 0
	if (want == "n") {
		ok = (have == "" || have == "n")
	} else {
		n = split(want, alt, "|")
		for (i = 1; i <= n; i++) if (have == alt[i]) ok = 1
	}
	if (ok) {
		if (quiet != "yes") printf "ok   %s=%s\n", sym, (have == "" ? "n" : have)
	} else {
		failed++
		printf "FAIL %s: want %s, got %s\n", sym, (want == "n" ? "not set" : want), (have == "" ? "not set (absent)" : (have == "n" ? "not set" : have))
	}
}
END {
	if (syntax) exit 2
	printf "check-kconfig: %s: %d requirements checked for %s, %d failed\n", ARGV[1], checked, arch, failed + 0
	exit failed ? 1 : 0
}' "$CONFIG" "$REQUIRED"
