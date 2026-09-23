################################################################################
#
# savior-firmware
#
################################################################################

# The same upstream tarball (and download directory) as Buildroot's
# linux-firmware package, so the version follows the pinned Buildroot release
# and the download is shared. Instead of per-driver Kconfig options, the
# allowlist in board/savior/firmware.list selects the files.
SAVIOR_FIRMWARE_VERSION = $(LINUX_FIRMWARE_VERSION)
SAVIOR_FIRMWARE_SOURCE = $(LINUX_FIRMWARE_SOURCE)
SAVIOR_FIRMWARE_SITE = $(LINUX_FIRMWARE_SITE)
ifneq ($(LINUX_FIRMWARE_SITE_METHOD),)
SAVIOR_FIRMWARE_SITE_METHOD = $(LINUX_FIRMWARE_SITE_METHOD)
endif
SAVIOR_FIRMWARE_DL_SUBDIR = linux-firmware
SAVIOR_FIRMWARE_LICENSE = Redistributable firmware (see WHENCE)
SAVIOR_FIRMWARE_LICENSE_FILES = WHENCE
SAVIOR_FIRMWARE_LIST = $(BR2_EXTERNAL_SAVIOR_PATH)/board/savior/firmware.list

define SAVIOR_FIRMWARE_INSTALL_TARGET_CMDS
	sh $(BR2_EXTERNAL_SAVIOR_PATH)/board/savior/install-firmware.sh \
		$(@D) $(TARGET_DIR)/lib/firmware $(SAVIOR_FIRMWARE_LIST)
endef

$(eval $(generic-package))
