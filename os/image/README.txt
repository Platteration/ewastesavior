SaviorOS @VERSION@ - old computers, new jobs
============================================

SaviorOS turns old laptops and desktops (about 2003 and newer, 32- or
64-bit, 256 MB RAM or more) into members of a swarm. Each machine offers
its CPU and RAM to batch jobs, and its screen as a status board, sign,
clock, dashboard or one tile of a video wall. One machine (or any Linux,
macOS or Windows computer) runs the "hive" that coordinates everything.

SaviorOS runs entirely from RAM. It does not touch the machine's hard disk
unless you ask it to (see "scratch" in savior.conf), so the old operating
system stays where it is.


QUICK START
-----------
1. Write the SaviorOS image to a USB stick (Rufus in "DD image" mode,
   balenaEtcher, or dd on Linux/macOS). The stick then shows up on any
   computer as a small drive called SAVIOR.
2. Open savior.conf on the stick in a text editor (Notepad is fine) and
   set swarm_key to the same secret on every machine and on the hive.
   On the hive, "savior ctl genkey" makes a good one, and
   "savior ctl node-config" writes a complete savior.conf for you.
3. Plug the stick into the old computer, switch it on and pick the stick
   in the boot menu (see below). After about a minute the screen shows the
   machine's name, its address and whether it found the hive.
4. Once the status screen appears you can pull the stick and use it for
   the next machine: every setting is in RAM. (A hive keeps its stick.)


EDITING savior.conf
-------------------
savior.conf is a plain text file, one "key = value" per line. Lines that
start with # are comments; remove the # to use a setting. Windows line
endings are fine. The most important keys:

    swarm_key = <the same secret everywhere>
    hive_fingerprint = sha256:...   (pins the hive; strongly recommended)
    name = kitchen-laptop           (optional; the hive can rename it)
    roles = auto                    (compute + display, or hive)
    wifi_ssid = MyNetwork           (only for Wi-Fi)
    wifi_psk = my wifi password

Every setting is explained in the file itself. Changes take effect at the
next boot.

Some sticks are made with settings built in (savior.conf then says so at
the top). Commenting out or deleting such a line does not switch it off:
the built-in value stays. Set another value instead, for example
"console_shell = no", or "ssh_key =" with nothing after the = to remove
built-in SSH keys.


BOOTING FROM THE STICK
----------------------
- Boot menu keys: most PCs show a one-time boot menu with F12, F11, F10,
  F9, F8 or Esc right after power-on. Dell: F12. HP: F9 (Esc on some).
  Lenovo: F12 (ThinkPads: F12 or the blue ThinkVantage key). Acer: F12
  (enable "F12 Boot Menu" in the setup first). Asus: Esc or F8.
  Toshiba: F12. Older Macs: hold Alt/Option.
- To boot SaviorOS every time, open the firmware setup (F2, F1, F10 or
  Del at power-on) and put "USB-HDD" / "USB Storage" / "Removable
  Devices" first in the boot order. On some older BIOSes the stick is
  listed under "Hard Disk Drives"; move it to the top there.
- Very old BIOSes may offer "USB-ZIP" or "USB-FDD": choose USB-HDD.
- CD-only machines: burn savior.iso to a CD. The CD can take its settings
  from any USB stick that has a savior.conf in its top folder.


SECURE BOOT
-----------
SaviorOS cannot boot with Secure Boot on (yet). In the firmware setup,
look under "Security" or "Boot" for "Secure Boot" and set it to Disabled.
Some machines first need a supervisor password, or "OS Type: Other OS",
or "CSM / Legacy support: Enabled". Machines from before 2012 don't have
Secure Boot at all.


MACHINES THAT SHOULD STAY ON
----------------------------
For machines that should come back after a power cut, look in the
firmware setup for "Restore on AC Power Loss", "AC Recovery", "After Power
Failure" or "Power On after Power Failure" and set it to "Power On" (or
"Last State"). Laptops: SaviorOS stops taking work on battery unless
run_on_battery = yes, and pauses work when the CPU runs hot.


WI-FI FIRMWARE
--------------
Common Intel, Atheros, Ralink and Realtek chips work out of the box. Some
cards (for example Intel PRO/Wireless 2100/2200 and Broadcom b43) need
firmware files that SaviorOS may not ship. Copy those files into the
folder "firmware" on this stick (create it if needed), keeping the
subfolder layout from linux-firmware (for example firmware/b43/...), then
reboot. A wired network cable always works without extra files.


BOOT MENU ENTRIES
-----------------
  SaviorOS                  normal start (picks 64- or 32-bit by itself)
  safe graphics (nomodeset) blank or garbled screen? try this first
  safe mode                 machines that freeze early (no ACPI/APIC)
  text only                 no graphics mode at all
  display only              use the screen, don't run compute jobs
  start as hive             make this machine the coordinator
  rescue shell              verbose boot plus a root shell on Alt+F2
  force 32-bit              run the 32-bit system on a 64-bit machine

Extra kernel options for every entry can go into a file boot-options.cfg
in the top folder of this stick, for example to switch off a broken
laptop panel output:

    set savior_args="video=LVDS-1:d"


ON THE MACHINE
--------------
  Alt+F1   status screen (name, address, hive connection, load)
  Alt+F2   root shell, only with console_shell = yes (else a notice)
  Alt+F7   the display (signs, slideshows, video wall)

SSH: put your public key into ssh_key in savior.conf and log in as root.


NETWORK BOOT
------------
A hive with the stick in and netboot = yes can boot whole labs over the
network (PXE). Netbooted machines never receive the swarm key: they show
up as "pending" on the hive until you approve them.


HELP
----
Documentation, source code and issue tracker:
    https://github.com/platteration/ewastesavior
When asking for help, include the machine model, what the screen showed,
and the files /var/log/savior-init.log and /var/log/savior-node.log
(rescue shell: "cat /var/log/savior-init.log").
