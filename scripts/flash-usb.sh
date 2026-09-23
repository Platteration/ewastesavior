#!/bin/sh
# flash-usb.sh - write a SaviorOS image to a USB stick, carefully.
#
# Usage:
#   sudo scripts/flash-usb.sh [options] IMAGE DEVICE
#   scripts/flash-usb.sh --list
#
#   IMAGE      savior.img or savior.iso (Linux also takes .img.xz / .img.gz)
#   DEVICE     the whole disk: /dev/sdX, /dev/mmcblkN (Linux), /dev/diskN (macOS)
#   --list     show the removable disks and exit
#   --unmount  unmount the stick's mounted partitions first (default: refuse)
#   --force    allow a disk that does not report itself as removable/USB
#              (the disk holding the running system, and virtual or stacked
#              devices such as /dev/mapper/*, /dev/dm-*, /dev/md*, are
#              refused regardless)
#   --yes      don't ask for confirmation (for scripts; the checks still apply)
#
# Safety: refuses partitions, virtual and stacked block devices (loop,
# device-mapper/LVM/LUKS, md RAID, ...), disks holding the running system
# (/, /boot, swap, the APFS/boot container on macOS), mounted disks (unless
# --unmount), non-removable disks (unless --force) and disks smaller than
# the image. It shows the disk's model and size and asks you to type its
# name back. Afterwards edit savior.conf on the stick's SAVIOR partition
# (any OS).
set -eu

# Test hook for scripts/test-scripts.sh, not for use: with
# FLASH_USB_TEST_ROOT=DIR the Linux checks read DIR/sys and DIR/proc instead
# of /sys and /proc, skip the root and block-device checks, and the script
# stops after the checks. Nothing is unmounted or written in that mode.
R=${FLASH_USB_TEST_ROOT:-}

die() { echo "flash-usb: $*" >&2; exit 1; }
say() { echo "flash-usb: $*" >&2; }

FORCE=no
YES=no
UNMOUNT=no
LIST=no
IMAGE=""
DEVICE=""
while [ $# -gt 0 ]; do
	case "$1" in
	--force) FORCE=yes; shift ;;
	--yes|-y) YES=yes; shift ;;
	--unmount) UNMOUNT=yes; shift ;;
	--list) LIST=yes; shift ;;
	-h|--help) awk 'NR > 1 { if (/^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
	-*) die "unknown option: $1 (see --help)" ;;
	*)
		if [ -z "$IMAGE" ]; then IMAGE=$1
		elif [ -z "$DEVICE" ]; then DEVICE=$1
		else die "too many arguments"; fi
		shift
		;;
	esac
done

OS=$(uname -s)
[ -z "$R" ] || OS=Linux
human() { awk -v b="$1" 'BEGIN { split("B KB MB GB TB", u, " "); i = 1; while (b >= 1000 && i < 5) { b /= 1000; i++ } printf (i == 1 ? "%d %s" : "%.1f %s"), b, u[i] }'; }

# ---------------------------------------------------------------------------
# Linux

# disk_of NAME: the whole-disk names under block device NAME (partitions map
# to their disk; device-mapper/md devices to the disks under them).
disk_of() {
	_n=$1
	[ -e "$R/sys/class/block/$_n" ] || return 0
	if [ -e "$R/sys/class/block/$_n/partition" ]; then
		basename "$(dirname "$(readlink -f "$R/sys/class/block/$_n")")"
		return 0
	fi
	_s=""
	for _slave in "$R"/sys/class/block/"$_n"/slaves/*; do
		[ -e "$_slave" ] || continue
		_s=yes
		disk_of "$(basename "$_slave")"
	done
	[ -n "$_s" ] || echo "$_n"
}

# mounted_disks: whole disks behind mounted filesystems and active swap.
linux_used_disks() {
	{
		awk '$1 ~ /^\/dev\// { print $1 }' "$R/proc/mounts"
		awk 'NR > 1 && $1 ~ /^\/dev\// { print $1 }' "$R/proc/swaps" 2>/dev/null
	} | while IFS= read -r dev; do
		real=$(readlink -f "$dev" 2>/dev/null || echo "$dev")
		disk_of "$(basename "$real")"
	done | sort -u
}

linux_system_disks() {
	for mp in / /boot /boot/efi /usr /var; do
		src=$(awk -v m="$mp" '$2 == m && $1 ~ /^\/dev\// { print $1 }' "$R/proc/mounts" | tail -n 1)
		[ -n "$src" ] || continue
		disk_of "$(basename "$(readlink -f "$src")")"
	done
	awk 'NR > 1 && $1 ~ /^\/dev\// { print $1 }' "$R/proc/swaps" 2>/dev/null | while IFS= read -r dev; do
		disk_of "$(basename "$(readlink -f "$dev")")"
	done
}

linux_is_removable() {
	[ "$(sysread "$R/sys/block/$1/removable")" = 1 ] && return 0
	case "$(readlink -f "$R/sys/block/$1")" in */usb*) return 0 ;; esac
	case "$1" in mmcblk*) [ "$(sysread "$R/sys/block/$1/device/type")" = SD ] && return 0 ;; esac
	return 1
}

# sysread FILE: its content, or nothing when it doesn't exist.
sysread() { if [ -r "$1" ]; then tr -s ' ' <"$1"; fi; }

linux_describe() {
	vendor=$(sysread "$R/sys/block/$1/device/vendor")
	model=$(sysread "$R/sys/block/$1/device/model")
	[ -n "$model" ] || model=$(sysread "$R/sys/block/$1/device/name")
	[ -n "$model" ] || model="unknown model"
	bytes=$(( $(cat "$R/sys/block/$1/size") * 512 ))
	echo "$(echo "$vendor $model" | sed 's/^ *//; s/ *$//') ($(human "$bytes"))"
}

# is_virtual NAME: a virtual or stacked block device (never a USB stick):
# loop, RAM disks, CD-ROMs, device-mapper (LVM, LUKS), md RAID, nbd, bcache,
# DRBD, Ceph RBD, ZFS zvols, or anything built on other block devices.
is_virtual() {
	case "$1" in loop*|ram*|zram*|dm-*|md*|sr*|nbd*|bcache*|drbd*|rbd*|zd*) return 0 ;; esac
	for _slave in "$R"/sys/block/"$1"/slaves/*; do
		if [ -e "$_slave" ]; then return 0; fi
	done
	return 1
}

linux_list() {
	for d in "$R"/sys/block/*; do
		n=$(basename "$d")
		is_virtual "$n" && continue
		[ "$(cat "$d/size")" -gt 0 ] || continue
		if linux_is_removable "$n"; then kind=removable; else kind="fixed (needs --force)"; fi
		if linux_system_disks | grep -qx "$n"; then kind="SYSTEM DISK (refused)"; fi
		printf '  /dev/%-10s %s  [%s]\n' "$n" "$(linux_describe "$n")" "$kind"
	done
}

# ---------------------------------------------------------------------------
# macOS

mac_info() { diskutil info "$1" 2>/dev/null; }
mac_field() { mac_info "$1" | sed -n "s/^ *$2: *//p" | head -n 1; }
mac_system_disks() {
	# The disk holding /, and the physical stores of its APFS container.
	d=$(mac_field / "Part of Whole")
	[ -z "$d" ] || echo "$d"
	for s in $(diskutil apfs list 2>/dev/null | sed -n 's/.*Physical Store *\(disk[0-9]*\)s[0-9]*.*/\1/p'); do echo "$s"; done
}
mac_list() {
	diskutil list external physical 2>/dev/null | sed -n 's#^\(/dev/disk[0-9]*\).*#\1#p' | while IFS= read -r d; do
		printf '  %-12s %s (%s)\n' "$d" "$(mac_field "$d" "Device / Media Name")" "$(mac_field "$d" "Disk Size" | sed 's/ (.*//')"
	done
}

# ---------------------------------------------------------------------------

if [ "$LIST" = yes ]; then
	echo "Disks:"
	case "$OS" in
	Linux) linux_list ;;
	Darwin) mac_list ;;
	*) die "unsupported OS $OS" ;;
	esac
	exit 0
fi

if [ -z "$IMAGE" ] || [ -z "$DEVICE" ]; then
	die "usage: flash-usb.sh [--force] [--unmount] [--yes] IMAGE DEVICE   (or --list)"
fi
if [ ! -f "$IMAGE" ] || [ ! -r "$IMAGE" ]; then
	die "cannot read image $IMAGE"
fi
case "$IMAGE" in
*.xz) DECOMP="xz -dc"; command -v xz >/dev/null 2>&1 || die "xz not installed" ;;
*.gz) DECOMP="gzip -dc" ;;
*) DECOMP="" ;;
esac
if [ -n "$DECOMP" ]; then
	[ "$OS" = Linux ] || die "decompress the image first on $OS"
	if [ "$DECOMP" = "xz -dc" ]; then
		img_bytes=$(xz --robot --list "$IMAGE" | awk -F'\t' '$1 == "file" { print $5 }')
	else
		img_bytes=$(gzip -l "$IMAGE" | awk 'NR == 2 { print $2 }')
	fi
else
	img_bytes=$(wc -c <"$IMAGE" | tr -d ' ')
	# A SaviorOS .img/.iso starts with an MBR (boot signature 55 aa at 510).
	sig=$(od -An -tx1 -j 510 -N 2 "$IMAGE" | tr -d ' \n')
	[ "$sig" = 55aa ] || say "warning: $IMAGE has no MBR boot signature; is it really a SaviorOS image?"
fi
[ "$(id -u)" = 0 ] || [ -n "$R" ] || die "run as root (sudo) to write $DEVICE"

case "$OS" in
Linux)
	[ -b "$DEVICE" ] || [ -n "$R" ] || die "$DEVICE is not a block device"
	name=$(basename "$(readlink -f "$DEVICE")")
	[ -e "$R/sys/block/$name" ] || {
		[ -e "$R/sys/class/block/$name/partition" ] && die "$DEVICE is a partition; give the whole disk (e.g. /dev/$(disk_of "$name"))"
		die "$DEVICE is not a whole disk"
	}
	# Refused even with --force: the system and mount checks below resolve
	# mounts to the physical disks under device-mapper and md, so they would
	# never match a /dev/mapper/... or /dev/md... target, even a mounted root.
	if is_virtual "$name"; then
		die "$DEVICE ($name) is a virtual or stacked device, not a USB stick or SD card; refusing (even with --force)"
	fi
	if linux_system_disks | grep -qx "$name"; then
		die "$DEVICE holds the running system; refusing (even with --force)"
	fi
	if linux_is_removable "$name"; then kind="removable"; else
		[ "$FORCE" = yes ] || die "$DEVICE does not report itself as removable or USB; use --force if you are sure"
		kind="FIXED DISK (--force)"
	fi
	if linux_used_disks | grep -qx "$name"; then
		[ "$UNMOUNT" = yes ] || die "$DEVICE has mounted partitions (or swap); unmount them or use --unmount"
		[ -z "$R" ] || { echo "flash-usb: test mode: would unmount $DEVICE, nothing done"; exit 0; }
		awk '{ print $1, $2 }' /proc/mounts | while read -r src mp; do
			case "$src" in /dev/*) ;; *) continue ;; esac
			[ "$(disk_of "$(basename "$(readlink -f "$src")")")" = "$name" ] || continue
			say "unmounting $mp"
			umount "$mp" || die "cannot unmount $mp"
		done
		awk 'NR > 1 { print $1 }' /proc/swaps | while read -r sw; do
			[ "$(disk_of "$(basename "$(readlink -f "$sw")")")" = "$name" ] && { swapoff "$sw" || die "cannot swapoff $sw"; }
		done
		! linux_used_disks | grep -qx "$name" || die "$DEVICE is still in use"
	fi
	dev_bytes=$(( $(cat "$R/sys/block/$name/size") * 512 ))
	desc=$(linux_describe "$name")
	OUT="/dev/$name"
	;;
Darwin)
	name=$(basename "$DEVICE" | sed 's/^r//')
	case "$name" in disk[0-9]*) ;; *) die "give a disk like /dev/disk4 (see --list)" ;; esac
	case "$name" in disk*s[0-9]*) die "$DEVICE is a partition; give the whole disk (/dev/$(echo "$name" | sed 's/s[0-9]*$//'))" ;; esac
	mac_info "$name" >/dev/null || die "diskutil does not know $DEVICE"
	if mac_system_disks | grep -qx "$name"; then
		die "$DEVICE holds the running system; refusing (even with --force)"
	fi
	loc=$(mac_field "$name" "Device Location")
	rem=$(mac_field "$name" "Removable Media")
	proto=$(mac_field "$name" "Protocol")
	if [ "$loc" = External ] || [ "$rem" = Removable ] || [ "$proto" = USB ]; then kind="removable"; else
		[ "$FORCE" = yes ] || die "$DEVICE is an internal disk; use --force if you are sure"
		kind="INTERNAL DISK (--force)"
	fi
	if mount | grep -q "^/dev/${name}s[0-9]"; then
		[ "$UNMOUNT" = yes ] || die "$DEVICE has mounted volumes; eject them or use --unmount"
		diskutil unmountDisk "/dev/$name" >/dev/null || die "cannot unmount $DEVICE"
	fi
	dev_bytes=$(mac_info "$name" | sed -n 's/^ *Disk Size:.*(\([0-9]*\) Bytes).*/\1/p' | head -n 1)
	desc="$(mac_field "$name" "Device / Media Name") ($(human "${dev_bytes:-0}"))"
	OUT="/dev/r$name"
	;;
*) die "unsupported OS $OS (use Raspberry Pi Imager, balenaEtcher or Rufus in DD mode)" ;;
esac

[ -n "$img_bytes" ] || die "cannot determine the image size"
if [ -n "${dev_bytes:-}" ] && [ "$dev_bytes" -gt 0 ] && [ "$img_bytes" -gt "$dev_bytes" ]; then
	die "image ($(human "$img_bytes")) is larger than $DEVICE ($(human "$dev_bytes"))"
fi

echo
echo "  image:  $IMAGE ($(human "$img_bytes"))"
echo "  target: $OUT - $desc [$kind]"
echo "  EVERYTHING ON THIS DISK WILL BE ERASED."
echo
if [ -n "$R" ]; then
	echo "flash-usb: test mode: checks passed for $name, nothing written"
	exit 0
fi
if [ "$YES" != yes ]; then
	[ -r /dev/tty ] || die "no terminal to confirm on; use --yes"
	printf 'Type the disk name (%s) to continue: ' "$name" >/dev/tty
	read -r answer </dev/tty || answer=""
	[ "$answer" = "$name" ] || die "not confirmed; nothing written"
fi

say "writing..."
if [ "$OS" = Linux ]; then
	if [ -n "$DECOMP" ]; then
		$DECOMP "$IMAGE" | dd of="$OUT" bs=4M iflag=fullblock conv=fsync status=progress
	else
		dd if="$IMAGE" of="$OUT" bs=4M conv=fsync status=progress
	fi
	sync
	blockdev --rereadpt "$OUT" 2>/dev/null || true
else
	if dd if=/dev/null of=/dev/null count=0 status=progress 2>/dev/null; then
		dd if="$IMAGE" of="$OUT" bs=4m status=progress
	else
		dd if="$IMAGE" of="$OUT" bs=4m
	fi
	sync
	diskutil eject "/dev/$name" >/dev/null 2>&1 || true
fi
say "done. Put your savior.conf (swarm_key = ...) on the stick's SAVIOR partition, then boot the old machine from it."
