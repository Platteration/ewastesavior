#!/bin/sh
# post-fakeroot.sh - Buildroot post-fakeroot script for SaviorOS (DESIGN 6.5, 13.2).
#
# Buildroot runs it as:  post-fakeroot.sh ROOTFS_DIR [ARCH]
# inside fakeroot, on the copy of the target tree it is about to pack into
# rootfs.cpio, after makedevs has applied the device and permission tables
# (BR2_ROOTFS_POST_FAKEROOT_SCRIPT).
#
# Buildroot's busybox package puts "/bin/busybox f 4755 0 0" into that
# permission table whenever BusyBox is one binary (for setuid applets like
# su and passwd). makedevs therefore makes /bin/busybox setuid root after
# post-build.sh has checked the tree, and post-image.sh would reject the
# initrd. SaviorOS has no setuid applets (busybox.fragment turns
# FEATURE_SUID off) and allows no setuid or setgid file at all, so this
# script clears the bit on /bin/busybox and fails on any setuid or setgid
# file that is left. post-image.sh checks the packed initrd again.
set -eu

die() { echo "savior post-fakeroot: ERROR: $*" >&2; exit 1; }

ROOTFS=${1:-}
if [ -z "$ROOTFS" ] || [ ! -d "$ROOTFS" ]; then
	die "usage: post-fakeroot.sh ROOTFS_DIR [ARCH]"
fi
ROOTFS=$(cd "$ROOTFS" && pwd)
case "$ROOTFS" in /|"") die "refusing to work on ROOTFS_DIR=$ROOTFS" ;; esac

if [ -f "$ROOTFS/bin/busybox" ] && [ ! -L "$ROOTFS/bin/busybox" ]; then
	chmod 0755 "$ROOTFS/bin/busybox"
fi
# Same rule as post-image.sh: no entry (file or directory) with the setuid
# or setgid bit.
left=$(find "$ROOTFS" ! -type l \( -perm -4000 -o -perm -2000 \) -print | head -n 5)
[ -z "$left" ] || die "setuid/setgid files in the rootfs (a package's _PERMISSIONS?): $left"
echo "savior post-fakeroot: no setuid/setgid files in $ROOTFS"
