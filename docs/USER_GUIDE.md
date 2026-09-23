# SaviorOS user guide

This guide walks through building a swarm: setting up a hive, preparing
boot sticks, adding machines, running jobs and driving screens. For
hardware questions see [HARDWARE.md](HARDWARE.md). For the security model
see [SECURITY.md](SECURITY.md).

## 1. Concepts

* **Node:** an old computer booted from SaviorOS. It runs `savior node`, which
  finds the hive, reports its hardware and health, runs tasks, and drives its
  screen.
* **Hive:** the coordinator. It runs `savior hive` (on SaviorOS with the
  `hive` role, or on any Linux/macOS/Windows computer) and serves the web
  dashboard and API on HTTPS port 7700.
* **Swarm key:** a shared secret that lets machines join. Nodes and the hive
  prove they know it without sending it.
* **Hive fingerprint:** the SHA-256 of the hive's certificate. Nodes that
  have it pinned (`hive_fingerprint`) talk only to that hive.
* **Job / task:** a job is a command or script run `count` times. Each run is
  a task with its own index (`SAVIOR_TASK_INDEX`, `{{index}}`).
* **Display spec:** what a node's screen shows (status, text, clock, image,
  slideshow, dashboard, part of a video wall, ...).

## 2. Start a hive

**On a normal computer** (the easiest option when you already have a laptop
at hand):

```sh
savior hive            # listens on :7700, stores state in ~/.local/share/savior/hive
```

It prints its addresses, its certificate fingerprint, where the admin token
is stored, and a pairing code for the dashboard. On Windows and macOS,
allow TCP 7700 and UDP 7701 through the firewall. The command prints the
exact rule.

**On a SaviorOS machine:** boot any stick with the *Start as hive* menu entry,
or put `roles = auto, hive` in its `savior.conf`. The hive's screen shows its
URLs, fingerprint and a pairing code. On first start it creates an ext4
`SAVIOR-DATA` partition in the stick's free space to keep its state across
reboots. Use a stick of 2 GB or more. If there's no free space, the hive runs
from RAM and warns you that its state is lost on reboot.

## 3. Prepare boot sticks

1. Write the image to each stick:
   `scripts/flash-usb.sh savior-universal.img /dev/sdX` (Linux/macOS), or
   Rufus in *DD image* mode or balenaEtcher on Windows.
2. Get a `savior.conf` for your swarm, either from the dashboard (*Overview →
   Add a machine → Download savior.conf*) or with
   `savior ctl node-config -o savior.conf`. It contains:
   ```
   swarm_key = ...
   hive_fingerprint = sha256:...
   hive = auto            # or 192.168.1.20:7700
   ```
3. Copy it to the root of each stick's `SAVIOR` partition, replacing the one
   that's there. The partition is plain FAT32 and you can edit it on any computer.

Optional per-machine settings go in the same file (`savior config keys` lists
them all): `name`, `roles`, `wifi_ssid`/`wifi_psk`, `labels`,
`max_cpu_percent`, `display_rotate`, `ssh_key`, and so on.

## 4. Add machines

Boot each machine from its stick (the boot menu key is usually F12, F10, F9
or Esc). Within a minute it appears in the dashboard's *Nodes* page and in
`savior ctl nodes`. Its screen, or its text console on Alt+F1 when there's no
framebuffer, shows its name, a 3-character short code, its addresses and
the state of the hive connection with a hint when something is wrong:

| Screen says | Fix |
|---|---|
| no swarm key | put `swarm_key = ...` in savior.conf on the stick |
| searching | the hive isn't reachable by broadcast: check it's running, or set `hive = <address>` |
| key mismatch | a hive was found but its swarm key differs: re-copy savior.conf |
| unreachable | the hive's port 7700 is blocked, usually by a firewall on the hive computer |
| fingerprint mismatch | `hive_fingerprint` doesn't match this hive: download a fresh savior.conf |
| pending | the hive uses `join_policy = approve`: approve the machine in the dashboard |

To find out which machine is which, use **Identify**. `savior ctl identify
--all` shows every screen's short code in huge letters and beeps. Then
rename the machines: `savior ctl rename <short-code-or-id> lab-shelf-3`.

## 5. Use the dashboard

Open `https://<hive-address>:7700/` in a browser. The browser warns about the
self-signed certificate. Compare the fingerprint it shows with the one on
the hive's screen, then continue. Log in with the **pairing code** from the
hive's screen or from `savior ctl pair`. You never type the admin token
into a browser.

## 6. Use the CLI

```sh
savior ctl find                                 # list hives on the LAN
savior ctl login --hive 192.168.1.20            # proves the admin token without sending it, pins the hive
savior ctl nodes
savior ctl stats
```

The admin token is in the hive's data directory (`admin_token`, readable
only by the hive's user). `login` asks for it once and stores a 12-hour
session in `ctl.json` under your user config directory.

## 7. Run jobs

Every task runs in its own sandbox on a node. It has a private root file
system with the OS's tools (BusyBox) and its work directory at `/work`. It
has no network unless you ask for it (`--network`), and it has CPU, memory
and scratch-space limits. Bring your own programs as static binaries or
scripts via `--input`.

```sh
# 20 tasks, each writing a result file; wait and download everything into out/
savior ctl run --count 20 --output 'result-*.txt' --fetch out/ -- \
    sh -c 'echo $(( {{index}} * {{index}} )) > result-{{index}}.txt'

# Ship a static binary and an input file; 2 cores and 512 MB per task
savior ctl run --count 8 --cores 2 --mem 512 \
    --input ./render:render --input scene.dat \
    --output 'frame-*.png' --wait -- ./render scene.dat --frame {{index}}

# A shell script from a file
savior ctl script crunch.sh --count 100 --output 'part-*.csv' --wait

savior ctl jobs
savior ctl logs -f <task-id>
savior ctl outputs <job-id> -o results/
savior ctl cancel <job-id>
```

**Placement:** `--arch amd64` (or `386`), `--label room=lab`, `--node lab-3`,
`--min-mem 1024` and `--cpu-flags sse2` restrict where tasks run. A task that
fits no node waits and shows a warning. When you ship programs with
`--input`, `ctl` reads their ELF headers and limits the job to their
architecture: a 64-bit SaviorOS node can't run 32-bit programs, and a 32-bit
machine can't run 64-bit ones. To run on both, ship both builds and pick one
in a script by `uname -m`; `--arch any` turns the automatic limit off. A
command that isn't found exits with 127, and one that can't run on the
machine exits with 126. Both count as a failed attempt.

**Retries:** failed tasks are retried (`--retries`, default 1). If a machine
disappears, is unplugged or overheats, its tasks move to another node without
using up a retry. If tasks keep failing on one machine but succeed
elsewhere, that machine is quarantined (`savior ctl unquarantine` clears it).

**Isolation:** by default jobs run only on fully sandboxed nodes (all SaviorOS
nodes). `--isolation any` also allows nodes running `sandbox = none`, for
example a developer's Linux box running `savior node`.

## 8. Screens and video walls

```sh
savior ctl display lobby-1 text --text "Welcome to the lab" --bg 003366
savior ctl display lobby-2 clock --timezone Europe/Berlin
savior ctl display lobby-3 slideshow --images-dir ./photos --interval 15
savior ctl display hall-1 dashboard        # live swarm statistics
savior ctl display hall-1 status           # back to the default status screen
savior ctl rotate hall-2 90                # a monitor mounted in portrait
```

**Video walls** spread one image, slideshow, text or colour over several
screens. List the nodes row by row (`-` leaves a gap). Geometry is in
millimetres, so mixed monitor sizes and bezels line up: SaviorOS reads
each monitor's physical size from its EDID, and `--gap-x`/`--gap-y` set the
distance between the visible areas of neighbouring screens.

```sh
savior ctl wall create --name lobby --rows 2 --cols 3 \
    --nodes a,b,c,d,e,f --gap-x 30 --gap-y 40 --test   # calibration pattern first
savior ctl wall content <wall-id> --image panorama.jpg
```

The calibration pattern draws a grid, diagonals and circles across the
whole wall, plus each screen's row, column and name. Adjust gaps until the
lines run straight across the bezels.

Slideshows on a wall change slides at the same moment on every screen. The
nodes use the hive's clock and prepare the next slide in advance.

## 9. Maintenance

* **Drain** a node before unplugging it (`savior ctl drain lab-3`): it
  finishes its current tasks and takes no new ones.
* **Reboot / power off:** `savior ctl reboot lab-3`, `savior ctl poweroff lab-3`.
* **Blobs** (uploaded inputs, outputs, images) live on the hive. `savior ctl gc`
  removes the ones nothing uses any more. The hive keeps the last 500
  finished jobs.
* **SSH:** add `ssh_key = ssh-ed25519 AAAA...` to a node's savior.conf to log
  in as root. Without keys, no SSH server runs.
* **Local shell:** `console_shell = yes` gives a root shell on Alt+F2 (for
  debugging; anyone at the keyboard gets root).
* **Logs on a node:** `/var/log/savior-node.log`, `/var/log/savior-init.log`,
  and `logread`.

## 10. Netboot (PXE)

For a lab of machines that can network-boot, set `netboot = yes` on a
SaviorOS hive booted from a stick. It serves the boot files and answers PXE
requests next to your existing DHCP server (proxy-DHCP). With
`dhcp_server = yes` and a static `ip`, it's the network's DHCP server too,
which suits an isolated switch. Netbooted machines never receive the swarm
key. They join *keyless* and wait until you approve them in the dashboard.
Only use netboot on a network you trust.
