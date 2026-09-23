#!/bin/sh
# fetch-buildroot.sh - download, verify and unpack a pinned Buildroot release.
#
# Usage:
#   scripts/fetch-buildroot.sh --version V --dest DIR [--dl-dir DIR] [--hash-file FILE]
#
# Verification, strongest available first:
#   1. the SHA-256 pinned in os/buildroot/buildroot.hash ("sha256  HEX  buildroot-V.tar.xz");
#      a mismatch is fatal;
#   2. otherwise the SHA-256 in the release's signed .sign manifest from
#      buildroot.org (checked with gpg too when BUILDROOT_GPG=1 and the
#      Buildroot release key is in the keyring). This protects against a
#      corrupted or truncated download, not against a compromised server,
#      so it is refused when BUILDROOT_REQUIRE_PIN=1 (release builds).
# The computed hash is printed so it can be pinned.
#
# The tarball is kept in --dl-dir (default build/br-cache/dl); DEST is replaced
# atomically and marked with the verified hash, so re-runs are no-ops.
set -eu

die() { echo "fetch-buildroot: error: $*" >&2; exit 1; }
log() { echo "fetch-buildroot: $*" >&2; }

HERE=$(cd "$(dirname "$0")" && pwd)
VERSION=""
DEST=""
DL_DIR="$HERE/../build/br-cache/dl"
HASH_FILE="$HERE/../os/buildroot/buildroot.hash"
MIRRORS=${BUILDROOT_MIRRORS:-"https://buildroot.org/downloads https://buildroot.net/downloads"}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION=$2; shift 2 ;;
	--dest) DEST=$2; shift 2 ;;
	--dl-dir) DL_DIR=$2; shift 2 ;;
	--hash-file) HASH_FILE=$2; shift 2 ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done
if [ -z "$VERSION" ] || [ -z "$DEST" ]; then
	die "usage: fetch-buildroot.sh --version V --dest DIR [--dl-dir DIR]"
fi
case "$VERSION" in *[!0-9.a-z-]*) die "odd version string: $VERSION" ;; esac

TARBALL="buildroot-$VERSION.tar.xz"
mkdir -p "$DL_DIR"
DL_DIR=$(cd "$DL_DIR" && pwd)

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

fetch() { # fetch URL OUT
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget"
	fi
}

fetch_any() { # fetch_any NAME OUT: try each mirror
	for m in $MIRRORS; do
		if fetch "$m/$1" "$2.part" 2>/dev/null; then
			mv "$2.part" "$2"
			return 0
		fi
		rm -f "$2.part"
	done
	return 1
}

pinned=""
if [ -f "$HASH_FILE" ]; then
	pinned=$(awk -v f="$TARBALL" '$1 == "sha256" && $3 == f { print $2 }' "$HASH_FILE")
fi

# Already unpacked and verified against the same expectation?
if [ -f "$DEST/Makefile" ] && [ -f "$DEST/.savior-verified" ]; then
	have=$(cat "$DEST/.savior-verified")
	if [ -z "$pinned" ] || [ "$have" = "$pinned" ]; then
		log "Buildroot $VERSION already in $DEST"
		exit 0
	fi
fi

if [ ! -f "$DL_DIR/$TARBALL" ]; then
	log "downloading $TARBALL"
	fetch_any "$TARBALL" "$DL_DIR/$TARBALL" || die "cannot download $TARBALL from: $MIRRORS"
fi
actual=$(sha256 "$DL_DIR/$TARBALL")

if [ -n "$pinned" ]; then
	if [ "$actual" != "$pinned" ]; then
		rm -f "$DL_DIR/$TARBALL"
		die "$TARBALL: SHA-256 $actual does not match the pinned $pinned (download deleted)"
	fi
	log "$TARBALL matches the pinned SHA-256"
else
	[ "${BUILDROOT_REQUIRE_PIN:-0}" != 1 ] ||
		die "no pinned SHA-256 for $TARBALL in $HASH_FILE (BUILDROOT_REQUIRE_PIN=1); add: sha256  $actual  $TARBALL"
	sign="$DL_DIR/$TARBALL.sign"
	[ -f "$sign" ] || fetch_any "$TARBALL.sign" "$sign" || die "cannot download $TARBALL.sign to verify the download"
	listed=$(grep -F "$TARBALL" "$sign" | grep -i 'sha256' | grep -o '[0-9a-f]\{64\}' | head -n 1 || true)
	[ -n "$listed" ] || die "$TARBALL.sign lists no SHA-256 for $TARBALL; verify by hand and pin it in $HASH_FILE"
	if [ "$actual" != "$listed" ]; then
		rm -f "$DL_DIR/$TARBALL" "$sign"
		die "$TARBALL: SHA-256 $actual does not match $TARBALL.sign ($listed); download deleted"
	fi
	if [ "${BUILDROOT_GPG:-0}" = 1 ]; then
		command -v gpg >/dev/null 2>&1 || die "BUILDROOT_GPG=1 but gpg is not installed"
		gpg --verify "$sign" 2>/dev/null || die "gpg could not verify $TARBALL.sign (import the Buildroot release key)"
		log "$TARBALL.sign has a good signature"
	fi
	log "$TARBALL matches its .sign manifest (not pinned). To pin it, add to $HASH_FILE:"
	log "  sha256  $actual  $TARBALL"
fi

parent=$(dirname "$DEST")
mkdir -p "$parent"
tmp=$(mktemp -d "$parent/.buildroot-unpack.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM
tar -xJf "$DL_DIR/$TARBALL" -C "$tmp"
[ -f "$tmp/buildroot-$VERSION/Makefile" ] || die "$TARBALL did not contain buildroot-$VERSION/"
printf '%s\n' "$actual" >"$tmp/buildroot-$VERSION/.savior-verified"
rm -rf "$DEST"
mv "$tmp/buildroot-$VERSION" "$DEST"
log "Buildroot $VERSION unpacked in $DEST"
