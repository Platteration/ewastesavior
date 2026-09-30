#!/bin/sh
# legal-info.sh - the licences and corresponding source a SaviorOS release
# publishes next to its images (README "Licenses", .github/workflows/images.yml).
#
# Usage:
#   scripts/legal-info.sh pack BR_OUTPUT_DIR OUT.tar [--add NAME=FILE]...
#       Pack the legal-info directory that Buildroot's "make legal-info"
#       wrote into BR_OUTPUT_DIR (licence texts, sources and patches of every
#       package in the image, manifest.csv) into OUT.tar as legal-info/...,
#       with each FILE as legal-info/savior/NAME (the kernel and BusyBox
#       configurations), then run "check" on it. `make legal-info ARCH=...`
#       does both steps.
#   scripts/legal-info.sh check OUT.tar
#       Fail unless OUT.tar holds a Buildroot legal-info: manifest.csv,
#       licence texts, and the sources of linux and busybox.
#   scripts/legal-info.sh host-manifest
#       Print "PACKAGE VERSION SOURCE SOURCE-VERSION" for the build host's
#       Debian/Ubuntu packages whose files mkimage.sh puts onto the media:
#       GRUB (grub-pc-bin, grub-efi-amd64-bin, grub-efi-ia32-bin) and iPXE
#       (ipxe, for undionly.kpxe). Buildroot's legal-info does not cover them.
#   scripts/legal-info.sh host-sources OUT.tar [MANIFEST]...
#       Download the source packages named by this host's manifest and by
#       the MANIFEST files (other build jobs' host-manifest output) with
#       "apt-get source --download-only SOURCE=VERSION" (needs deb-src lines
#       in the apt sources), and pack them with the manifests into OUT.tar
#       as host-sources/.... A version the archive no longer has fails.
#
# The archives are plain tar: legal-info/sources holds upstream tarballs that
# are compressed already. Exit status: 0 ok, 1 failure, 2 usage error.
set -eu

die() { echo "legal-info: $*" >&2; exit 1; }
usage() { echo "usage: legal-info.sh pack|check|host-manifest|host-sources ... (see --help)" >&2; exit 2; }

# The host packages whose files end up on the media.
HOST_PACKAGES=${SAVIOR_HOST_PACKAGES:-"grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin ipxe"}

W=""
trap '[ -z "$W" ] || rm -rf "$W"' EXIT
trap 'exit 1' INT TERM
work() { [ -n "$W" ] || W=$(mktemp -d "${TMPDIR:-/tmp}/legal-info.XXXXXX"); }

# abspath FILE: FILE as an absolute path (its directory must exist).
abspath() { printf '%s/%s\n' "$(cd "$(dirname "$1")" && pwd)" "$(basename "$1")"; }

check() {
	[ -f "$1" ] || die "no such file: $1"
	work
	tar -tf "$1" >"$W/list" 2>"$W/tar.err" || die "cannot list $1: $(head -n 1 "$W/tar.err")"
	missing=""
	grep -q -x 'legal-info/manifest.csv' "$W/list" || missing="$missing manifest.csv"
	grep -q '^legal-info/licenses/[^/][^/]*/..*[^/]$' "$W/list" || missing="$missing licenses/"
	for p in linux busybox; do
		grep -q "^legal-info/sources/$p-[0-9][^/]*/..*[^/]\$" "$W/list" || missing="$missing sources/$p-*"
	done
	[ -z "$missing" ] || die "$1 is not a complete Buildroot legal-info; missing:$missing"
	echo "legal-info: $1: $(grep -c '^legal-info/sources/[^/]*/$' "$W/list" || true) source packages," \
		"$(grep -c '^legal-info/licenses/[^/]*/$' "$W/list" || true) licence directories"
}

pack() {
	[ $# -ge 2 ] || usage
	br=$1 out=$2
	shift 2
	[ -d "$br/legal-info" ] || die "$br/legal-info not found (run Buildroot's make legal-info first)"
	work
	mkdir -p "$W/stage/legal-info/savior"
	while [ $# -gt 0 ]; do
		case "$1" in
		--add)
			[ $# -ge 2 ] || usage
			name=${2%%=*} file=${2#*=}
			case "$name" in ""|*/*|.*) die "--add: bad name '$name'" ;; esac
			[ "$name" != "$2" ] || die "--add takes NAME=FILE"
			[ -f "$file" ] || die "--add: no such file: $file"
			cp "$file" "$W/stage/legal-info/savior/$name"
			shift 2
			;;
		*) usage ;;
		esac
	done
	mkdir -p "$(dirname "$out")"
	out=$(abspath "$out")
	rm -f "$out"
	if [ -n "$(ls -A "$W/stage/legal-info/savior")" ]; then
		tar -cf "$out" -C "$br" legal-info -C "$W/stage" legal-info/savior || die "tar failed"
	else
		tar -cf "$out" -C "$br" legal-info || die "tar failed"
	fi
	check "$out"
	echo "legal-info: wrote $out ($(($(wc -c <"$out") / 1048576)) MiB)"
}

host_manifest() {
	command -v dpkg-query >/dev/null 2>&1 || die "dpkg-query not found (a Debian/Ubuntu host is needed)"
	# shellcheck disable=SC2086 # a list of package names
	dpkg-query -W -f='${Package} ${Version} ${source:Package} ${source:Version}\n' $HOST_PACKAGES ||
		die "not installed: one of $HOST_PACKAGES"
}

host_sources() {
	[ $# -ge 1 ] || usage
	out=$1
	shift
	work
	mkdir -p "$W/host-sources"
	host_manifest >"$W/host-sources/host-packages.txt"
	n=0
	for m in "$@"; do
		[ -f "$m" ] || die "no such manifest: $m"
		n=$((n + 1))
		cp "$m" "$W/host-sources/host-packages-$n.txt"
	done
	cat "$W"/host-sources/host-packages*.txt | awk 'NF == 4 { print $3 " " $4 }' | LC_ALL=C sort -u >"$W/wanted"
	[ -s "$W/wanted" ] || die "the manifests name no source packages"
	while read -r src ver; do
		echo "legal-info: apt-get source --download-only $src=$ver" >&2
		(cd "$W/host-sources" && apt-get source --download-only -qq "$src=$ver") >"$W/apt.out" 2>&1 ||
			die "cannot download the source of $src $ver (deb-src lines in the apt sources?): $(tail -n 2 "$W/apt.out" | tr '\n' ' ')"
		# .dsc names leave out the epoch.
		[ -f "$W/host-sources/${src}_${ver#*:}.dsc" ] || die "apt-get source $src=$ver left no ${src}_${ver#*:}.dsc"
	done <"$W/wanted"
	cat >"$W/host-sources/README.txt" <<'EOF'
Corresponding source of the build host packages whose files are on the
SaviorOS media: GRUB (boot images and modules) and iPXE (undionly.kpxe).
host-packages*.txt: PACKAGE VERSION SOURCE SOURCE-VERSION, one file per
build job. Unpack a source package with: dpkg-source -x SOURCE_VERSION.dsc
EOF
	mkdir -p "$(dirname "$out")"
	out=$(abspath "$out")
	rm -f "$out"
	tar -cf "$out" -C "$W" host-sources || die "tar failed"
	echo "legal-info: wrote $out: $(wc -l <"$W/wanted" | tr -d ' ') source packages ($(($(wc -c <"$out") / 1048576)) MiB)"
}

cmd=${1:-}
[ $# -eq 0 ] || shift
case "$cmd" in
pack) pack "$@" ;;
check) [ $# -eq 1 ] || usage; check "$1" ;;
host-manifest) [ $# -eq 0 ] || usage; host_manifest ;;
host-sources) host_sources "$@" ;;
-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0" ;;
*) usage ;;
esac
