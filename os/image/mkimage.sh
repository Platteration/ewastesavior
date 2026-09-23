#!/bin/sh
# mkimage.sh - assemble SaviorOS boot media from one or two payloads.
#
# Produces (see docs/DESIGN.md section 13.1):
#   <out>/<name>.img      hybrid USB disk image: legacy BIOS + 64-bit UEFI + 32-bit UEFI
#   <out>/<name>.iso      CD/DVD image (BIOS El Torito + UEFI), also dd-able to USB
#   <out>/netboot/        TFTP/HTTP tree for PXE (BIOS and UEFI)
#
# The disk image is an MBR disk with GRUB's boot.img in the MBR, GRUB's
# i386-pc core image in the gap before the first partition, and a single
# FAT32 partition labeled SAVIOR that holds everything else, including the
# user-editable savior.conf and the GRUB network images (/boot/netboot) a
# hive serves with netboot = yes.
#
# Usage:
#   mkimage.sh --out DIR --payload x86_64=VMLINUZ,INITRD [--payload i686=VMLINUZ,INITRD]
#              [--name savior] [--version V] [--formats img,iso,netboot]
#              [--conf FILE] [--grub-lib /usr/lib/grub] [--fat-mb N]
#              [--cmdline "extra kernel args"] [--timeout SECONDS]
#
#   --conf FILE   copied to the stick as savior.conf (for editing) and baked
#                 into boot/savior-conf.cpio (/etc/savior/baked.conf, mode
#                 0600), which every USB/ISO menu entry loads as a second
#                 initrd. The netboot tree never gets it (TFTP is public).
#
# Each medium gets its own grub.cfg: savior.media=UUID=XXXX-XXXX (the FAT
# volume serial) on the stick, savior.media=UUID=<ISO volume UUID> on the
# ISO, savior.media=none on netboot. `savior storage find-media` matches
# these (the ISO also by the build marker file /boot/savior-<id>.id).
#
# Requirements: grub-mkimage + GRUB platform modules (i386-pc, x86_64-efi,
# optionally i386-efi), mkfs.fat, mtools (mcopy, mmd), cpio (--conf), dd,
# grub-mknetdir (netboot images), and xorriso for the ISO. On Debian/Ubuntu:
#   apt install grub-common grub-pc-bin grub-efi-amd64-bin grub-efi-ia32-bin \
#               dosfstools mtools xorriso cpio
# COMMON_MODS is word-split on purpose; single-quoted GRUB variables are literal.
# shellcheck disable=SC2086,SC2016
set -eu

die() { echo "mkimage: error: $*" >&2; exit 1; }
log() { echo "mkimage: $*" >&2; }

OUT=""
NAME="savior"
VERSION="dev"
FORMATS="img,iso,netboot"
CONF=""
GRUB_LIB="${GRUB_LIB:-/usr/lib/grub}"
FAT_MB=""
EXTRA_CMDLINE=""
TIMEOUT=5
PAYLOADS=""

while [ $# -gt 0 ]; do
	case "$1" in
	--out) OUT="$2"; shift 2 ;;
	--name) NAME="$2"; shift 2 ;;
	--version) VERSION="$2"; shift 2 ;;
	--formats) FORMATS="$2"; shift 2 ;;
	--conf) CONF="$2"; shift 2 ;;
	--grub-lib) GRUB_LIB="$2"; shift 2 ;;
	--fat-mb) FAT_MB="$2"; shift 2 ;;
	--cmdline) EXTRA_CMDLINE="$2"; shift 2 ;;
	--timeout) TIMEOUT="$2"; shift 2 ;;
	--payload) PAYLOADS="$PAYLOADS $2"; shift 2 ;;
	-h|--help) awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0" | sed '/^shellcheck /d'; exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done

[ -n "$OUT" ] || die "--out is required"
[ -n "$PAYLOADS" ] || die "at least one --payload ARCH=VMLINUZ,INITRD is required"
case "$TIMEOUT" in ''|*[!0-9]*) die "--timeout must be a number" ;; esac
case "$FAT_MB" in *[!0-9]*) die "--fat-mb must be a number" ;; esac
case "$VERSION" in *[!A-Za-z0-9._+-]*) die "--version may only contain A-Z a-z 0-9 . _ + -" ;; esac
case "$EXTRA_CMDLINE" in *[\"\$\\]*) die "--cmdline must not contain \", \$ or a backslash" ;; esac

need() { command -v "$1" >/dev/null 2>&1 || die "missing tool: $1 ($2)"; }
need grub-mkimage "grub-common"
need mkfs.fat "dosfstools"
need mcopy "mtools"
need mmd "mtools"
[ -d "$GRUB_LIB/i386-pc" ] || die "GRUB i386-pc modules not found in $GRUB_LIB (grub-pc-bin)"
[ -d "$GRUB_LIB/x86_64-efi" ] || die "GRUB x86_64-efi modules not found in $GRUB_LIB (grub-efi-amd64-bin)"
HAVE_IA32=no
[ -d "$GRUB_LIB/i386-efi" ] && HAVE_IA32=yes
[ -z "$CONF" ] || need cpio "cpio"

HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/savior-mkimage.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# A build-unique marker file lets GRUB find *this* boot medium even when other
# SaviorOS disks are attached. Its first 8 hex digits are the FAT volume
# serial, the next 8 the MBR disk signature.
BUILD_ID=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
MARKER="/boot/savior-$BUILD_ID.id"
FAT_SERIAL=$(printf '%s' "$BUILD_ID" | cut -c1-8 | tr a-f A-F | sed 's/^\(....\)/\1-/')
# The ISO volume UUID (what blkid and GRUB report) is its modification date,
# YYYYMMDDhhmmsscc; the hundredths come from the build ID to keep builds in
# the same second apart.
ISO_DATE="$(date -u +%Y%m%d%H%M%S)$(printf '%02d' $((0x$(printf '%s' "$BUILD_ID" | cut -c1-2) % 100)))"
ISO_UUID=$(printf '%s' "$ISO_DATE" |
	sed 's/^\(....\)\(..\)\(..\)\(..\)\(..\)\(..\)\(..\)$/\1-\2-\3-\4-\5-\6-\7/')

STAGE="$WORK/stage"
mkdir -p "$STAGE/boot/grub" "$STAGE/EFI/BOOT"
printf '%s\n' "$VERSION" >"$STAGE$MARKER"

ARCHES=""
for p in $PAYLOADS; do
	arch=${p%%=*}
	files=${p#*=}
	kernel=${files%%,*}
	initrd=${files#*,}
	case "$arch" in x86_64|i686) ;; *) die "payload arch must be x86_64 or i686, got $arch" ;; esac
	case " $ARCHES " in *" $arch "*) die "payload $arch given twice" ;; esac
	[ -f "$kernel" ] || die "kernel not found: $kernel"
	[ -f "$initrd" ] || die "initrd not found: $initrd"
	mkdir -p "$STAGE/boot/$arch"
	cp "$kernel" "$STAGE/boot/$arch/vmlinuz"
	cp "$initrd" "$STAGE/boot/$arch/initrd"
	ARCHES="$ARCHES $arch"
done
ARCHES=${ARCHES# }
has_arch() { case " $ARCHES " in *" $1 "*) return 0 ;; esac; return 1; }

# ---------------------------------------------------------------------------
# Baked-in configuration (DESIGN 5.1 source 3): a tiny newc cpio the kernel
# unpacks over the main initramfs. Directory entries carry the same modes as
# the rootfs, because the kernel applies them to existing directories.

CONF_INITRD=""
if [ -n "$CONF" ]; then
	[ -f "$CONF" ] || die "config not found: $CONF"
	cstage="$WORK/conf-cpio"
	mkdir -p "$cstage/etc/savior"
	chmod 0755 "$cstage/etc" "$cstage/etc/savior"
	tr -d '\r' <"$CONF" >"$cstage/etc/savior/baked.conf"
	chmod 0600 "$cstage/etc/savior/baked.conf"
	(cd "$cstage" && printf '%s\n' etc etc/savior etc/savior/baked.conf |
		cpio -o -H newc -R 0:0 --quiet) >"$STAGE/boot/savior-conf.cpio" ||
		die "cannot create savior-conf.cpio"
	CONF_INITRD=" /boot/savior-conf.cpio"
fi

# ---------------------------------------------------------------------------
# grub.cfg: one per medium (usb, iso, netboot), differing in savior.media=,
# /boot-options.cfg and the baked config.

CMDLINE="consoleblank=0 quiet loglevel=3 console=ttyS0,115200 console=tty0"
VERBOSE="consoleblank=0 loglevel=7 console=ttyS0,115200 console=tty0"
if [ -n "$EXTRA_CMDLINE" ]; then
	CMDLINE="$CMDLINE $EXTRA_CMDLINE"
	VERBOSE="$VERBOSE $EXTRA_CMDLINE"
fi
# BIOS: ask for a VBE mode so a firmware framebuffer (simplefb/simpledrm)
# exists even without a KMS driver. UEFI keeps the GOP mode.
GFXMODES="1024x768x32,1024x768x16,800x600x32,800x600x16,auto"

# entry TITLE ARCH ARGS GFX [CMDVAR]: one menu entry. ARCH is a payload
# directory or '$arch'; GFX is fb (BIOS VBE mode) or text; CMDVAR names the
# GRUB variable holding the base command line.
entry() {
	printf 'menuentry "%s" {\n' "$1"
	case "$4" in
	fb) printf '\tif [ "$grub_platform" = "pc" ]; then\n\t\tset gfxpayload=%s\n\tfi\n' "$GFXMODES" ;;
	text) printf '\tset gfxpayload=text\n' ;;
	esac
	printf '\tlinux /boot/%s/vmlinuz $%s%s $savior_args\n' "$2" "${5:-savior_cmdline}" "${3:+ $3}"
	printf '\tinitrd /boot/%s/initrd%s\n' "$2" "$conf_initrd"
	printf '}\n'
}

# write_grub_cfg MEDIUM
write_grub_cfg() {
	medium=$1
	conf_initrd=$CONF_INITRD
	case "$medium" in
	usb) media="savior.media=UUID=$FAT_SERIAL" ;;
	iso) media="savior.media=UUID=$ISO_UUID" ;;
	netboot) media="savior.media=none"; conf_initrd="" ;;
	*) die "write_grub_cfg: unknown medium $medium" ;;
	esac
	cat <<EOF
# SaviorOS $VERSION boot menu ($medium), generated by os/image/mkimage.sh.
set timeout=$TIMEOUT
set default=0
set savior_cmdline="$CMDLINE $media"
set savior_verbose="$VERBOSE $media"
set savior_args=""
insmod all_video
serial --unit=0 --speed=115200
terminal_input console serial
terminal_output console serial
EOF
	if [ "$medium" != netboot ]; then
		cat <<'EOF'
# Operator settings from any computer, e.g. set savior_args="video=LVDS-1:d"
if [ -f "($root)/boot-options.cfg" ]; then
	source "($root)/boot-options.cfg"
fi
if [ "$root" = "$savior_bootroot" ]; then
	echo "SaviorOS: boot medium ($root) found by boot device"
else
	echo "SaviorOS: boot medium ($root) found by search (boot device: $savior_bootroot)"
fi
EOF
	fi
	echo
	# Payload selection is baked in (no file tests: they fail over TFTP).
	# With both payloads: 64-bit when the CPU has long mode, on every
	# platform (the x86_64 kernel has EFI_MIXED for 32-bit UEFI).
	if has_arch x86_64 && has_arch i686; then
		printf '%s\n' 'if cpuid -l; then' '	set arch=x86_64' 'else' '	set arch=i686' 'fi'
	elif has_arch x86_64; then
		printf '%s\n' 'set arch=x86_64' \
			'if ! cpuid -l; then' \
			'	echo "This CPU is 32-bit only, but this SaviorOS build has only the 64-bit payload."' \
			'	echo "Use the i686 or universal image. Trying anyway in 10 seconds..."' \
			'	sleep 10' \
			'fi'
	else
		echo 'set arch=i686'
	fi
	echo
	entry 'SaviorOS ($arch)' '$arch' "" fb
	entry 'SaviorOS - safe graphics (nomodeset, firmware framebuffer)' '$arch' nomodeset fb
	entry 'SaviorOS - safe mode (no ACPI/APIC)' '$arch' "noapic nolapic acpi=off irqpoll nomodeset" fb
	entry 'SaviorOS - text only' '$arch' "" text
	entry 'SaviorOS - display only' '$arch' savior.roles=display fb
	entry 'SaviorOS - start as hive' '$arch' savior.roles=auto,hive fb
	entry 'SaviorOS - rescue shell on Alt+F2, verbose boot' '$arch' savior.console_shell=yes fb savior_verbose
	if has_arch x86_64 && has_arch i686; then
		entry 'SaviorOS - force 32-bit' i686 "" fb
	fi
	if [ "$medium" = netboot ]; then
		printf '%s\n' 'menuentry "Boot from local disk" { exit }'
	fi
	printf '%s\n' 'menuentry "Reboot" { reboot }' 'menuentry "Power off" { halt }'
}

check_grub_cfg() {
	if command -v grub-script-check >/dev/null 2>&1; then
		grub-script-check "$1" || die "generated $1 failed grub-script-check"
	fi
}

# Early config baked into every core image (DESIGN 13.1). It runs under
# GRUB's rescue parser (plain commands, no "if"), so "use \$root when it holds
# this build's marker, else search" is search --hint: the hinted device (the
# boot partition from the prefix on BIOS disks, the loaded EFI image's
# partition on UEFI) is tried first, then every other device.
cat >"$WORK/early.cfg" <<EOF
set savior_bootroot=\$root
search --no-floppy --file --set=root --hint=\$root $MARKER
export savior_bootroot
set prefix=(\$root)/boot/grub
configfile (\$root)/boot/grub/grub.cfg
EOF

# ---------------------------------------------------------------------------
# savior.conf, README and the firmware folder on the stick

if [ -n "$CONF" ]; then
	cp "$CONF" "$STAGE/savior.conf"
elif [ -f "$HERE/savior.conf.template" ]; then
	cp "$HERE/savior.conf.template" "$STAGE/savior.conf"
else
	printf '# SaviorOS configuration. See README.txt.\n#swarm_key =\n' >"$STAGE/savior.conf"
fi
if [ -f "$HERE/README.txt" ]; then
	sed "s/@VERSION@/$VERSION/g" "$HERE/README.txt" >"$STAGE/README.txt"
fi
mkdir -p "$STAGE/firmware"
cat >"$STAGE/firmware/README.txt" <<'EOF'
Firmware files for Wi-Fi cards and other devices that SaviorOS does not
ship (for example Intel PRO/Wireless 2100/2200: ipw2100-*.fw, ipw2200-*.fw;
Broadcom b43: the b43/ folder). Copy them here keeping the folder layout of
linux-firmware, then reboot. SaviorOS loads firmware from this folder.
EOF
# Windows-friendly line endings for the files people edit on any computer.
for f in "$STAGE/savior.conf" "$STAGE/README.txt" "$STAGE/firmware/README.txt"; do
	[ -f "$f" ] && sed -i 's/\r*$/\r/' "$f"
done

# ---------------------------------------------------------------------------
# GRUB images

COMMON_MODS="part_msdos part_gpt fat iso9660 normal linux echo test configfile cpuid search search_fs_file search_label ls cat sleep reboot halt minicmd serial terminal gfxterm font all_video"
log "building GRUB images"
# BIOS disk: the prefix names partition 1 of the boot drive; GRUB fills in
# the drive the BIOS booted from, so $root is right even with other disks.
grub-mkimage -O i386-pc -d "$GRUB_LIB/i386-pc" -c "$WORK/early.cfg" -p '(,msdos1)/boot/grub' \
	-o "$WORK/core.img" biosdisk $COMMON_MODS
if [ "$(wc -c <"$WORK/core.img")" -gt $((2047 * 512)) ]; then
	die "i386-pc core image too large for the MBR gap"
fi
grub-mkimage -O x86_64-efi -d "$GRUB_LIB/x86_64-efi" -c "$WORK/early.cfg" -p /boot/grub \
	-o "$STAGE/EFI/BOOT/BOOTX64.EFI" $COMMON_MODS efi_gop efi_uga
if [ "$HAVE_IA32" = yes ]; then
	grub-mkimage -O i386-efi -d "$GRUB_LIB/i386-efi" -c "$WORK/early.cfg" -p /boot/grub \
		-o "$STAGE/EFI/BOOT/BOOTIA32.EFI" $COMMON_MODS efi_gop efi_uga
else
	log "warning: no i386-efi GRUB modules; image won't boot on 32-bit UEFI"
fi

# Network boot images (grub-mknetdir layout: boot/grub/<platform>/). They go
# into the netboot tree and onto the stick as /boot/netboot/boot/grub/, where
# a hive with netboot = yes serves them (S65netboot).
NETGRUB=""
if command -v grub-mknetdir >/dev/null 2>&1; then
	NETGRUB="$WORK/netgrub"
	mkdir -p "$NETGRUB"
	for plat in i386-pc x86_64-efi i386-efi; do
		[ -d "$GRUB_LIB/$plat" ] || continue
		grub-mknetdir --net-directory="$NETGRUB" --subdir=boot/grub -d "$GRUB_LIB/$plat" >/dev/null 2>&1 ||
			die "grub-mknetdir failed for $plat"
	done
	mkdir -p "$STAGE/boot/netboot/boot/grub"
	for plat in i386-pc x86_64-efi i386-efi; do
		[ -d "$NETGRUB/boot/grub/$plat" ] && cp -R "$NETGRUB/boot/grub/$plat" "$STAGE/boot/netboot/boot/grub/"
	done
else
	log "warning: grub-mknetdir not found: no netboot tree, and a hive booted from these media cannot serve PXE"
fi

# ---------------------------------------------------------------------------
# Disk image

le32() {
	# Print a 32-bit little-endian integer as raw bytes.
	n=$1
	for _ in 1 2 3 4; do
		# shellcheck disable=SC2059 # the format string is the octal escape
		printf "\\$(printf '%03o' $((n & 255)))"
		n=$((n >> 8))
	done
}

build_img() {
	img="$OUT/$NAME.img"
	write_grub_cfg usb >"$STAGE/boot/grub/grub.cfg"
	check_grub_cfg "$STAGE/boot/grub/grub.cfg"
	content_kb=$(du -sk "$STAGE" | cut -f1)
	fat_mb=$FAT_MB
	if [ -z "$fat_mb" ]; then
		# payload + 25% + 48 MiB free for hive data / logs, rounded up to 8 MiB,
		# at least 64 MiB so FAT32 has enough clusters.
		fat_mb=$(( (content_kb * 5 / 4 / 1024 + 48 + 7) / 8 * 8 ))
		[ "$fat_mb" -lt 64 ] && fat_mb=64
	fi
	[ $((fat_mb * 1024)) -gt $((content_kb + 1024)) ] || die "--fat-mb $fat_mb is too small for ${content_kb} KiB of files"
	part_sectors=$((fat_mb * 2048))
	start=2048
	log "building $img (FAT32 partition ${fat_mb} MiB, serial $FAT_SERIAL)"

	part="$WORK/part.img"
	rm -f "$part"
	dd if=/dev/zero of="$part" bs=1M count=0 seek="$fat_mb" 2>/dev/null
	mkfs.fat -F 32 -n SAVIOR -i "$(printf '%s' "$BUILD_ID" | cut -c1-8)" "$part" >/dev/null
	MTOOLS_SKIP_CHECK=1 mcopy -s -Q -i "$part" "$STAGE"/* ::/

	rm -f "$img"
	dd if=/dev/zero of="$img" bs=512 count=0 seek=$((start + part_sectors)) 2>/dev/null
	# MBR: GRUB boot code (first 440 bytes of boot.img), disk signature,
	# one bootable FAT32-LBA partition, 0x55AA.
	dd if="$GRUB_LIB/i386-pc/boot.img" of="$img" bs=440 count=1 conv=notrunc 2>/dev/null
	le32 $((0x$(printf '%s' "$BUILD_ID" | cut -c9-16))) | dd of="$img" bs=1 seek=440 conv=notrunc 2>/dev/null
	{
		printf '\200'                # status: bootable
		printf '\040\041\000'        # CHS start (LBA 2048 in 64/32 geometry)
		printf '\014'                # type 0x0C: FAT32 LBA
		printf '\376\377\377'        # CHS end: beyond 8 GiB marker
		le32 "$start"
		le32 "$part_sectors"
	} | dd of="$img" bs=1 seek=446 conv=notrunc 2>/dev/null
	printf '\125\252' | dd of="$img" bs=1 seek=510 conv=notrunc 2>/dev/null
	# GRUB core image right after the MBR (sector 1).
	dd if="$WORK/core.img" of="$img" bs=512 seek=1 conv=notrunc 2>/dev/null
	# The FAT32 partition.
	dd if="$part" of="$img" bs=1M seek=1 conv=notrunc 2>/dev/null
	rm -f "$part"
	log "wrote $img ($(du -h "$img" | cut -f1))"
}

build_iso() {
	need xorriso "xorriso"
	iso="$OUT/$NAME.iso"
	log "building $iso (volume UUID $ISO_UUID)"
	isostage="$WORK/iso"
	mkdir -p "$isostage/boot/grub/i386-pc"
	cp -R "$STAGE/boot/." "$isostage/boot/"
	cp -R "$STAGE/EFI" "$isostage/"
	for f in savior.conf README.txt firmware; do
		[ -e "$STAGE/$f" ] && cp -R "$STAGE/$f" "$isostage/"
	done
	write_grub_cfg iso >"$isostage/boot/grub/grub.cfg"
	check_grub_cfg "$isostage/boot/grub/grub.cfg"
	# BIOS: El Torito no-emulation image (cdboot.img + core image).
	grub-mkimage -O i386-pc-eltorito -d "$GRUB_LIB/i386-pc" -c "$WORK/early.cfg" -p /boot/grub \
		-o "$isostage/boot/grub/i386-pc/eltorito.img" biosdisk $COMMON_MODS
	# UEFI: a small FAT image holding the same EFI loaders as the stick.
	efi_kb=$(( $(du -sk "$STAGE/EFI" | cut -f1) + 256 ))
	mkfs.fat -C "$isostage/efi.img" "$efi_kb" >/dev/null
	MTOOLS_SKIP_CHECK=1 mcopy -s -Q -i "$isostage/efi.img" "$STAGE/EFI" ::/
	# Same layout grub-mkrescue produces: hybrid MBR for USB, GPT EFI partition.
	# --modification-date sets the volume UUID that savior.media= names.
	xorriso -as mkisofs -quiet -o "$iso" -V SAVIOR -r -J \
		--modification-date="$ISO_DATE" \
		-b boot/grub/i386-pc/eltorito.img -no-emul-boot -boot-load-size 4 \
		-boot-info-table --grub2-boot-info \
		--grub2-mbr "$GRUB_LIB/i386-pc/boot_hybrid.img" \
		--efi-boot efi.img -efi-boot-part --efi-boot-image \
		--protective-msdos-label \
		"$isostage" >"$WORK/xorriso.log" 2>&1 || { cat "$WORK/xorriso.log" >&2; die "xorriso failed"; }
	rm -rf "$isostage"
	log "wrote $iso ($(du -h "$iso" | cut -f1))"
}

build_netboot() {
	[ -n "$NETGRUB" ] || die "netboot needs grub-mknetdir (grub-common)"
	nb="$OUT/netboot"
	log "building $nb/"
	rm -rf "$nb"
	mkdir -p "$nb/boot"
	cp -R "$NETGRUB/boot/grub" "$nb/boot/"
	# Only the payloads: never the marker, the stick's netboot copy or the
	# baked config (it may hold the swarm key; TFTP has no access control).
	for a in $ARCHES; do
		cp -R "$STAGE/boot/$a" "$nb/boot/"
	done
	# Over the network the root is the TFTP server; paths stay the same.
	write_grub_cfg netboot >"$nb/boot/grub/grub.cfg"
	check_grub_cfg "$nb/boot/grub/grub.cfg"
	cat >"$nb/README.txt" <<EOF
SaviorOS $VERSION netboot tree.
Serve this directory over TFTP (or HTTP). PXE boot files:
  BIOS:        boot/grub/i386-pc/core.0
  UEFI 64-bit: boot/grub/x86_64-efi/core.efi
  UEFI 32-bit: boot/grub/i386-efi/core.efi
Netbooted nodes have no stick to read savior.conf from: put savior.<key>=<value>
settings into savior_cmdline in boot/grub/grub.cfg, ideally
savior.hive=<address> savior.hive_fingerprint=sha256:... savior.join=keyless
(nodes then wait for approval on the hive). A swarm key on the kernel
command line is readable by anyone on the LAN. A hive with netboot = yes
does all of this by itself.
EOF
}

case ",$FORMATS," in *,img,*) build_img ;; esac
case ",$FORMATS," in *,iso,*) build_iso ;; esac
case ",$FORMATS," in *,netboot,*) build_netboot ;; esac
log "done (payloads: $ARCHES; build id $BUILD_ID)"
