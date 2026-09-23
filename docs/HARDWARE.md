# Hardware guide

SaviorOS targets the machines people throw away: x86 laptops and desktops
from roughly 2003 onwards. This page explains what works, what needs help,
and how to get stubborn machines booting.

## Which image to use

| Machine | Image |
|---|---|
| Anything with a 64-bit CPU (Core 2 and later, Athlon 64 and later, most Atoms from 2008) | `x86_64` or `universal` |
| 32-bit-only CPUs: Pentium M, Core Duo/Solo (not Core 2), Pentium 4 without EM64T, Atom N270/N280, Athlon XP, VIA C3/C7 | `i686` or `universal` |
| Not sure / mixed pile of machines | `universal` (GRUB picks the right system from the CPU) |

The `universal` stick is roughly twice the size of a single-arch one and
works everywhere, so it's the one to start with.

The 32-bit system uses a non-PAE kernel, so it boots early Pentium M
(Banias/Dothan) and Celeron M laptops that lack PAE. At boot, one of two
`savior` builds is picked: one uses SSE2 when the CPU has it, the other
runs on chips without SSE2 (Pentium III, Athlon XP, Duron, VIA C3 Nehemiah, Geode LX).

## Memory

| RAM | What to expect |
|---|---|
| 256 MB | Boots and works as a display or a small compute node. Leave `display` on `status`/`text`/`clock` and use small tasks. |
| 512 MB – 1 GB | Comfortable compute or display node. |
| ≥ 1 GB | Recommended for the hive role on SaviorOS (it keeps job state and blobs). |

SaviorOS runs from RAM and uses compressed swap in RAM (zram). Kernel
modules and firmware stay compressed in a squashfs image and are
decompressed only when needed.

## Booting

**USB.** Write the `.img` to a stick (`scripts/flash-usb.sh`, or Rufus in
"DD image" mode on Windows, or balenaEtcher). In the firmware setup:
* Put **USB-HDD** (sometimes "USB-ZIP" or "Removable") first in the boot order.
* On 2012+ machines, **disable Secure Boot**. SaviorOS v0.x isn't signed.
  Machines with a dead CMOS battery forget these settings after every power
  loss. Replace the coin cell (CR2032) if you can, or plan on netboot.
* If available, enable **"Restore on AC power loss"** (or "AC recovery")
  so nodes come back after a power cut.

**CD.** Machines that can't boot USB (many pre-2005 BIOSes) can boot the
`.iso` from a CD. A CD can't hold per-site settings, so also plug in any
USB stick (FAT) that has a `savior.conf` in its root. SaviorOS reads it once
Linux is running, even if the BIOS can't boot from it.

**Network (PXE).** Set a hive node's `netboot = yes` and enable "network
boot" / "PXE" on the other machines. Netbooted nodes join keyless and wait
until you approve them in the dashboard. Only use netboot on a LAN you trust.

### Boot menu entries for difficult machines

| Entry | Use when |
|---|---|
| *SaviorOS* | Normal boot. |
| *safe graphics* | The screen goes black or garbled when the graphics driver loads. It uses the firmware framebuffer instead (`nomodeset`). |
| *safe mode* | Boot hangs early on old ACPI/APIC firmware (`noapic nolapic acpi=off irqpoll nomodeset`). |
| *text only* | The BIOS's VESA graphics modes are broken. The screen stays in text mode, which rules out the display role. |
| *display only* | Use this machine purely as a screen. |
| *start as hive* | Make this machine the coordinator. |
| *rescue shell* | Verbose boot plus a root shell on Alt+F2. |

To make a kernel option permanent (for example `video=LVDS-1:d` to switch off a
broken laptop panel), create `boot-options.cfg` in the stick's root containing:

```
set savior_args="video=LVDS-1:d"
```

## Graphics and displays

SaviorOS draws on the Linux framebuffer. It supports 16-, 24- and 32-bit
colour, any resolution, rotation for portrait screens, and multiple
monitor sizes in one video wall.

| Hardware | Status |
|---|---|
| Intel GMA 900/950/X3100/4500 and later HD Graphics | Works (i915 driver). |
| Intel GMA 500/600/3600 (Poulsbo netbooks) | Works (gma500 driver). |
| ATI/AMD Radeon R100 – Southern Islands (≈2000–2013) | Works (radeon driver + firmware). |
| AMD GCN 1.1 (Kabini/Beema/Mullins/Kaveri APUs, HD 7790, R7 260, R9 290) | Uses the firmware framebuffer: radeon leaves these to amdgpu (`cik_support=0`), which isn't included. |
| NVIDIA GeForce 2 – GTX 700 era | Works (nouveau driver). |
| Matrox G200 server graphics, ASPEED BMCs | Works (mgag200 / ast). |
| VIA UniChrome, SiS, S3 Savage, Intel 810/815, ATI Rage | No modern driver. Uses the VESA framebuffer GRUB sets up (1024×768 or 800×600). |
| Newer AMD (GCN 1.2+, amdgpu) | amdgpu isn't included, to keep the image small. Uses the firmware framebuffer. |

Laptops: the lid switch blanks the panel. A connected external monitor keeps
the screen on. The *identify* action flashes a large 3-character code on every
screen so you can tell which physical machine is which node.

## Networking

**Wired:** Intel (e100, e1000, e1000e), Realtek (8139, 8169/8111), Broadcom
(tg3, b44), Marvell (sky2), NVIDIA nForce (forcedeth), VIA Rhine, SiS 900,
Atheros/Qualcomm (atl1, atl1c, atl1e, alx), plus common USB Ethernet
adapters (ASIX, AX88179, RTL8152/8153, CDC).

**Wi-Fi:** set `wifi_ssid` and `wifi_psk` in `savior.conf`.

| Chip | Status |
|---|---|
| Intel 3945/4965 (iwlegacy), Intel 1000–7000 series (iwlwifi) | Works (firmware included). |
| Atheros AR5xxx (ath5k), AR9xxx (ath9k) | Works (no firmware needed). |
| Ralink / MediaTek RT2800 (PCI and USB) | Works (firmware included). |
| Realtek RTL8187, RTL8192CE | Works (firmware included). |
| Broadcom BCM43xx (b43), Intel PRO/Wireless 2100/2200/2915 (ipw2x00) | **Bring your own firmware**: the firmware can't be redistributed. Copy the files into a `firmware/` folder on the stick (for example `firmware/b43/ucode15.fw`). They're loaded at boot. |

A wired connection is always more reliable for a swarm. A cheap unmanaged
switch plus a hive with `dhcp_server = yes` makes a working isolated swarm
with no router.

## Temperatures, batteries and power

* SaviorOS reads the CPU's own sensor (coretemp, k8temp, k10temp,
  via_cputemp) or ACPI thermal zones. Tasks pause when the CPU reaches
  `max_temp_c` (default 85 °C) or the sensor's own limit, whichever is lower,
  and resume 10 °C cooler. The CPU's thermal-throttle counters are watched
  too, which protects Pentium 4 / Pentium M machines without a CPU sensor.
  Old laptops with dried-out thermal paste will spend a lot of time paused.
  Re-pasting them is the single best upgrade.
* Laptops stop taking new tasks on battery (`run_on_battery = no`). If the
  battery drops below `battery_min_percent`, running tasks go back to the
  queue. Batteries under 20% health are treated as missing.
* Disks are never used unless you set `scratch`. A machine with a dead or
  removed hard drive works fine.

## Known limitations

* No Secure Boot support yet (planned: shim + MOK).
* No GPU compute. Tasks use the CPU only.
* The minimum CPU is i686 with CMOV (Pentium Pro/II and later, Athlon,
  VIA C3 Nehemiah, Geode LX). 486, Pentium/Pentium MMX, AMD K6, VIA C3
  Samuel/Ezra and Geode GX are not supported.
* BIOSes that can boot neither USB, CD nor PXE can't be used without
  installing SaviorOS to their disk (planned: `savior install`).
