# SaviorOS design specification

Status: v0.2. This revision covers a five-lens design review (old hardware,
security, boot and build, display and UX, scheduling and protocol). This
document is the authoritative contract between the components in this
repository. When code and this document disagree, fix one of them in the same change.

## 1. Goals

SaviorOS is a tiny Linux distribution that turns old x86 laptops and desktops
(roughly 2003 onwards, 32- and 64-bit, 256 MB RAM and up) into members of a
**swarm**:

* **Compute nodes** contribute CPU and RAM to batch jobs submitted to the swarm.
* **Display nodes** turn the machine's screen into a managed output: status
  board, signage (text, images, slideshows, clock), swarm dashboard, or one tile
  of a multi-screen **video wall**.
* **The hive** (coordinator) tracks nodes, schedules work, stores job
  inputs and outputs, and drives displays. Any SaviorOS machine can be the hive
  (`roles=hive`), or it can run as an ordinary program on any Linux, macOS or
  Windows computer.

Design principles:

1. **Run from RAM, touch nothing.** The whole OS is an initramfs. Internal
   disks are neither mounted nor written unless the operator opts in (the
   `scratch` key), so dead drives don't matter and the machine's original OS
   stays where it is.
2. **One stick, any machine.** A single hybrid USB image boots on legacy BIOS,
   64-bit UEFI and 32-bit UEFI, and picks a 64-bit or 32-bit payload based on
   the CPU. An ISO covers CD-only machines, and PXE netboot covers whole labs.
3. **Zero-config LAN join.** A node finds the hive by LAN broadcast. The
   operator writes a `swarm_key` (and ideally a `hive_fingerprint` pin) into a
   text file on the stick's FAT partition, from any computer. The hive can
   generate that file (`savior ctl node-config`).
4. **Stateless nodes.** Node identity is derived from hardware. Everything a node
   needs to remember (name, labels, display assignment, rotation) is stored on the hive.
5. **Small and boring.** Linux kernel + BusyBox + a single static Go binary
   (`savior`) that holds every SaviorOS service. No systemd, no package manager
   on the node, no runtime dependencies.
6. **Respect old hardware.** Throttle on real CPU temperature, stop taking
   work on battery, use zram, support 16/24 bpp framebuffers and CPUs without
   SSE2 or PAE, keep the initramfs within a RAM budget, and never
   rewrite flash in a tight loop.

Non-goals (v0.x): writing a kernel, containers/OCI images, GPU compute,
desktop use, multi-hive federation, Secure Boot (v0.x requires it to be off;
shim + MOK is the planned path).

## 2. System overview

```
             +-----------------------------------------------+
 operator -> |                  HIVE (savior hive)            |
 savior ctl  |  HTTPS :7700  admin API + web dashboard        |
 browser     |  node API  | scheduler | blob store | walls    |
             |  UDP :7701 beacon     HTTP :7702 netboot files |
             +------------------+----------------------------+
                                | HTTPS, TLS pinned via swarm-key proof
          +---------------------+----------------------+
   +------+------+       +------+------+        +------+------+
   | node agent  |       | node agent  |        | node agent  |
   | compute     |       | compute     |        | display     |
   +-------------+       +-------------+        +-------------+
```

Every node runs `savior node`. It:

1. derives its node ID from hardware (10.1),
2. finds the hive (config `hive=`, else UDP beacons, 7.3),
3. registers using a swarm-key proof bound to the pinned TLS certificate (6.2),
4. sends a heartbeat every 5 s with metrics and running tasks, and gets
   **directives** back: display spec, rotation, drain, cancellations, actions,
5. long-polls for tasks while it has free capacity, runs them in a sandbox,
   uploads outputs, and reports results,
6. drives the local screen if it has the display role.

## 3. Repository layout

```
cmd/savior/            multi-call binary entry point (subcommand dispatch)
internal/version/      build version + build time (set via -ldflags)
internal/proto/        wire types + shared validation (THE protocol)
internal/config/       savior.conf + kernel cmdline parsing, `savior config`
internal/auth/         stretched secrets, proofs, nonces, tokens, TLS identity + pinning
internal/discovery/    UDP beacon announce/probe/collect
internal/hwinfo/       inventory, identity, live metrics from /proc and /sys; `savior info`
internal/power/        battery / AC / thermal / lid policy (pure logic)
internal/runner/       task execution: inputs, sandbox, cgroups, seccomp, outputs; `savior sandbox-exec`
internal/display/      fbdev driver, VT handling, scene renderer, fonts, walls; `savior display`
internal/hive/         coordinator: API, scheduler, state store, blobs, walls, sessions, web UI
internal/node/         node agent main loop; `savior node`, `savior console`
internal/ctl/          operator CLI; `savior ctl`
internal/storage/      SaviorOS stick helpers (find media, data partition); `savior storage`
os/rootfs-overlay/     files copied verbatim onto the target rootfs (init scripts etc.)
os/image/              boot media assembly (mkimage.sh: USB .img, ISO, netboot tree)
os/buildroot/          BR2_EXTERNAL tree for production images (x86_64, i686)
os/dev/                dev image built from host (Ubuntu) packages + QEMU test harness
docs/                  user and developer documentation
```

Go module `github.com/platteration/ewastesavior`, Go 1.24, `CGO_ENABLED=0`,
built with `-trimpath -ldflags "-s -w -X ...version.Version=... -X ...version.BuildTime=..."`.
Allowed dependencies: the standard library, `golang.org/x/sys`, `golang.org/x/image`.

Node binaries: `linux/amd64` (`GOAMD64=v1`), `linux/386` built twice
(`GO386=sse2` and `GO386=softfloat`). At boot, `S00mounts` keeps the SSE2 build
when `/proc/cpuinfo` lists `sse2` and deletes the other (P3, Athlon XP, VIA
C3 and Geode get softfloat). Hive and ctl code must also compile for
`darwin/*` and `windows/*`. Linux-only syscalls go in `_linux.go` files with
stubs elsewhere.

## 4. The `savior` binary

| Command | Entry point | Purpose |
|---|---|---|
| `savior node [flags]` | `node.Main(args) int` | node agent |
| `savior hive [flags]` | `hive.Main(args) int` | coordinator |
| `savior ctl <cmd> ...` | `ctl.Main(args) int` | operator CLI |
| `savior info [--json]` | `hwinfo.Main(args) int` | inventory + metrics |
| `savior display <cmd>` | `display.Main(args) int` | `test`, `render` (to PNG), `show` (spec on fb), `vt-reset` |
| `savior config <cmd>` | `config.Main(args) int` | `env`, `get`, `dump`, `keys`, `sample` |
| `savior console` | `node.ConsoleMain(args) int` | text status screen for tty1 |
| `savior storage <cmd>` | `storage.Main(args) int` | `find-media`, `init-data`, `pick-binary` (boot helpers) |
| `savior sandbox-exec` | `runner.SandboxExecMain(args) int` | internal sandbox shim |
| `savior version` | main | version |

Each `Main` parses its own flags (`flag.NewFlagSet`), returns an exit code
and never calls `os.Exit` (except `SandboxExecMain`, which ends in `execve`).
Logging: `log/slog` text to stderr, or with `--log-file PATH` to a
size-rotated file (1 MiB, one `.1` backup) plus stderr. `--log-level`/`log_level`.

Long-running commands (`node`, `hive`) call `debug.SetMemoryLimit`
(node: 25% of MemTotal, min 48 MiB) and on Linux as root set their own
`oom_score_adj` to -900. At start, if the system clock is before
`version.BuildTime`, they raise it to BuildTime (dead CMOS batteries).

## 5. Configuration (`savior.conf`)

### 5.1 Sources and precedence

1. Built-in defaults (lowest).
2. `/etc/savior/savior.conf`, which holds the image defaults. The SaviorOS image
   ships `sandbox = strict` here.
3. `/etc/savior/baked.conf` from an optional second initrd (`savior-conf.cpio`)
   that mkimage `--conf` or the hive's netboot bakes in.
4. `savior.conf` in the root of the config medium mounted at `/media/savior`
   (13.3).
5. The kernel command line `savior.<key>=<value>` (highest). Values are
   percent-decoded (`%20` = space). Cmdline values are readable by every local
   process, so secrets should not be passed this way except for testing.

`S08config` merges everything into `/run/savior/savior.conf` (mode 0600) with
`savior config dump --out`, plus `/run/savior/env` (0600, `savior config env`).
Services read only the merged file.

### 5.2 File format

Unchanged from v0.1. Each line is `key = value`, and `#` starts a comment
only at the start of a line. Values may be wrapped in `"..."` (with `\"` and
`\\` escapes). Lines may end in CRLF and the file may start with a BOM.
Unknown keys, and invalid values for known keys, only produce warnings.
List keys: a source that sets a list key replaces earlier sources' values,
and repeated lines within one source append.

### 5.3 Keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | str | "" | Node name, `[A-Za-z0-9-]`. The hive lowercases it and falls back to the default name `savior-<last 6 hex of identity>` if invalid or already taken. An admin rename wins. |
| `node_id` | str | "" | Override the hardware-derived node ID (`[a-z0-9-]`). |
| `roles` | list | `auto` | `auto`, `compute`, `display`, `hive`. `auto` = compute + display when any `/sys/class/drm/card*` or `/sys/class/graphics/fb*` exists (checked continuously; a display that appears later enables the role). |
| `labels` | list | "" | `key=value` config labels (admin labels override them key by key). |
| `hive` | str | `auto` | `auto`, `host`, `host:port`, `https://host:port`. |
| `swarm_key` | str | "" | Shared join secret, min 16 chars (warning below 24). `savior ctl genkey` makes 32 base32 chars. |
| `hive_fingerprint` | str | "" | `sha256:<hex>` pin of the hive certificate. Strongly recommended; without it the swarm key alone decides who is "the hive". |
| `join` | str | `key` | `key` = join with the swarm key; `keyless` = join without a key and wait for admin approval (netboot). |
| `net` | str | `dhcp` | `dhcp`, `static`, `off`. With `dhcp` and no lease after 30 s, IPv4 link-local (169.254/16) is used if the image has `zcip`. |
| `ip`, `gateway`, `dns` | | | Static addressing (`ip` in CIDR). |
| `wifi_ssid`, `wifi_psk`, `wifi_country` | | "", "", `US` | Wi-Fi (WPA2-PSK or open). |
| `dhcp_server` | bool | no | Serve DHCP on the first wired interface (needs `net=static`). With `netboot=yes` dnsmasq does DHCP+PXE instead. |
| `dhcp_range` | str | "" | `first-last`; empty = .100-.200 of the static subnet. |
| `netboot` | bool | no | Serve PXE boot (hive role). |
| `ntp` | list | `pool.ntp.org` | NTP servers or `off`. |
| `ssh_key` | list | "" | Root SSH keys (dropbear). No keys = no SSH server. |
| `console_shell` | bool | no | Root shell on tty2 without password. |
| `max_cpu_percent` | int | 100 | Percent of logical CPUs offered (fractional cores allowed). |
| `max_mem_percent` | int | 75 | Percent of the memory budget offered (10.3). |
| `sandbox` | str | `auto` | `strict` (SaviorOS image default): refuse tasks without full isolation; `auto`: use what's available; `none`: no namespaces/cgroups (privileges are still dropped when root). |
| `scratch` | str | `ram` | `ram`, a block device, or `LABEL=x` with an existing ext2/3/4 fs. |
| `scratch_wipe` | bool | no | Allow formatting `scratch` (ext4, label `SAVIOR-SCRATCH`). DESTRUCTIVE. |
| `run_on_battery` | bool | no | Laptops: keep taking tasks on battery. |
| `battery_min_percent` | int | 40 | On battery below this, running tasks are preempted. |
| `max_temp_c` | int | 85 | Upper bound for the thermal pause threshold (10.4). |
| `cpu_governor` | str | `auto` | `auto` (schedutil, else ondemand, else driver default), or a governor name. |
| `display_mode` | str | `status` | Local mode until the hive sends one: `status`, `off`, `text`, `clock`, `test`. |
| `display_text` | str | "" | Text for `display_mode=text`. |
| `display_rotate` | int | 0 | Initial rotation 0/90/180/270. The hive-stored value wins once set. |
| `display_device` | str | `auto` | `auto` (11.1) or a `/dev/fbN` path. |
| `display_idle_off_min` | int | 15 | Blank the screen after N minutes without input, but only in the local default status/clock modes. 0 = never. |
| `hive_listen` | str | `:7700` | Hive HTTPS listen address. |
| `hive_data` | str | `auto` | Hive state dir. `auto` on SaviorOS = the `SAVIOR-DATA` ext4 partition (created on first use, 13.4), else RAM (with a warning). Elsewhere, `$XDG_DATA_HOME/savior/hive` or `~/.local/share/savior/hive`. |
| `admin_token` | str | "" | Admin token (min 32 chars). Empty = generated and stored in `<hive_data>/admin_token` (0600). |
| `join_policy` | str | `open` | `open`: key-proven nodes join directly. `approve`: new nodes are pending until an admin approves them. Keyless nodes always need approval. |
| `beacon` | bool | yes | Hive announces itself. |
| `log_level` | str | `info` | |
| `timezone` | str | `UTC` | IANA zone (tzdata is embedded). |

### 5.4 Go API

As in v0.1 (`Default`, `Load`, `Set`, `Get`, `Values`, `WriteFile`,
`ShellEnv`, `Keys`, `EffectiveRoles(hasDisplay bool)`, `HiveURL`,
`NormalizeFingerprint`, `LoadDefault`, `Main`). The load order in 5.1 is
implemented by `DefaultFiles` = `/etc/savior/savior.conf`,
`/etc/savior/baked.conf`, `/media/savior/savior.conf`.

## 6. Security model

Assets: swarm membership, the hive identity, the admin role, task data, and
the nodes themselves.

Trust assumptions:
* Anyone holding the swarm key can join as a node.
* Without a `hive_fingerprint` pin, anyone holding the swarm key can also
  impersonate the hive to nodes that re-join. Pinning closes this, which is why
  `savior ctl node-config` always emits the pin.
* Whoever holds the admin token or an admin session controls the swarm.
* Physical access to a node (or its stick) = root on that node and
  possession of the swarm key.
* Task code is untrusted by the node: tasks must not be able to read node
  secrets, other tasks' data, or escalate.
* Netboot implies a trusted LAN (PXE is unauthenticated); netboot never serves the swarm key.

### 6.1 Hive TLS identity

ECDSA P-256 self-signed certificate in `<hive_data>/tls/` (20 years), random
`hive_id`. Fingerprint = `sha256:` + hex SHA-256 of the DER certificate.
Clients verify only the fingerprint (pin) and ignore dates. The hive shows its
fingerprint on its screen, in logs, in `HiveInfo`, and in `node-config`.

### 6.2 Node join handshake

Both sides stretch the key once per process:
`K = PBKDF2-SHA256(swarm_key, "savior-swarm-v1", 200000, 32)` (`auth.NewSwarmSecret`).

```
node                                          hive
 1 TLS connect with TOFU config, record FP_seen
   (if hive_fingerprint is set: require FP_seen == pin)
 2 GET /api/v1/hello --------------------------> nonce = stateless MAC'd nonce (auth.NonceIssuer, 60 s TTL)
   <-------------------------------------------- {hive_id, api_version, version, nonce, time}
 3 NEW client pinned to FP_seen. /register and every later request go only
   through clients pinned to FP_seen. A proof is never sent over a
   connection whose certificate differs from the fingerprint inside it.
 4 proof = K.NodeProof(hive_nonce, node_nonce, node_id, FP_seen)
   POST /api/v1/register {..., hive_nonce, node_nonce, proof}
                                                 check nonce MAC + TTL; single use (bounded set of used nonces)
                                                 recompute with FP_own; constant-time compare
                                                 issue token (32 random bytes hex), store SHA-256(token)
   <-------------------------------------------- {token, hive_proof = K.HiveProof(same, FP_own), ...}
 5 verify hive_proof with FP_seen; keep the pin for the process lifetime
```

`Hello.Time` is display-only. Nodes adjust their clock only from
`RegisterResponse`/`HeartbeatResponse` after `hive_proof` has been verified
(10.2). Keyless join (`join=keyless`, used by netboot): the node sends
`proof=""`. The hive accepts it only if `hive_fingerprint` is pinned on the
node side (checked by the node) and places the node in `pending` (no tasks,
no blobs, status screen only) until an admin approves it. A keyless node whose
HWIDs match a previously approved record is re-approved automatically. This
is documented as trusted-LAN only.

Tokens live only in hive memory. After a hive restart nodes get 401 and
re-register (with their running tasks, 8.6).

Swarm hint (beacons): `K.SwarmHint()`, 8 hex chars (32 bits). It is not in `Hello`.

### 6.3 Admin access

* **ctl login (proof):** `GET /hello`, then over a client pinned to FP_seen,
  `POST /api/v1/admin/login {hive_nonce, client_nonce, proof = A.AdminProof(hive_nonce, client_nonce, FP_seen)}`
  where `A = auth.NewAdminSecret(admin_token)`. The hive verifies with FP_own and
  returns `{session, expires_at (12 h), hive_proof = A.HiveProof(hive_nonce, client_nonce, "admin", FP_own)}`.
  ctl verifies `hive_proof`, then pins the fingerprint in its config (TOFU
  leaks nothing reusable). The admin token itself is never sent.
* **Browser:** the dashboard never asks for the master token. The hive shows a
  pairing code (8 chars Crockford base32, 10 min, single use) on its screen and
  `savior ctl pair` prints one. `POST /api/v1/admin/session {pair_code}`, or
  `{token}` for scripts, sets the cookie
  `savior_admin=<session>; HttpOnly; Secure; SameSite=Strict; Path=/`.
  State-changing requests authenticated by cookie must carry `X-Savior: 1`, and
  their Origin (if present) must match the Host (CSRF). `POST /api/v1/admin/logout`
  revokes the session, and `POST /api/v1/admin/sessions/revoke` revokes all of them.
* Admin endpoints accept `Authorization: Bearer <session>` or the cookie. The raw
  admin token as a bearer is also accepted, for scripts on trusted hosts. Stored
  secrets are compared as SHA-256 hashes in constant time.
* **Rate limits:** `/hello` 20/s per source. Failed auth is counted in separate
  buckets for register, admin and pair, keyed by IPv4 /32 or IPv6 /64: after 5
  failures in a minute that source gets 429 for 60 s. Maps are capped at 10k
  entries (evict oldest). At most 4 concurrent claims per node token.
* **Server:** `http.Server{ReadHeaderTimeout: 10s, IdleTimeout: 120s, MaxHeaderBytes: 16 KiB}`.
  There is no global Read/WriteTimeout; handlers set deadlines with
  `http.ResponseController` (claims wait_s + 15 s, JSON 30 s, blobs by size).
* **Web hardening:** every response has `X-Content-Type-Options: nosniff` and
  `Referrer-Policy: no-referrer`. HTML also gets `Content-Security-Policy:
  default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:;
  connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`.
  There is no inline script, and the UI inserts untrusted strings only with
  `textContent`. Blobs are served as `application/octet-stream`,
  `Content-Disposition: attachment` and `Content-Security-Policy: sandbox`.
  Logs are `text/plain; charset=utf-8`. No CORS headers.
* **Input hygiene:** the hive validates `node_id` (`proto.ValidNodeID`) and
  names (`proto.ValidNodeName`, unique, not ID-shaped). It allows at most 32
  labels. Every other node-supplied string is passed through
  `proto.Sanitize` (256 B for inventory, 4 KiB for errors). ctl strips control
  characters (except `\n`, `\t`) before printing logs or names to a TTY.

### 6.4 Node token scope

* GET blob: only blobs referenced by inputs of tasks currently assigned to
  that node, or by media in its current display spec.
* PUT blob: only while it has a running task. Each lease may upload at most
  1 GiB in total. Node-uploaded blobs not referenced by a report within 1 h
  are garbage.
* Claim: free = min(request.Free, node.Total − node.Allocated). Total is
  clamped to the inventory (cores ≤ Inventory.Cores, mem ≤ MemTotalMB). Max
  is ≤ 16. Nodes without the compute role, or that are pending, draining,
  quarantined or paused, get nothing.
* Labels and names reported by a node are advisory. Admin labels override
  them, and `Requirements.Nodes` is resolved to IDs at submit.

### 6.5 Task sandbox

Normative for `runner` when the agent runs as root on Linux:

1. The runner creates the task cgroup and starts `savior sandbox-exec` with
   `CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC|CLONE_NEWUTS` (and `CLONE_NEWNET`
   unless `network=true`), `Pdeathsig=SIGKILL` and `Setsid` (its own
   session, so no controlling terminal; pgid = pid for group kills and
   SIGSTOP). The shim locks itself to one OS thread before anything else and
   marks the status pipe close-on-exec. It uses `CgroupFD`/`UseCgroupFD`
   when available; otherwise the shim writes its own pid to `<cgroup>/cgroup.procs`
   as its very first action, before anything else runs.
2. The shim (still root, inside the namespaces) makes `/` recursively private
   (`MS_REC|MS_PRIVATE`), then builds a new root on a small tmpfs mounted on
   `<WorkRoot>/.sandbox-root` (root 0700, created by the runner; a task dir
   can never have that name):
   * read-only, nosuid, nodev bind mounts of `/bin`, `/sbin`, `/usr`, `/lib` (and `/lib64`, `/lib32` if present);
   * `/etc` (tmpfs, then read-only) with only `passwd`, `group`, `hosts`, `nsswitch.conf`, `ssl/certs` (bind) and, when `network=true`, `resolv.conf`;
   * `/work`: the task workdir, bind rw, nosuid, nodev (cwd and HOME);
   * `/tmp`: tmpfs, `size=64m`, nosuid, nodev;
   * `/proc`: fresh procfs (`hidepid=2` where supported), with `cmdline`,
     `kcore`, `keys`, `kmsg`, `sysrq-trigger`, `timer_list`, `sched_debug` and `config.gz`
     masked by bind-mounting `/dev/null` over them; `sys`, `irq` and `bus`
     bind-mounted onto themselves and remounted read-only, nosuid, nodev,
     noexec (a failure is fatal in `strict` mode);
   * `/dev`: tmpfs with `null`, `zero`, `full`, `random` and `urandom` bind-mounted, plus `devpts` (newinstance) and `ptmx`, `shm` as tmpfs (no `tty`);
   * no `/sys`, `/media`, `/run`, `/var`, `/root`.
   It then runs `pivot_root`, detaches the old root and `chdir /work`.
3. Brings `lo` up in a new network namespace.
4. Sets rlimits: `RLIMIT_CORE=0`, `RLIMIT_NOFILE=4096`, `RLIMIT_NPROC=512`,
   `RLIMIT_FSIZE = disk_mb` (a per-file cap; the real quota is the per-task
   tmpfs or disk scratch accounting).
5. Drops privileges in order: `setgroups([])`, `setresgid(g,g,g)`,
   `setresuid(u,u,u)` with u = g = `10000 + slot` (a per-concurrency-slot uid),
   then verifies with `getresuid`. It clears ambient and bounding
   capabilities, sets `PR_SET_NO_NEW_PRIVS` and `PR_SET_DUMPABLE 0`, sets
   `PR_SET_PDEATHSIG` again (the uid change clears it) and exits if the
   runner is already gone (the status pipe has no reader).
6. Installs a seccomp-BPF filter (`golang.org/x/sys/unix`) with
   `SECCOMP_FILTER_FLAG_TSYNC` (prctl fallback on old kernels). It checks
   `seccomp_data.arch` (AUDIT_ARCH_X86_64 or I386 as native; on x86_64 kills
   i386-compat and x32 syscalls), returns ENOSYS for `clone3`, and EPERM for
   `unshare, setns, mount, umount2, pivot_root, chroot, fsopen, fsconfig,
   fsmount, fspick, open_tree, move_mount, mount_setattr, keyctl, add_key,
   request_key, bpf, perf_event_open, userfaultfd, io_uring_setup,
   io_uring_enter, io_uring_register, kexec_load, kexec_file_load,
   init_module, finit_module, delete_module, open_by_handle_at,
   name_to_handle_at, acct, swapon, swapoff, reboot, settimeofday,
   clock_settime, adjtimex, clock_adjtime, iopl, ioperm, ptrace,
   process_vm_readv, process_vm_writev, quotactl, lookup_dcookie, vhangup,
   syslog` and `clone` with any `CLONE_NEW*` flag.
7. `execve` of the command. A command that isn't found exits 127 and one
   that can't be executed (permissions, wrong architecture) exits 126, like a
   shell: ErrorKind `exit`, which uses up an attempt. Any failure before
   this step writes a reason to the status pipe and exits 125, and only that
   combination is ErrorKind `sandbox`. Since the pipe is close-on-exec, the
   task can't forge it.

`sandbox=none` skips steps 1-3 and 6, but when root it still does 4, 5 and
`no_new_privs`. The node reports its effective `Sandbox` and `SandboxCaps`
in `RegisterRequest`. `FullIsolation` means all of mountns, pidns, netns,
ipcns, utsns, cgroup2 (cpu, memory and pids controllers), seccomp, nnp and
privdrop. Jobs require `isolation=full` unless they say `any`.

Kernel and sysctl hardening (both images; `S10system`): `user.max_user_namespaces=0`,
`kernel.unprivileged_bpf_disabled=1`, `kernel.io_uring_disabled=2`, `vm.unprivileged_userfaultfd=0`,
`kernel.perf_event_paranoid=3`, `kernel.kptr_restrict=2`, `kernel.dmesg_restrict=1`,
`fs.protected_symlinks=1`, `fs.protected_hardlinks=1`, `fs.protected_fifos=2`,
`fs.protected_regular=2`, `kernel.yama.ptrace_scope=1` (when present). Kernel:
`# CONFIG_USER_NS is not set`, CPU mitigations and page-table isolation on.

Files: `S08config` mounts the stick `ro,nosuid,nodev,noexec,uid=0,gid=0,fmask=0177,dmask=0077`
(vfat) or with 0700 dirs (ext4/iso9660 mount options as supported). rcS and
all services run with `umask 077`. `/run/savior/*` and logs are 0600, except
`status.json`, which is 0644.

## 7. Wire protocol (HTTPS + JSON)

All types are in `internal/proto`. JSON is snake_case, times are RFC 3339,
and errors are `{"error": "..."}` with a matching status. Request bodies are
capped at 1 MiB, except blob uploads and log chunks (256 KiB).

### 7.1 Node endpoints

| Method + path | Auth | Request → Response |
|---|---|---|
| `GET /api/v1/hello` | none | → `Hello` |
| `POST /api/v1/register` | proof (or keyless) | `RegisterRequest` → `RegisterResponse`; 409 `duplicate node id` if the ID is online with another BootID; 403 bad proof; 426 when `api_version` (0 = v1) differs |
| `POST /api/v1/heartbeat` | node | `HeartbeatRequest` → `HeartbeatResponse` |
| `POST /api/v1/claim` | node | `ClaimRequest` → `ClaimResponse` (long-poll ≤ 30 s) |
| `POST /api/v1/tasks/{id}/report` | node | `TaskReport` → `{}`; 409 `stale lease` |
| `POST /api/v1/tasks/{id}/log?lease=L&offset=N` | node | raw text → `{"next": N'}` (bytes before the hive's current offset are dropped, so retries are idempotent) |
| `GET /api/v1/blobs/{sha256}` | node (scoped) or admin | bytes (Range supported) |
| `GET /api/v1/blobs/{sha256}/render?cw=&ch=&x=&y=&w=&h=&pw=&ph=&fit=` | node (scoped) or admin | PNG: the image fitted into a canvas cw×ch, the rect x,y,w,h cropped from it, scaled to pw×ph pixels (11.5) |
| `PUT /api/v1/blobs/{sha256}` | node (scoped) or admin | bytes → `BlobInfo` (hash verified; 413 above max; 507 when disk would drop below max(5%, 512 MiB), capped at 10%, free. While free space is below that, jobs with outputs are not dispatched; the job and `HiveInfo.Warnings` say why) |
| `GET /api/v1/stats` | node or admin | → `SwarmStats` |

A node token for a node the hive no longer knows gets 401, and the node re-joins.

### 7.2 Admin endpoints

All admin endpoints are under `/api/v1/admin`. They accept a session (cookie
or bearer) or the admin token (bearer).

| Method + path | Request → Response |
|---|---|
| `POST login` | `LoginRequest` → `SessionResponse` (proof-based, 6.3) |
| `POST session` | `SessionRequest` → `SessionResponse` + cookie |
| `POST logout`, `POST sessions/revoke` | → `{}` |
| `POST pair` | → `PairCode` (admin only; the hive screen shows one too) |
| `GET info` | → `HiveInfo` (includes `X-Savior-Client-Time` handling, 10.2) |
| `GET node-config?hive=<addr>\|auto` | → `text/plain` savior.conf with `swarm_key`, `hive_fingerprint`, `hive` (default: the Host the request came in on) |
| `GET nodes` / `GET nodes/{ref}` | → `[]NodeView` / `NodeView` (`ref` = ID or name) |
| `PATCH nodes/{ref}` | `NodePatch` → `NodeView` (409 for duplicate names, display of wall members, mode=wall) |
| `DELETE nodes/{ref}` | → `{}` (offline only) |
| `POST nodes/{ref}/action` | `NodeAction` → `{}` |
| `POST identify` | `IdentifyRequest` → `{}` |
| `GET jobs?state=&limit=&before=<seq>` | → `[]JobView` (newest first, no spec) |
| `POST jobs` | `JobSpec` → `JobDetail` (persisted before responding; 400 with reason) |
| `GET jobs/{id}` | → `JobDetail` |
| `GET jobs/{id}/tasks?state=&offset=&limit=` | → `TaskPage` (limit ≤ 1000) |
| `GET jobs/{id}/outputs` | → `[]OutputEntry` |
| `GET jobs/{id}/outputs.zip` | streamed zip, entries `task-<index>/<name>`, method Store |
| `POST jobs/{id}/cancel` | → `JobView` |
| `DELETE jobs/{id}` | → `{}` (finished jobs only) |
| `GET tasks/{id}` | → `TaskView` |
| `GET tasks/{id}/log?offset=N&wait_s=W` | → text from offset N, header `X-Savior-Log-Offset: <next>`; long-polls up to W s (≤ 25) when no new data |
| `GET tasks/{id}/outputs/{name}` | → file with Content-Disposition attachment |
| `GET walls`, `POST walls`, `GET/PUT/DELETE walls/{id}` | `WallSpec` |
| `GET blobs` | → `[]BlobInfo` |
| `POST blobs` | raw body → `BlobInfo` (hive hashes while streaming; for browsers) |
| `DELETE blobs/{sha256}` | → `{}`; 409 naming the reference if referenced |
| `POST blobs/gc` | → `{"deleted": n, "freed_bytes": n}` |

`GET /` serves the dashboard: static, embedded, CSP-compatible, no external resources.

### 7.3 Discovery (UDP 7701)

The hive broadcasts a `Beacon` every 5 s, to `255.255.255.255:7701` and to
each interface's directed broadcast address. It answers `Probe` datagrams of
at least `MinProbeSize` bytes, only from directly connected subnets and at most
once per second per source, with a unicast beacon. The node collects beacons
for 2 s (`discovery.Collect`), keeps those matching its swarm hint, and tries
them in order. A hive that answered wrongly (swarm key, fingerprint, API
version, refused registration) is skipped for 5 minutes. One that could not
be reached (not listening yet, network error, 5xx) is skipped for 2 s,
doubling up to 1 minute, until a join succeeds. If beacons are seen but none match, the link state is
`key_mismatch`.

The hive also watches for beacons with its own hint and another hive_id and
reports them in `HiveInfo.Warnings` ("another hive with this swarm key at …").

### 7.4 Timing

| Parameter | Value |
|---|---|
| heartbeat interval | 5 s |
| node offline after no heartbeat | 20 s (monotonic clock) |
| tasks of an offline node requeued (`lost`) | 60 s offline |
| task assigned but absent from node's `running_tasks` | 20 s after assignment → requeued (`lost`, no attempt) |
| hive recovery window after restart | 80 s: restored assignments are kept unconfirmed and not redispatched |
| task deadline (hive) | node-reported `run_s` > `timeout_s` + 60, or no `xfer_bytes` progress for 5 min while fetching/uploading |
| claim long-poll max | 30 s (node HTTP timeout = wait_s + 15 s) |
| nonce TTL | 60 s |
| action expiry | 5 min unacked |

## 8. Scheduling

### 8.1 Jobs and tasks

* A **job** is a normalized `JobSpec` (`proto.ApplyJobDefaults`, then
  `proto.ValidateJobSpec`) plus `count` tasks. Job and task IDs, and leases,
  are random (`j`/`t` + 8 random bytes hex; leases 16 bytes); they are never counters.
  Jobs get a persisted, monotonically increasing `Seq`, which defines queue
  order, listings and retention. Wall clock times are for display only.
* Task records are created lazily at first dispatch. A job keeps `next_index`
  plus a `requeued` list, and `TaskCounts` are maintained counters. The claim
  walk iterates jobs in queue order (priority desc, then Seq asc), so a claim
  costs O(jobs).
* Task `i` gets `SAVIOR_TASK_INDEX`, `SAVIOR_TASK_COUNT`, `SAVIOR_JOB_ID`,
  `SAVIOR_TASK_ID` and `SAVIOR_ATTEMPT`. `{{index}}` and `{{count}}` are expanded
  in command args and the script (`proto.ExpandTemplate`).
* Defaults: cores 1, mem 128 MB, disk 64 MB, timeout 3600 s, retries 1,
  count 1, isolation full.

### 8.2 Claim and fit

A node claims when not draining/paused/pending and free cores ≥ 0.1. The
hive computes free = min(req.Free, Total − Allocated) and, for each job in
queue order, dispatches tasks that fit (`Resources.Fits(need, ScratchInRAM)`, and with
disk scratch also `need.disk_mb ≤ free.disk_mb`) and whose requirements
match. Requirements are:
* `arch` (any of), `min_mem_mb` (node inventory total) and `cpu_flags` (all);
* `labels` (all equal, effective labels) and `nodes` (IDs);
* isolation: `full` needs `FullIsolation`;
* not in the task's `FailedNodes` unless no other online eligible node exists;
* never a requeued task the node still holds (lists or has not reported).
It dispatches at most `max` tasks. Every dispatch gets a new lease, `Attempt++`,
and a history entry. Before committing (including after a long-poll wakeup),
the hive re-checks under the mutex that the node is still online/eligible and
that the request context is alive.

**ClaimID idempotency:** the hive remembers each node's last ClaimID and the
tasks it returned. A repeated ClaimID whose tasks are still `assigned` to the
node gets the same tasks and leases.

**Reservation:** a pending head-of-queue task may fit some eligible node's
Total but no node's Free for 120 s. The hive then reserves the eligible
online node with the most free cores for it (`NodeView.ReservedFor`). While
reserved, that node receives only this task, once it fits. The reservation is
cleared on dispatch, offline or drain, or after 30 min. `TaskView.WaitReason`
explains waits: "no eligible node", "reserved for nX", "waiting for 4 cores".

**Warnings:** a job whose tasks fit no known node (cores, memory, disk and
requirements together) gets `JobView.Warning`. It stays queued, since nodes may join later.

### 8.3 States and retries

* Task states: `pending → assigned → running → succeeded | failed | canceled`.
* `failed` reports carry an `ErrorKind`. `exit` and `timeout` consume an attempt:
  the task fails for good at `Failures == 1 + retries`. `input`, `sandbox`,
  `output` and `internal` are **node errors**: the task is requeued, the node
  is added to `FailedNodes`, and `NodeErrors++`.
* `preempted` (battery below minimum, admin) and `lost` (the hive found the
  node gone, or the task missing from `running_tasks`) requeue without
  consuming an attempt. A task that was never seen in the node's
  `running_tasks` doesn't count as an interruption.
* Hard cap: a task fails with "too many interruptions" when
  `Attempt ≥ 1 + retries + MaxInterruptions`.
* **Quarantine:** a node is quarantined (no tasks, `NodeView.Quarantine`
  reason) until `NodePatch.ClearQuarantine` when, within 10 minutes, ≥ 3
  tasks got a node error on it, or ≥ 5 distinct tasks failed on it within
  10 s of starting, *and those tasks then succeeded on another node*. Both
  rules need evidence against the machine: a job that fails everywhere (a
  missing input URL, a broken command) must not quarantine the swarm. The
  evidence is kept in memory only; a hive restart forgets it.
* **Usage bounds:** reported `run_s` is capped at `max(timeout_s,1) × 10 +
  3600` and `cpu_seconds` at `run_s × cores × 1.1 + 1`; negative or NaN
  values become 0.
* **Job state:** `queued` (nothing dispatched), `running`, `succeeded` (all
  succeeded), `failed` (all terminal, ≥1 failed), `canceled`.

### 8.4 Leases and reconciliation (level-triggered)

* Every report and log upload carries the lease. The hive accepts it only if
  `task.lease == lease && task.node == caller`. A terminal report for the lease
  that already finalized the task, with the same state, is answered 200
  (idempotent replay). Anything else gets 409 `stale lease`, and the node
  discards the work.
* On every heartbeat the hive compares the node's `running_tasks` with its
  assignments:
  * an entry whose (id, lease) is not the node's live assignment, is
    cancel-requested, or is past its deadline goes into `CancelTasks`. This
    repeats on every heartbeat until the node stops listing it;
  * a task assigned to the node more than 20 s ago and not listed is
    requeued as `lost`.
* **Cancel:** pending tasks become `canceled` immediately. Assigned and running
  ones get a hidden `cancel_requested` flag and are shown as `canceled`, but
  their resources stay charged to the node until it stops listing them or
  goes offline. A terminal report for a cancel-requested lease is accepted
  (200) and records cpu time and outputs without changing the state.
* **Timeouts:** `timeout_s` counts only unfrozen process time (`run_s`). The
  node kills at `timeout_s` and reports `failed/timeout`. The hive fails the
  task and cancels it through the same path when `run_s > timeout_s + 60` or
  when transfer progress stalls for 5 minutes.
* **Node side:** a task enters the node's `running_tasks` as soon as the claim
  response is decoded (phase `fetching`) and leaves only after its final
  report gets a 2xx or 409. Final reports go through an in-memory outbox,
  retried with backoff and re-sent after re-registration.

### 8.5 Logs, outputs, blobs

* **Logs:** the node posts combined stdout+stderr chunks at most every 2 s
  (`?lease&offset`). The hive keeps a 1 MiB in-memory ring per running task.
  When the task finishes, the last 64 KiB goes to `<hive_data>/logs/<task_id>.log`.
  Logs are never stored in `state.json`. Offsets count every byte ever written.
* **Outputs:** collected only after the task's cgroup is empty. Patterns
  (`proto.ValidOutputPattern`) are matched via `os.Root` on the workdir.
  Every match must be a regular file (checked with `Lstat`). It is opened with
  `O_NOFOLLOW|O_NONBLOCK` and must satisfy `S_ISREG`, owner = the task uid (or an
  input the agent wrote) and `nlink == 1`. It is hashed and uploaded from that
  fd. At most 1000 files and 1 GiB total. The hive rejects a report whose output
  names fail `ValidRelPath`, or whose blobs are missing (treated as a node
  error, `output`).
* **Blob GC roots:** inputs of retained jobs, outputs of retained tasks, every
  node's display spec, every wall's content, and blobs touched in the last hour
  (monotonic time). GC removes only unreferenced, untouched blobs. `DELETE`
  of a referenced blob is 409. `BlobInfo.LastTouched` is updated on PUT/GET.
* **Retention:** at most 500 finished jobs and at most 200k task records.
  Finished jobs are deleted in the order they finished (`DoneSeq`, a
  persisted counter, then `Seq`). The job that just finished is never the
  one deleted. Deleting a job removes the logs of every task that ran.

### 8.6 Hive restart

State persists assignments (node, lease, attempt). On startup, assigned and
running tasks are restored as **unconfirmed** and not redispatched during the
80 s recovery window. Nodes re-register with `RunningTasks`. The hive
re-adopts entries whose lease matches (returned in `AdoptedTasks`) and
cancels all others. When the window closes, unconfirmed tasks still unclaimed
are requeued as `lost`. Liveness timers are in-process monotonic: after a
restart every node starts `offline` until its first heartbeat.

## 9. Hive internals

* In-memory state behind one mutex. Structural changes (jobs, tasks, walls,
  node records, blobs) mark the state dirty. Liveness fields (last_seen,
  metrics, inventory) are persisted at most every 60 s. The snapshot is copied
  under the lock and serialized and written outside it: `state.json.tmp` is
  written, fsynced and renamed, and the previous file is kept as
  `state.json.prev`. If `state.json` fails to parse, `.prev` is loaded. The
  minimum interval between writes is max(2 s, 10× the last write duration),
  and 30 s when hive_data is in RAM or on vfat. `POST /admin/jobs` and wall or
  node patches are written synchronously before responding.
* Blobs: `<hive_data>/blobs/<2 hex>/<sha256>`. Max 8 GiB (4 GiB − 1 on vfat).
  Uploads stream to a temp file while hashing, and a hash mismatch is a 400.
  Derived images for the render endpoint are cached in `<hive_data>/cache/`
  (LRU, 256 MiB).
* Long-poll: claims wait on a broadcast channel that is closed and replaced
  when tasks become pending or node capacity may have changed.
* Background loop (1 s): liveness, lost/offline requeue, deadlines,
  reservations, recovery window, action expiry, session expiry, persistence.
* Walls: saving computes each cell's `WallTile` (canvas geometry in mm, see
  `proto.WallSpec` doc) and assigns it to the node's display spec, bumping rev.
  Rules: nodes must exist and have the display role, and a node can be in only one wall.
  Deleting a wall resets its nodes to `status`.
* Node records: ID, name, config labels, admin labels, roles, display spec,
  display rotate, drain, approved, quarantine, HWIDs, first/last seen, last
  inventory, BootID. Names are unique. A new ID that shares a HWID with an
  offline record takes that record over (name, labels, display, wall cell).
* Clock: the hive tracks `TimeSynced`/`TimeSource`. On Linux as root with
  `TimeSynced=false`, an authenticated admin request carrying
  `X-Savior-Client-Time` that differs by more than 60 s sets the clock
  (`TimeSource=admin`, plus `hwclock -w` when available). NTP sync is detected
  via `adjtimex` status (`STA_UNSYNC` clear).
* On a SaviorOS hive, the status screen (11.3) shows hive URLs, the
  fingerprint, node count and a pairing code. `HiveInfo.Persistent=false`
  shows as a banner.

## 10. Node agent internals

### 10.1 Identity

`hwinfo.GetIdentity(root) (Identity, error)` returns the node ID, the
identity MAC (if any) and all HWIDs. Source order for the ID:

1. PCI NICs, wired first, sorted by PCI address (basename of
   `/sys/class/net/X/device`), taking the first whose `addr_assign_type == 0`
   and whose MAC is not locally administered, zero or broadcast;
2. a valid DMI `product_uuid`: not all-zero, not all-F, not the placeholder
   `03000200-0400-0500-0006-000700080009`;
3. DMI `product_serial`/`board_serial`, excluding placeholders ("To Be Filled By
   O.E.M.", "System Serial Number", "Default string", "0123456789", "None", "Not Specified");
4. a USB NIC MAC;
5. random (logged as unstable).

A NIC is eligible only if `addr_assign_type` is readable and 0 and its MAC is
unicast and globally administered. QEMU's default `52:54:00` MACs are locally
administered, so VMs need `-uuid` to get a stable ID.
`node_id = "n" + first 12 hex of SHA-256(source string)`. HWIDs lists every
candidate (`mac:`, `uuid:`, `serial:`). `BootID = /proc/sys/kernel/random/boot_id`
+ ":" + a random agent-start nonce (a respawned agent is a new session).

### 10.2 Clock

Nodes compute `offset = hive_time − local_time` (RTT/2 corrected) only from
verified responses. They step the system clock (root on Linux) only when
`|offset| > 5 s` and either the hive reports `TimeSynced`, or the local clock is
before BuildTime. They never step it when their own ntpd has synced.
Otherwise the offset is used only for wall and slideshow sync.

### 10.3 Capacity

`budget_mb = MemAvailable (at agent start) − 48 (agent reserve) − display
reserve (3 × fb bytes + image cache cap, when the display role is active)`.
`total.mem_mb = budget_mb × max_mem_percent / 100`. `total.cores =
logical_cpus × max_cpu_percent / 100`, rounded down to 0.05, min 0.1. This is
enforced by a parent cgroup `<CgroupRoot>/tasks` with
`cpu.max = total.cores × 100000 100000`. With `scratch=ram`,
`ScratchInRAM=true`, `total.disk_mb = 0` and disk is charged to memory. With
disk scratch, `total.disk_mb = free space × 90%`. Free = total − Σ running
task resources. When the power policy says no, free is forced to 0.

### 10.4 Power and thermal policy (`internal/power`, pure)

```go
type Policy struct { RunOnBattery bool; BatteryMinPercent int; MaxTempC float64 }
type Input struct { Metrics proto.Metrics; ExternalDisplay bool; MemBudgetMB int }
type Decision struct { Accept, Pause, Preempt, BlankDisplay bool; Reason string; Since time.Time /* + hysteresis state */ }
func Evaluate(p Policy, in Input, prev Decision) Decision
```
* `limit = m.CPUTempLimitC` (hwinfo computes min(max_temp_c, sensor max, crit − 5)).
  Pause when `CPUTempC ≥ limit`, or when `ThrottleEvents` rose in 3
  consecutive samples. Resume at `limit − 10` with no new throttle events.
* On battery and not `RunOnBattery`: Accept = false. On battery and below the
  minimum: Preempt, and work is accepted again only from minimum + 5% (or on
  AC). A battery below 20% health counts as absent for Preempt.
* `LidClosed` with no connected external connector sets BlankDisplay; compute
  is unaffected.
* Memory pressure (`MemPressure ≥ 30`) or `SwapUsedMB > budget/2` sets Accept = false.

hwinfo metrics rules (normative): the CPU temperature comes from hwmon
`coretemp`, `k8temp`, `k10temp` and `via_cputemp` `temp*_input`, else `acpitz`
thermal zones. Readings ≤ 0 °C, ≥ 125 °C, or unchanged for 10 minutes are
ignored, and drivetemp, GPU and Wi-Fi sensors are never used. On AMD k10temp,
the limit uses `temp1_max` if present. `ThrottleEvents` is the sum of
`/sys/devices/system/cpu/cpu*/thermal_throttle/*_throttle_count`.
`OnBattery` requires a present system battery (type Battery, not
scope=Device). Then, if any `Mains` supply exists, it's true only when all of
them report `online=0`; otherwise only when a battery is `Discharging`. The percentage
comes from `capacity`, else `energy_now/energy_full`, else `charge_now/charge_full`.
Health = `energy_full/energy_full_design`.

### 10.5 Main loop

Goroutines run discovery and registration, the heartbeat, the claim loop,
task workers, the display controller, and the power monitor. Claim
backoff grows exponentially from 1 s to 30 s. Actions: identify is acked
when it starts, and reboot or poweroff are acked in a heartbeat before being
executed. Each action ID runs at most once per agent process.

At start, before registering, the agent kills and removes any leftover
`<CgroupRoot>/task-*` cgroups (`cgroup.kill`), SIGKILLs every process whose
real, effective or saved uid is a slot uid (via pidfd, bounded at 5 s) and
wipes `<WorkRoot>/*`. The claim loop never asks for more tasks than the
runner has free slots. Power pause and preempt are level-triggered: they
are applied to every running task on every tick while they hold. On
shutdown all tasks share one 10 s grace period. It
writes `/run/savior/status.json` (0644, atomic) every heartbeat for `savior console`.

### 10.6 Link state and console

The agent tracks a `proto.HiveLink` state plus the hive address and last
error. `savior console` (tty1) and the status scene show it with a one-line
fix, for example:

* `unreachable`: "Hive found at 192.168.1.20 but port 7700 is blocked. Allow savior through that computer's firewall."
* `key_mismatch`: "A hive is on the network but its swarm key differs. Check swarm_key in savior.conf."
* `no_swarm_key`: "Put swarm_key = ... in savior.conf on the stick."

## 11. Display subsystem

### 11.1 Device selection and fbdev driver

`display_device=auto` picks the first `/dev/fbN` whose DRM card has a
connector with `status=connected`, else the first fb. The controller polls
`/sys/class/graphics` and `/sys/class/drm` every 2 s. It reopens when fb0's
device or `/sys/class/graphics/fb0/name` changes, or when an ioctl or write
returns ENODEV, because a KMS driver replaces simpledrm/efifb after boot.

Open sequence:
1. `FBIOGET_VSCREENINFO`/`FSCREENINFO`. Require `type == FB_TYPE_PACKED_PIXELS`,
   visual TRUECOLOR or DIRECTCOLOR, `grayscale == 0` and bpp in {16, 24, 32};
   anything else sets `DisplayState.Error`. For DIRECTCOLOR, load identity
   ramps with `FBIOPUTCMAP`.
2. Try `FBIOPAN_DISPLAY` to offset 0,0; if that fails, honor the current
   `xoffset`/`yoffset`.
3. mmap `PAGE_ALIGN((smem_start & (PAGE_SIZE-1)) + smem_len)`. The pixel (x,y)
   is at `(smem_start & (PAGE_SIZE-1)) + (y+yoffset)*line_length + (x+xoffset)*Bpp`.
   If mmap fails, use pwrite at the same offsets.
4. Formats come from the bitfields: XRGB8888, XBGR8888, RGB888, BGR888,
   RGB565 and XRGB1555 have fast paths, and a generic bitfield path handles the rest.

`Device.Show(img *image.RGBA, dirty []image.Rectangle)` converts into a RAM
shadow in device format (stride = line_length). Rotation is applied during
this conversion, tile-wise. Only whole changed rows are copied into the
mapping with `copy()`, and the mapping is never read. Blanking falls back in
order: `FBIOBLANK`, then backlight (`bl_power`, else `brightness=0`, saved and
restored), then a black frame. `DisplayState.BlankMethod` says which one worked.

### 11.2 VT handling

The controller opens a dedicated VT (`/dev/tty7`), then runs `VT_ACTIVATE` +
`VT_WAITACTIVE`, `KDSETMODE KD_GRAPHICS`, and
`VT_SETMODE{VT_PROCESS, relsig=SIGUSR1, acqsig=SIGUSR2}`. On SIGUSR1 it stops
writing and answers `VT_RELDISP 1`. On SIGUSR2 it answers
`VT_RELDISP VT_ACKACQ`, invalidates the shadow and repaints. Keys: Alt+F1 =
text status, Alt+F2 = shell (when enabled), Alt+F7 = display. When the agent
exits, `/usr/libexec/savior/run` calls `savior display vt-reset` (KD_TEXT,
VT_AUTO). `DisplayState.Foreground` reports whether the display VT is active.

### 11.3 Scenes

The modes are those of `proto.DisplayModes`:
* `status`: the default and the "which machine is this?" screen. Shows the
  node name, short code, IPs, roles, link state with its fix hint, and
  CPU/RAM/temp/battery bars. On a hive it adds a panel with the hive URLs,
  fingerprint, pairing code and node count.
* `off`: blank.
* `color`: solid `bg`.
* `text`: auto-sized, word-wrapped, centered, optional `title`.
* `clock`: time in `clock_format` (Go layout, default `15:04`) and the date.
  Uses `timezone` (spec, else config).
* `image`: the image fitted with `fit` (default `contain`).
* `slideshow`: the image index is `floor(hive_time_unix / interval) mod len`.
  The next slide is pre-rendered from `boundary − interval/2`, and at the
  boundary the prepared frame is only copied.
* `dashboard`: `SwarmStats` refreshed every 5 s.
* `wall`: `WallTile.Content` fitted into the canvas; this node shows the
  canvas rect (X, Y, W, H) scaled to its whole screen. The full canvas is
  never allocated. `test` content draws a canvas-wide calibration pattern
  (grid, diagonals, circles, and `Label` per tile).
* `test`: color bars, gradients, a 1-pixel border, the resolution and the format.
* `identify` (action): the huge short code, name and wall position on a
  flashing background, over the current scene, for N seconds. It also
  unblanks the screen, blinks the keyboard LEDs (`KDSETLED`) and beeps (`KIOCSOUND`).

Rotation (0/90/180/270, clockwise: with 90 the logical top-left is shown at
the physical top-right) is `Directives.DisplayRotate` and applies under every scene.
Lid closed (with no external display) or the idle timer (local default modes
only) blanks the screen. Any `/dev/input/event*` activity or an identify
wakes it.

### 11.4 Rendering API and performance

```go
type Device interface {
    Size() (w, h int)                                         // physical fb size
    Show(img *image.RGBA, dirty []image.Rectangle) error      // img is logical (rotated) size
    Blank(on bool) (method string, err error)
    Info() proto.DisplayState                                 // format, driver, fb size
    Close() error
}
func OpenFramebuffer(path string, rotate int) (Device, error) // linux
func NewPNGDevice(path string, w, h, rotate int) Device       // tests/headless
func NewMemDevice(w, h, bpp int, rotate int) *MemDevice       // tests: exposes device-format bytes

type Env struct {
    Now      func() time.Time                  // hive-adjusted clock
    Status   func() StatusInfo
    Stats    func(ctx context.Context) (*proto.SwarmStats, error)
    Fetch    func(ctx context.Context, m proto.Media, req FetchRequest) (image.Image, error)
    Location *time.Location
}
type FetchRequest struct { CanvasW, CanvasH int; Rect image.Rectangle; PixelW, PixelH int; Fit string }
type StatusInfo struct { Name, ShortCode, NodeID, Version string; Link proto.HiveLink; HiveAddr, HiveError, Message string;
    Addrs []string; Roles []proto.Role; Metrics proto.Metrics; Inventory proto.Inventory; RunningTasks int;
    PowerReason string; Hive *HivePanel }
type HivePanel struct { URLs []string; Fingerprint, PairCode string; NodesOnline int; Persistent bool }

// Render draws one frame of spec into dst and returns when the scene next changes.
func Render(ctx context.Context, spec proto.DisplaySpec, dst *image.RGBA, env Env) (next time.Time, err error)
func NewController(dev func() (Device, error), env Env, log *slog.Logger) *Controller
func (c *Controller) Apply(spec proto.DisplaySpec)
func (c *Controller) SetRotate(deg int)
func (c *Controller) Identify(d time.Duration, code, label string)
func (c *Controller) SetBlank(reason string, on bool)   // lid/idle policy
func (c *Controller) State() proto.DisplayState
func (c *Controller) Run(ctx context.Context) error
```
* Frames render into preallocated buffers and are not re-rendered until
  `next` or a spec change. Clocks without seconds redraw once a minute.
* Fonts are the embedded Go fonts, read by a small bounds-checked TrueType
  parser (x/image/font/opentype would pull in golang.org/x/text) and
  rasterized with x/image/vector. Glyph masks are cached by (face, size,
  rune) within 4 MiB, and static layers are cached too. Glyphs over 512 px
  are drawn in tiles straight into the frame.
* No float scalers on `GOARCH=386`. Scaling is integer: nearest neighbor for
  upscaling, a fixed-point box filter for downscaling. Each media item is
  scaled once to its final size.
* Every frame render is wrapped in `recover()`, so a panic becomes
  `DisplayState.Error`.
* Budget: steady-state status/clock under 5% of a 1 GHz core.

### 11.5 Media

* Blob media: the node requests the hive render endpoint with its exact tile
  geometry, so the hive decodes and scales and the node gets a PNG of at most
  its screen size. If that fails, it falls back to local decode.
* URL media and local fallback: a separate `http.Client` with no auth header,
  system CA verification, http/https only, at most 3 redirects, and a
  64 MiB cap. If `Media.SHA256` is set it must match.
* Before decoding: `image.DecodeConfig`. Reject if either side exceeds 8192
  or the image exceeds 16 MP, or if w×h×8 exceeds 25% of MemAvailable.
  Rejections become `DisplayState.MediaErrors`, never a crash. After decode,
  crop to the needed source rectangle, scale to the final size and discard
  the original.
* Cache: scaled frames only, within min(64 MiB, 10% of MemTotal). Compressed
  bytes are cached on tmpfs (32 MiB) so slideshows don't refetch.
* Formats: png, jpeg, gif (first frame), bmp and webp.

## 12. Task runner

```go
type Transfer interface {
    FetchBlob(ctx context.Context, sha256 string, w io.Writer) (int64, error)
    FetchURL(ctx context.Context, url string, maxBytes int64, w io.Writer) (int64, error)
    UploadBlob(ctx context.Context, sha256 string, size int64, r io.Reader) error
}
type Config struct {
    WorkRoot, CacheDir, CgroupRoot, SelfExe string
    Sandbox string            // strict | auto | none
    ScratchInRAM bool
    UIDBase int               // 10000
    Slots int                 // max concurrent tasks (= uid slots)
    Log *slog.Logger
}
func New(cfg Config, tr Transfer) (*Runner, error)            // probes caps; cleans leftovers
func (r *Runner) Caps() (mode string, caps []string, full bool)
func (r *Runner) Run(ctx context.Context, t proto.Task, logs io.Writer, progress func(proto.RunningTask)) proto.TaskReport
func (r *Runner) Freeze(lease string, frozen bool) error
func (r *Runner) Preempt(lease string) error                 // kill ⇒ report preempted
func SandboxExecMain(args []string) int
```
Flow per task (keyed by lease; the workdir name is `<task_id>.<attempt>`):
1. `WorkRoot` is root:root 0711. It is opened once as an `os.Root`, and the
   task dir is created with `Root.Mkdir` (it must not exist) and
   chowned/chmodded through the fd.
2. With `ScratchInRAM`, mount a tmpfs `size=<disk_mb>m,mode=0700,uid,gid` on
   the task dir, so the quota is enforced by ENOSPC.
3. Fetch inputs into the blob cache (root 0700) and verify the hash. URL
   inputs stop at `Size`. Each input is written into the task dir with
   `O_CREATE|O_EXCL|O_NOFOLLOW`, and the exec bit is set via the fd. The
   phase is `fetching` and xfer_bytes are reported.
4. Create cgroup `<CgroupRoot>/tasks/task-<id>.<attempt>` with `cpu.max` (cores),
   `memory.max` (mem_mb, plus disk_mb when ScratchInRAM), `memory.swap.max=0`,
   `memory.oom.group=1` and `pids.max=1024`.
5. Start the sandbox (6.5). The script body is written to
   `/work/.savior-script` and runs as `/bin/sh /work/.savior-script`. The
   environment is only `PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`,
   `HOME=/work`, `TMPDIR=/tmp`, `LANG=C.UTF-8`, the `SAVIOR_*` vars and the spec's `env`.
6. Stream combined output to `logs`. Enforce `timeout_s` on unfrozen time
   (`Freeze` stops the clock), then `cgroup.kill` and wait for
   `cgroup.events populated 0`. The output pipes get 2 s to drain after that.
   Without a cgroup, a process that left the group and still holds them is
   killed by its slot uid, and the slot is retired only if that fails. A
   preempt that arrives before the process starts means it never starts.
   A task killed by a signal failed with exit code 128 + signal.
7. Collect outputs (8.5) only from a task that ran to its end (not canceled,
   preempted or failed in setup), upload them (phase `uploading`), and record
   `cpu.stat usage_usec` and `memory.peak`.
8. Clean up: unmount and remove the workdir and remove the cgroup.

## 13. Boot and OS integration

### 13.1 Boot media (`os/image/mkimage.sh`, verified in QEMU)

* **`savior.img`:** an MBR disk. GRUB `boot.img` sits in the MBR, the GRUB
  `i386-pc` core image in sectors 1-2047, and one FAT32 partition (0x0C,
  bootable, at 1 MiB, label `SAVIOR`, volume serial derived from the build ID).
  The partition holds `/EFI/BOOT/{BOOTX64.EFI,BOOTIA32.EFI}`,
  `/boot/grub/grub.cfg`, `/boot/<arch>/{vmlinuz,initrd}`,
  `/boot/savior-<id>.id` (marker), optionally `/boot/savior-conf.cpio`,
  `/savior.conf`, `/boot-options.cfg` and `/README.txt`, plus `/firmware/`
  (user-provided firmware) and `/boot/netboot/` (PXE images, used by a hive
  with `netboot=yes`).
* **`savior.iso`:** El Torito BIOS (`i386-pc-eltorito`) + UEFI (`efi.img` with
  both EFI loaders). It has a hybrid MBR and a GPT EFI partition, so it also
  boots when written to a USB stick. Volume ID `SAVIOR`.
* **`netboot/`:** `grub-mknetdir` images for i386-pc, x86_64-efi and
  i386-efi. The payloads are fetched over TFTP, or over HTTP from the hive's port 7702 when served by a hive.
* **Payload selection** is baked in at build time; there are no runtime file
  tests (they fail over TFTP). With both payloads: `cpuid -l` → x86_64, else
  i686, on every platform (the x86_64 kernel has `EFI_MIXED`, so 32-bit UEFI
  on a 64-bit CPU boots it).
* **Early config** (embedded in every core image; it runs under GRUB's rescue
  parser, which has no `if`): `search --no-floppy --file --set=root
  --hint=$root <marker>`, so the boot device wins when it has the marker and
  other disks are scanned otherwise. Then `configfile /boot/grub/grub.cfg`,
  which prints how the medium was found.
* **Menu** (timeout 5):
  * default, with `gfxpayload=1024x768x32,1024x768x16,800x600x32,800x600x16,auto`
    on BIOS so firmware framebuffers exist;
  * safe graphics (`nomodeset`, firmware framebuffer);
  * safe mode (`noapic nolapic acpi=off irqpoll nomodeset`);
  * text only (`gfxpayload=text`);
  * display only;
  * start as hive (`savior.roles=auto,hive`);
  * rescue shell (verbose, `console_shell`);
  * force 32-bit (universal images);
  * reboot and power off.
  If the file `/boot-options.cfg` exists, it is sourced first, so an operator can set
  `savior_args="video=LVDS-1:d"` from any computer. Every entry appends
  `$savior_args` and `savior.media=UUID=<FAT volume serial>` (USB),
  `savior.media=UUID=<ISO volume UUID, YYYY-MM-DD-hh-mm-ss-cc>` (ISO, set with
  xorriso `--modification-date`) or `savior.media=none` (netboot). The
  rescue entry has its own verbose base command line.
* With `--conf`, `mkimage` also bakes `boot/savior-conf.cpio`
  (`etc/savior/baked.conf`, 0600) as a second initrd on USB and ISO media,
  never in the netboot tree.
* **Kernel cmdline:** `consoleblank=0 quiet loglevel=3 console=ttyS0,115200 console=tty0`.

### 13.2 Payload (initramfs) and memory budget

* The initramfs is a newc cpio compressed with `xz --check=crc32` (CRC64
  panics the kernel's decompressor).
* `/lib/modules` and `/lib/firmware` ship as an xz squashfs
  (`/lib/modloop.sqfs`), loop-mounted by `S00mounts` and read-only. They stay
  compressed in RAM, and their pages are reclaimable.
* Budgets, enforced by `make image` and CI:

  | | compressed initrd | unpacked rootfs (excl. modloop) |
  |---|---|---|
  | i686 | ≤ 24 MiB | ≤ 48 MiB |
  | x86_64 | ≤ 32 MiB | ≤ 64 MiB |

  Target: MemAvailable ≥ 150 MB at idle with the display role on a 256 MB machine.
* Firmware allowlist: Intel NIC and Wi-Fi (iwlwifi older generations), Realtek
  NIC (`rtl_nic`), Atheros (ar9170, ar3k), Ralink (rt2x00), radeon (R100–SI).
  No amdgpu, no nvidia GSP. ipw2x00 and b43 are "bring your own firmware" via
  `/firmware/` on the stick.
* The x86_64 payload uses the `GOAMD64=v1` binary. The i686 payload carries both 386 builds.

### 13.3 Init sequence (BusyBox init)

The kernel starts the overlay's `/init`. The kernel's initramfs root can't
be moved by `pivot_root(2)` (EINVAL), which the task sandbox needs, so
`/init` bind-mounts `/` onto `/mnt`, moves it to `/`, chroots into it and
execs `/sbin/init`. Nothing is copied. If a step fails it execs
`/sbin/init` directly (tasks then fail with a sandbox error).

`/etc/inittab` (os/rootfs-overlay):
```
::sysinit:/etc/init.d/rcS
tty1::respawn:/usr/libexec/savior/run console
tty2::respawn:/usr/libexec/savior/tty2          (shell if console_shell=yes, else a notice)
::respawn:/usr/libexec/savior/run node
::respawn:/usr/libexec/savior/run hive          (exec sleep forever unless role hive)
::ctrlaltdel:/sbin/reboot
::shutdown:/etc/init.d/rcK
```
`rcS` sets `umask 077` and runs only `/etc/init.d/S??*` scripts from the
allowlist below; the Buildroot post-build deletes all others. Only fast,
mandatory steps block (mounts, mdev, config, network start). Slow steps run in the
background (`start-stop-daemon -b`) with progress on tty1.

| Script | Job |
|---|---|
| `S00mounts` | mount proc, sys, devtmpfs, /run, /tmp, devpts, cgroup2 (enable cpu, memory, pids, freezer in the root and in `/sys/fs/cgroup/savior`); loop-mount the modloop; pick the savior 386 build (`savior storage pick-binary`) |
| `S05mdev` | mdev hotplug + coldplug (modalias → modprobe); blacklist `p4-clockmod` |
| `S08config` | `savior storage find-media`: wait up to 20 s for the medium named by `savior.media=` (removable/USB/sr devices first, fixed disks last), else LABEL=SAVIOR, else any removable vfat/iso9660 with `/savior.conf` (so a CD-booted machine can take its config from a plain USB stick). Mount read-only with the 6.5 options, set `firmware_class.path=/media/savior/firmware` and re-probe Wi-Fi drivers without a netdev, merge config (5.1), write `/run/savior/{savior.conf,env}`, print "no savior.conf found" on tty1 when nothing turns up |
| `S10system` | hostname, timezone, sysctls (6.5), cpufreq governor, zram swap (50% of RAM, lz4 or zstd) |
| `S20scratch` | task scratch (`scratch_wipe` honored; ext4 via mke2fs when available, else busybox mke2fs) — background if formatting |
| `S30network` | lo; wired interfaces via udhcpc (`-s /usr/libexec/savior/udhcpc.script`, background) or static (then any other wired NICs use DHCP, so a hive can have an uplink; the static gateway and DNS win); Wi-Fi via wpa_supplicant; resolv.conf; link-local fallback |
| `S35dhcpd` | udhcpd when `dhcp_server=yes` and not `netboot=yes` |
| `S40time` | ntpd if present and `ntp` isn't off (optional) |
| `S50sshd` | dropbear (`-R`, ed25519) when `ssh_key` is set; background |
| `S65netboot` | dnsmasq when `netboot=yes` (hive role): proxy-DHCP with a PXE menu, or with `dhcp_server=yes` full DHCP with the boot file chosen by client architecture (0 → `i386-pc/core.0`, 7/9 → `x86_64-efi/core.efi`, 6 → `i386-efi/core.efi`; UEFI firmware ignores PXE menus). TFTP root `/run/savior/tftp`: world-readable RAM copies of the GRUB network images (dnsmasq serves only world-readable files) and a generated `grub.cfg` that loads `(http,<hive ip>:7702)/boot/<arch>/{vmlinuz,initrd}` (served by the hive from the stick; faster than TFTP), retries 5 times and then reboots, with `savior.hive=<ip>:7700 savior.hive_fingerprint=... savior.join=keyless savior.media=none`. Never the key, `savior.conf` or `savior-conf.cpio` |

A hive writes its status panel (URLs, fingerprint, pairing code, nodes
online, persistence) to `/run/savior/hive-status.json` (0600). The node's
status scene and `savior console` read it.

`/usr/libexec/savior/run <node|hive|console>` sources the env and runs
`savior <svc> --config /run/savior/savior.conf --log-file /var/log/savior-<svc>.log`
as a child, logs its exit status, and sleeps 5 s when it ran for less than
10 s (so a crashing agent restarts at most every 5 s). For `hive` without the
hive role it runs `exec sleep 2147483647`. For `node` it runs `savior display
vt-reset` before and after (restore the console after a crash).

Test support: with `console=ttyS0` on the command line, rcS prints
`SAVIOR-BOOT: rcS start` and `/usr/libexec/savior/boot-report` prints
`SAVIOR-BOOT: rcS done ... cfg= media= key= fb= net= console= node= ver=` on
the serial console (never secrets). `savior_dumplog=1` also copies the boot
log and syslog there.

### 13.4 Hive data partition

On first start with the hive role and `hive_data=auto`, `savior storage init-data`:
1. finds the device holding the booted SAVIOR partition;
2. if it has exactly one MBR partition and ≥ 256 MiB unallocated after it,
   appends partition 2 (type 0x83, 1 MiB aligned, to the end of the device) by
   writing the 16-byte entry itself (fsync and read back), then `BLKRRPART`,
   or `BLKPG` when a partition of the disk is mounted (the stick always is);
3. formats it with `mke2fs -t ext4 -L SAVIOR-DATA` (or ext2 with busybox mke2fs);
4. mounts it at `/var/lib/savior/data`. The hive keeps its state in
   `/var/lib/savior/data/hive`.

The FAT boot partition is never written at runtime. Without a data
partition the hive runs from RAM with `Persistent=false`.

### 13.5 Users

`root` (password locked). Task uids 10000..10000+slots-1 have no passwd
entries on the host; the sandbox `/etc/passwd` lists `savior-job:x:<uid>:<uid>::/work:/bin/sh`.

## 14. Build system

* Top-level `make`: `build` (host binary), `build-linux` (amd64 v1, 386 sse2,
  386 softfloat into `build/linux-*/savior`), `test`, `vet`, `fmt-check`,
  `lint-sh` (shellcheck), `dev-image`, `dev-test` (QEMU), `image ARCH=x86_64|i686`
  (Buildroot), `universal-image`, `clean`.
* **Buildroot** (`os/buildroot`, BR2_EXTERNAL name `SAVIOR`): a pinned
  release; defconfigs `savior_x86_64_defconfig` (generic x86-64) and
  `savior_i686_defconfig` (`BR2_x86_i686`); an internal musl toolchain; BusyBox
  init with mdev; `BR2_TARGET_ROOTFS_CPIO=y` + `CPIO_XZ` (`INITRAMFS` unset).
  Packages: busybox (config fragment enabling mdev, udhcpc, udhcpd, ntpd,
  zcip, findfs, blkid, mkfs.vfat, fdisk), dropbear, wpa_supplicant, iw,
  wireless-regdb, linux-firmware (allowlist), dnsmasq, e2fsprogs (mke2fs),
  squashfs (host), ca-certificates.
  The post-build step installs the prebuilt `savior` binaries, deletes any
  non-allowlisted `/etc/init.d/S*`, builds the modloop squashfs, checks the
  budget and writes `/etc/savior-release`. Boot media are made by
  `os/image/mkimage.sh` from the Buildroot kernel and initrd, with host GRUB
  packages (the same GRUB verified in the QEMU tests).
* **Kernel:** arch defconfig + fragments in `board/savior/linux/`, with a
  required-symbols table in `board/savior/linux/required.txt`. That table is
  checked against the final `.config` by `scripts/check-kconfig.sh`, and
  `scripts/check-defconfig.sh` checks BR2 symbols after `olddefconfig`.
  Required on both arches:
  * base: `DEVTMPFS`, `TMPFS`, `BLK_DEV_INITRD`, `RD_XZ`, `RD_ZSTD`;
  * cgroups and namespaces: `CGROUPS`, `MEMCG`, `CGROUP_PIDS`,
    `CGROUP_FREEZER`, `CGROUP_SCHED`, `PID_NS`, `NET_NS`, `IPC_NS`, `UTS_NS`,
    `# CONFIG_USER_NS is not set`, `SECCOMP`, `SECCOMP_FILTER`;
  * memory and firmware: `SWAP`, `ZRAM`, `ZSMALLOC`, `CRYPTO_LZ4`, `PSI`,
    `SQUASHFS`, `SQUASHFS_XZ`, `BLK_DEV_LOOP`, `FW_LOADER_COMPRESS_XZ`,
    `# CONFIG_FW_LOADER_USER_HELPER is not set`;
  * filesystems: `VFAT_FS`, `NLS_CODEPAGE_437`, `NLS_ISO8859_1`, `NLS_UTF8`,
    `ISO9660_FS`, `JOLIET`, `EXT4_FS`;
  * storage (built in): `USB_UHCI_HCD`, `USB_OHCI_HCD`, `USB_EHCI_HCD`,
    `USB_XHCI_HCD`, `USB_STORAGE`, `USB_UAS`, `ATA_PIIX`, `ATA_GENERIC`,
    `BLK_DEV_SR`;
  * display: `FB`, `FB_DEVICE`, `DRM`, `DRM_FBDEV_EMULATION`,
    `SYSFB_SIMPLEFB`, `DRM_SIMPLEDRM`, `FRAMEBUFFER_CONSOLE`, `VT`,
    `VT_CONSOLE`, `BACKLIGHT_CLASS_DEVICE`, `ACPI_VIDEO`, `INPUT_EVDEV`,
    `INPUT_PCSPKR`;
  * ACPI, power and sensors: `ACPI_BUTTON`, `ACPI_AC`, `ACPI_BATTERY`,
    `ACPI_THERMAL`, `THERMAL_HWMON`, `SENSORS_CORETEMP`, `SENSORS_K8TEMP`,
    `SENSORS_K10TEMP`, `X86_ACPI_CPUFREQ`, `CPU_FREQ_GOV_ONDEMAND`,
    `CPU_FREQ_GOV_SCHEDUTIL`;
  * EFI: `EFI`, `EFI_STUB`;
  * modules: GPUs i915, radeon, nouveau, gma500, mgag200, ast, bochs,
    cirrus-qemu and virtio-gpu; NICs e100, e1000, e1000e, r8169, 8139too,
    sky2, tg3, b44, forcedeth, via-rhine, sis900, atl1, atl1c, atl1e, alx,
    virtio_net; Wi-Fi iwlegacy, iwlwifi, ipw2100, ipw2200, ath5k, ath9k,
    rt2800pci, rt2800usb, rtl8187, rtl8192ce, brcmsmac, b43; usbnet asix,
    ax88179, r8152, cdc_ether.
  No legacy fbdev drivers (sisfb, viafb, savagefb, rivafb). i686 adds
  `M686`, `X86_GENERIC`, `HIGHMEM4G`, `# CONFIG_X86_PAE is not set`, and
  cpufreq `X86_SPEEDSTEP_CENTRINO` and `X86_POWERNOW_K7`. x86_64 adds
  `EFI_MIXED`, CPU mitigations and `PAGE_TABLE_ISOLATION` (named `MITIGATION_PAGE_TABLE_ISOLATION` on 6.9+).
* **Dev image** (`os/dev/build.sh`): Ubuntu 24.04 `linux-image-unsigned`,
  `linux-modules` and `linux-modules-extra`, fetched with `apt-get download`. The
  modules come from the checked-in `os/dev/modules.txt` plus their dependency
  closure from `modules.dep`; `.ko.zst` files are decompressed and `depmod`
  is run. It also uses `busybox-static` (`/init` → busybox; applets symlinked),
  optionally dropbear and dnsmasq with their libraries, the same
  `os/rootfs-overlay`, and the savior binary. The cpio is compressed with
  `xz --check=crc32`.
* **QEMU tests** (`os/dev/qemu-test.sh`, TCG): SeaBIOS USB-EHCI stick (boot +
  fb0 present); OVMF x64; ISO under BIOS and UEFI; PXE via slirp TFTP; and a
  multi-VM swarm on a socket-multicast LAN with a random group/port per run
  and `localaddr=127.0.0.1`. Each VM gets a distinct MAC and `-uuid`. The
  swarm run (hive + 2 compute + 1 display) runs a job end to end, checks the
  display through `screendump`, and runs a duplicate-ID test. Buildroot CI
  adds `qemu-system-i386 -cpu pentium3,-pae -m 256` (join, a default job,
  16 bpp via the nomodeset entry, MemAvailable) and OVMF32 → i686.

## 15. Testing strategy

* Unit tests per package. `hwinfo` uses fixture trees under
  `internal/hwinfo/testdata/<machine>/` (ThinkPad T60, Pentium 4 desktop,
  Atom netbook, AMD k10temp desktop, QEMU). `display` has converter tests for
  every format with padded line_length and non-zero yoffset, plus render
  tests. `runner` has real-process tests with `sandbox=none`, and when run as
  root with cgroup2 it tests namespaces, cgroups and seccomp: a task must not
  be able to read `/proc/cmdline` or files outside `/work`, `unshare` must fail,
  and output symlinks must be refused.
* `internal/node` integration test: a real `hive.Server` over TLS plus 3
  in-process agents with fake hardware roots and memory display devices. It
  covers join, heartbeat, a count=5 job with outputs, cancellation, node loss →
  requeue, a stale-lease report being rejected, hive restart → re-adoption,
  display assignment, walls, a rejected wrong key, and a MITM (different cert
  between /hello and /register) that never gets a proof.
* shellcheck on all scripts. QEMU tests (14) in CI.
