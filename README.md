# SaviorOS — E-Waste Savior

**Give old computers a new job.** SaviorOS is a tiny Linux-based operating
system that turns the laptops and desktops nobody wants any more (roughly 2003
onwards, 32- or 64-bit, 256 MB of RAM and up) into members of a **swarm**:

* **Compute nodes** pool their CPUs and memory to run batch jobs: rendering
  frames, transcoding video, crunching data, compiling, or anything else that
  splits into independent tasks.
* **Display nodes** turn their screens into managed signage: status boards,
  text, clocks, images, slideshows, a live swarm dashboard, or one tile of a
  multi-screen **video wall** built from mismatched monitors.
* **The hive** coordinates everything: it finds the nodes, schedules work,
  stores inputs and outputs, and drives the screens. It runs on any SaviorOS
  machine or as a normal program on Linux, macOS or Windows, with a web
  dashboard and a CLI.

SaviorOS runs entirely from RAM. It boots from a USB stick, a CD, or the
network, and never touches the machine's hard disk unless you ask it to, so
dead drives don't matter and whatever was installed stays put.

```
  old ThinkPad ─┐
  P4 tower ─────┤   UDP discovery + pinned HTTPS    ┌─────────────┐   savior ctl
  netbook ──────┼──────────────────────────────────►│    hive     │◄── web dashboard
  17" LCD + box ┤   (swarm key proves membership)   └─────────────┘
  ...           ┘
```

## Highlights

* **One stick for every machine.** A single hybrid image boots on legacy
  BIOS, 64-bit UEFI and 32-bit UEFI, and picks the 64-bit or 32-bit system
  from the CPU. It also runs on CPUs without PAE or SSE2 (Pentium M, Athlon XP).
* **Zero-config join.** Nodes find the hive by LAN broadcast. The only thing
  to set is a swarm key in a text file on the stick, which you can edit on any
  computer. The hive can generate that file for you.
* **Safe by design.** The join handshake is bound to the hive's pinned TLS
  certificate. Tasks run in a sandbox: namespaces, pivot_root into a minimal
  root, cgroup limits, seccomp and dropped privileges. Admin logins never send
  the master token.
* **Kind to old hardware.** Tasks pause when the CPU gets hot, and laptops
  stop taking work on battery. Swap is compressed in RAM (zram). Screens use
  16, 24 and 32 bpp framebuffers, and scaling uses integer math only.
  Nothing rewrites the USB stick in a loop.
* **Real video walls.** Wall geometry is set in millimeters, so bezels and
  mixed monitor sizes line up. An identify mode flashes a short code on every
  screen, and a calibration pattern shows the layout.

## Status

Early development (v0.x). See [docs/DESIGN.md](docs/DESIGN.md) for the full
specification. It covers the protocol, security model, scheduler, display
pipeline and boot process.

## Quick start

1. **Build or download an image.** `make dev-image` builds a test image from
   Ubuntu packages. `make image ARCH=x86_64` (or `i686`) and
   `make universal-image` build production images with Buildroot; see
   [os/buildroot](os/buildroot).
2. **Flash it** to a USB stick: `scripts/flash-usb.sh build/images/savior-universal.img /dev/sdX`
   (it refuses to write to non-removable disks).
3. **Start a hive.** Boot one machine with the *Start as hive* menu entry, or
   run `savior hive` on any computer. The hive screen shows its address, its
   certificate fingerprint and a pairing code for the web dashboard.
4. **Configure the sticks.** Download `savior.conf` from the dashboard (*Add a
   machine*) or run `savior ctl node-config -o savior.conf`, and copy it to the
   root of each stick's `SAVIOR` partition.
5. **Boot the old machines** from the sticks. They show up in the dashboard
   within seconds.
6. **Put them to work:**
   ```sh
   savior ctl login --hive https://192.168.1.20:7700
   savior ctl run --count 20 --input render.sh --output 'frame-*.png' --fetch out/ -- ./render.sh {{index}}
   savior ctl display lobby-screen text --text "Welcome!"
   savior ctl wall create --name lobby --rows 1 --cols 3 --nodes a,b,c --image panorama.jpg
   ```

## Repository layout

| Path | What |
|---|---|
| `cmd/savior` | The single `savior` binary (node agent, hive, CLI, helpers) |
| `internal/` | Go packages: `proto` (protocol), `hive`, `node`, `runner` (sandbox), `display`, `hwinfo`, `power`, `ctl`, `auth`, `config`, `discovery`, `imaging`, `storage` |
| `os/rootfs-overlay` | Init scripts and configuration files shared by all images |
| `os/image` | Boot media builder (`mkimage.sh`): USB image, ISO, PXE tree |
| `os/buildroot` | Buildroot external tree for production x86_64 and i686 images |
| `os/dev` | Dev image built from Ubuntu packages + QEMU test harness |
| `docs/` | Design specification and guides |

## Building

Requirements: Go 1.24+, GNU make, and for images the tools listed in
[os/dev/README.md](os/dev/README.md) and [os/buildroot/README.md](os/buildroot/README.md).

```sh
make build        # host binary in build/savior
make test         # unit and integration tests
make dev-image    # bootable test image from Ubuntu packages
make dev-test     # boot it in QEMU (BIOS, UEFI, ISO, PXE, display)
make swarm-test   # multi-VM swarm end-to-end test
```

## License

No license has been chosen yet. Until one is added, all rights are reserved
by the authors.

The boot images also contain third-party software under its own licenses,
among them the Linux kernel and firmware, BusyBox, GRUB, iPXE, dnsmasq and
dropbear (mostly GPL). Anyone distributing images must also provide their
licenses and corresponding source; for Buildroot images, `make legal-info`
in the Buildroot output directory collects both.
