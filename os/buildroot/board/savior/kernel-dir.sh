# shellcheck shell=sh
# kernel-dir.sh - sourced by post-build.sh and post-image.sh.
#
# savior_kernel_dir: print the build directory of the kernel this Buildroot
# configuration builds, $BUILD_DIR/linux-<BR2_LINUX_KERNEL_VERSION> (that is
# Buildroot's LINUX_DIR). Never pick one by globbing when the configuration
# names it: after a kernel bump Buildroot keeps the old build/linux-<old>
# next to the new one, and a glob finds the old one first. Without
# BR2_CONFIG (or the symbol in it) it prints the only build/linux-*
# directory that has a .config, and fails when there are several.
# Needs BUILD_DIR; reads BR2_CONFIG.
savior_kernel_dir() {
	_kv=""
	if [ -n "${BR2_CONFIG:-}" ] && [ -f "$BR2_CONFIG" ]; then
		_kv=$(sed -n 's/^BR2_LINUX_KERNEL_VERSION="\(.*\)"$/\1/p' "$BR2_CONFIG" | head -n 1)
		[ -n "$_kv" ] ||
			_kv=$(sed -n 's/^BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE="\(.*\)"$/\1/p' "$BR2_CONFIG" | head -n 1)
	fi
	if [ -n "$_kv" ]; then
		echo "$BUILD_DIR/linux-$_kv"
		return 0
	fi
	_found=""
	for _d in "$BUILD_DIR"/linux-*/; do
		case "$(basename "$_d")" in linux-headers-* | linux-firmware-* | linux-tools-*) continue ;; esac
		[ -f "$_d/.config" ] || continue
		if [ -n "$_found" ]; then
			echo "several kernel build directories in $BUILD_DIR and no BR2_LINUX_KERNEL_VERSION in BR2_CONFIG" >&2
			return 1
		fi
		_found=${_d%/}
	done
	if [ -z "$_found" ]; then
		echo "no kernel build directory (linux-<version> with a .config) in $BUILD_DIR" >&2
		return 1
	fi
	echo "$_found"
}
