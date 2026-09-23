# SaviorOS top-level Makefile (docs/DESIGN.md section 14).
#
# "make" (or "make help") lists the targets. The Go targets work anywhere Go
# 1.24 does (including GNU make 3.81 on macOS); dev-image, dev-test and the
# Buildroot image targets need a Linux host.

GO ?= go
GOFMT ?= gofmt
SHELLCHECK ?= shellcheck
PKG := github.com/platteration/ewastesavior

# Version and build time baked into the binaries and /etc/savior-release.
# BUILD_UNIX is the node's clock floor (DESIGN 4): SOURCE_DATE_EPOCH when set,
# else the last commit's time (reproducible), else now.
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
BUILD_UNIX ?= $(shell echo "$${SOURCE_DATE_EPOCH:-$$(git log -1 --format=%ct 2>/dev/null || date +%s)}")

GOFLAGS ?= -trimpath
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.BuildUnix=$(BUILD_UNIX)
GOBUILD = CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)"

BUILD := build
IMAGES := $(BUILD)/images
HOSTEXE = $(shell $(GO) env GOEXE 2>/dev/null)
CROSS_TARGETS ?= darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# Dev image (Ubuntu kernel + busybox-static, os/dev).
DEV_OUT ?= $(BUILD)/dev

# Buildroot images. ARCH selects the payload: x86_64 or i686.
ARCH ?= x86_64
BUILDROOT_VERSION ?= 2025.02.5
BUILDROOT_SRC ?= $(BUILD)/buildroot-$(BUILDROOT_VERSION)
BR_DL_DIR ?= $(BUILD)/br-cache/dl
BR_OUT = $(BUILD)/br-$(ARCH)
BR_PAYLOAD = $(BR_OUT)/images/payload/$(ARCH)
BIN_DIR_x86_64 = $(BUILD)/linux-amd64
BIN_DIR_i686 = $(BUILD)/linux-386
X86_64_PAYLOAD ?= $(BUILD)/br-x86_64/images/payload/x86_64
I686_PAYLOAD ?= $(BUILD)/br-i686/images/payload/i686
# Buildroot's own make runs with a clean environment: variables from this
# make's command line (ARCH=...) must not leak into it or the kernel build.
BRMAKE ?= make
BR_ENV = env -u ARCH -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEOVERRIDES BR2_DL_DIR=$(abspath $(BR_DL_DIR))
BR_ARGS = -C $(BUILDROOT_SRC) O=$(abspath $(BR_OUT)) BR2_EXTERNAL=$(abspath os/buildroot)

.PHONY: help build build-linux build-linux-amd64 build-linux-386 cross test test-386 test-race \
	vet fmt-check lint-sh test-scripts check dev-image dev-test swarm-test \
	buildroot-src check-arch image universal-image universal-media clean distclean

help:
	@echo "SaviorOS $(VERSION) - make targets:"
	@echo ""
	@echo "  build             host savior binary -> $(BUILD)/savior"
	@echo "  build-linux       node binaries: $(BUILD)/linux-amd64/savior (GOAMD64=v1),"
	@echo "                    $(BUILD)/linux-386/savior-sse2 and savior-softfloat"
	@echo "  cross             hive/ctl builds for $(CROSS_TARGETS) -> $(BUILD)/<os>-<arch>/"
	@echo "  test              go test ./..."
	@echo "  test-race         go test -race ./... (needs cgo; falls back to test)"
	@echo "  test-386          go test ./... as GOARCH=386, sse2 and softfloat"
	@echo "  vet               go vet ./..."
	@echo "  fmt-check         fail if gofmt would change anything"
	@echo "  lint-sh           shellcheck -s sh on every shell script in os/ and scripts/"
	@echo "  test-scripts      self-tests of the build scripts (scripts/test-scripts.sh)"
	@echo "  check             fmt-check vet lint-sh test-scripts test"
	@echo ""
	@echo "  dev-image         dev image from Ubuntu packages -> $(DEV_OUT)/ (os/dev/build.sh)"
	@echo "  dev-test          QEMU boot tests of the dev image (os/dev/qemu-test.sh all)"
	@echo "  swarm-test        QEMU swarm test only (os/dev/qemu-test.sh swarm)"
	@echo ""
	@echo "  image ARCH=x86_64|i686"
	@echo "                    Buildroot $(BUILDROOT_VERSION) image -> $(IMAGES)/<arch>/savior-<arch>.{img,iso} + netboot/"
	@echo "  universal-image   both archs, then one image choosing the payload by CPU"
	@echo "                    -> $(IMAGES)/savior-universal.{img,iso} + $(IMAGES)/netboot/"
	@echo "  universal-media   only the last step, from existing payloads"
	@echo "                    (X86_64_PAYLOAD=dir I686_PAYLOAD=dir)"
	@echo ""
	@echo "  clean             remove binaries, images and the dev image (keeps Buildroot trees)"
	@echo "  distclean         remove all of $(BUILD)/"
	@echo ""
	@echo "Variables: GO=$(GO) VERSION=$(VERSION) BUILD_UNIX=$(BUILD_UNIX) ARCH=$(ARCH)"

# --- Go ----------------------------------------------------------------------

build:
	$(GOBUILD) -o $(BUILD)/savior$(HOSTEXE) ./cmd/savior

build-linux: build-linux-amd64 build-linux-386

build-linux-amd64:
	GOOS=linux GOARCH=amd64 GOAMD64=v1 $(GOBUILD) -o $(BUILD)/linux-amd64/savior ./cmd/savior

build-linux-386:
	GOOS=linux GOARCH=386 GO386=sse2 $(GOBUILD) -o $(BUILD)/linux-386/savior-sse2 ./cmd/savior
	GOOS=linux GOARCH=386 GO386=softfloat $(GOBUILD) -o $(BUILD)/linux-386/savior-softfloat ./cmd/savior

cross:
	@set -e; for t in $(CROSS_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		case $$os in windows) ext=.exe ;; *) ext= ;; esac; \
		echo "GOOS=$$os GOARCH=$$arch -> $(BUILD)/$$os-$$arch/savior$$ext"; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o $(BUILD)/$$os-$$arch/savior$$ext ./cmd/savior; \
	done

test:
	CGO_ENABLED=0 $(GO) test $(GOFLAGS) ./...

# The 32-bit builds run on the oldest machines (softfloat: no SSE2); run the
# tests as they are built. Needs a kernel that runs 386 binaries.
test-386:
	CGO_ENABLED=0 GOARCH=386 GO386=sse2 $(GO) test $(GOFLAGS) ./...
	CGO_ENABLED=0 GOARCH=386 GO386=softfloat $(GO) test $(GOFLAGS) ./...

test-race:
	@if CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/version >/dev/null 2>&1; then \
		echo "CGO_ENABLED=1 $(GO) test -race ./..."; \
		CGO_ENABLED=1 $(GO) test -race $(GOFLAGS) ./...; \
	else \
		echo "test-race: the race detector is unavailable here (needs cgo and a C compiler); running plain tests"; \
		CGO_ENABLED=0 $(GO) test $(GOFLAGS) ./...; \
	fi

vet:
	CGO_ENABLED=0 $(GO) vet ./...

fmt-check:
	@files=$$(find . -path ./$(BUILD) -prune -o -path ./.git -prune -o -name '*.go' -print); \
	out=$$($(GOFMT) -l $$files); \
	if [ -n "$$out" ]; then echo "gofmt -w needed for:"; echo "$$out"; exit 1; fi; \
	echo "gofmt: ok"

# --- scripts -------------------------------------------------------------------

lint-sh:
	SHELLCHECK=$(SHELLCHECK) sh scripts/lint-sh.sh

test-scripts:
	sh scripts/test-scripts.sh

check: fmt-check vet lint-sh test-scripts test

# --- dev image (os/dev) ----------------------------------------------------------

dev-image: build-linux-amd64
	sh os/dev/build.sh --out $(DEV_OUT) --savior $(BUILD)/linux-amd64/savior

dev-test:
	@[ -f $(DEV_OUT)/vmlinuz ] || $(MAKE) dev-image
	sh os/dev/qemu-test.sh all --out $(DEV_OUT)

swarm-test:
	@[ -f $(DEV_OUT)/vmlinuz ] || $(MAKE) dev-image
	sh os/dev/qemu-test.sh swarm --out $(DEV_OUT)

# --- Buildroot images (os/buildroot) -----------------------------------------------

buildroot-src: $(BUILDROOT_SRC)/Makefile

$(BUILDROOT_SRC)/Makefile:
	sh scripts/fetch-buildroot.sh --version $(BUILDROOT_VERSION) --dest $(BUILDROOT_SRC) --dl-dir $(BR_DL_DIR)

check-arch:
	@case "$(ARCH)" in x86_64|i686) ;; \
	*) echo "ARCH must be x86_64 or i686 (got '$(ARCH)')" >&2; exit 1 ;; esac

image: check-arch buildroot-src build-linux
	$(BR_ENV) $(BRMAKE) $(BR_ARGS) savior_$(ARCH)_defconfig
	sh scripts/check-defconfig.sh --br $(BUILDROOT_SRC) --out $(BR_OUT) os/buildroot/configs/savior_$(ARCH)_defconfig
	$(BR_ENV) SAVIOR_BIN_DIR=$(abspath $(BIN_DIR_$(ARCH))) SAVIOR_VERSION=$(VERSION) \
		SAVIOR_BUILD_UNIX=$(BUILD_UNIX) SAVIOR_MKIMAGE=no $(BRMAKE) $(BR_ARGS)
	sh scripts/check-kconfig.sh -q --arch $(ARCH) $(BR_PAYLOAD)/kernel.config
	sh os/image/mkimage.sh --out $(IMAGES)/$(ARCH) --name savior-$(ARCH) --version $(VERSION) \
		--payload $(ARCH)=$(BR_PAYLOAD)/vmlinuz,$(BR_PAYLOAD)/initrd

universal-image:
	$(MAKE) image ARCH=x86_64
	$(MAKE) image ARCH=i686
	$(MAKE) universal-media

universal-media:
	sh os/image/mkimage.sh --out $(IMAGES) --name savior-universal --version $(VERSION) \
		--payload x86_64=$(X86_64_PAYLOAD)/vmlinuz,$(X86_64_PAYLOAD)/initrd \
		--payload i686=$(I686_PAYLOAD)/vmlinuz,$(I686_PAYLOAD)/initrd

# --- housekeeping -------------------------------------------------------------------

clean:
	rm -rf $(BUILD)/savior $(BUILD)/savior.exe $(BUILD)/linux-amd64 $(BUILD)/linux-386 \
		$(BUILD)/darwin-* $(BUILD)/windows-* $(BUILD)/images $(DEV_OUT)

distclean:
	rm -rf $(BUILD)
