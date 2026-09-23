# SaviorOS design specification

Status: v0.1 (initial architecture). This document is the authoritative contract
between the components in this repository. When code and this document disagree,
fix one of them in the same change.

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

1. **Run from RAM, touch nothing.** The whole OS is an initramfs. Internal disks
   are ignored unless the operator opts in, so dead or failing hard drives don't
   matter and the machine's original OS stays where it is.
2. **One stick, any machine.** A single hybrid USB image boots on legacy BIOS,
   64-bit UEFI and 32-bit UEFI, and picks a 64-bit or 32-bit payload based on
   the CPU. An ISO covers CD-only machines, and PXE netboot covers
   whole labs.
3. **Zero-config LAN join.** A node finds the hive by LAN broadcast. The only
   secret a node needs is the `swarm_key`, which the operator writes into a
   text file on the stick's FAT partition from any computer.
4. **Stateless nodes.** Node identity is derived from hardware. Everything a node
   needs to remember (name, labels, display assignment) is stored on the hive.
5. **Small and boring.** Linux kernel + BusyBox + a single static Go binary
   (`savior`) that holds every SaviorOS service. No systemd, no package manager
   on the node, no runtime dependencies.
6. **Respect old hardware.** Throttle on heat, stop taking work on battery,
   use compressed RAM swap (zram), support 16 bpp framebuffers and CPUs without SSE2.

Non-goals (v0.x): writing a kernel, running containers/OCI images, GPU compute,
general-purpose desktop use, multi-hive federation.

## 2. System overview

```
             +-----------------------------------------------+
             |                  HIVE (savior hive)            |
 operator -> |  HTTPS :7700  admin API + web dashboard        |
 savior ctl  |  node API  | scheduler | blob store | walls    |
             |  UDP :7701 beacon (LAN discovery)              |
             +------------------+----------------------------+
                                | HTTPS (TLS pinned via swarm-key proof)
          +---------------------+----------------------+
          |                     |                      |
   +------+------+       +------+------+        +------+------+
   | SaviorOS    |       | SaviorOS    |        | SaviorOS    |
   | node agent  |       | node agent  |        | node agent  |
   | compute     |       | compute     |        | display     |
   | (old tower) |       | (laptop)    |        | (old LCD)   |
   +-------------+       +-------------+        +-------------+
```

Every node runs `savior node`. It:

1. derives its node ID from hardware,
2. finds the hive (config `hive=` or UDP beacon),
3. registers using a mutual swarm-key proof bound to the TLS certificate,
4. sends a heartbeat every 5 s with live metrics and gets **directives** back
   (display spec, drain, cancellations, actions),
5. long-polls for tasks while it has free capacity, runs them in a sandbox,
   uploads outputs, and reports results,
6. drives the local screen if it has the display role.

## 3. Repository layout

```
cmd/savior/            multi-call binary entry point (subcommand dispatch)
internal/version/      build version (set via -ldflags)
internal/proto/        wire types shared by hive, node and ctl (THE protocol)
internal/config/       savior.conf + kernel cmdline parsing, `savior config`
internal/auth/         swarm-key proofs, tokens, TLS cert generation and pinning
internal/discovery/    UDP beacon announce/discover
internal/hwinfo/       hardware inventory + live metrics from /proc and /sys; `savior info`
internal/power/        battery / AC / thermal / lid policy (pure logic)
internal/runner/       task execution: inputs, sandbox, cgroups, outputs; `savior sandbox-exec`
internal/display/      framebuffer driver, scene renderer, fonts, walls; `savior display`
internal/hive/         coordinator: API, scheduler, state store, blobs, walls, web UI
internal/node/         node agent main loop; `savior node`, `savior console`
internal/ctl/          operator CLI; `savior ctl`
os/rootfs-overlay/     files copied verbatim onto the target rootfs (init scripts etc.)
os/image/              bootable image assembly (hybrid USB .img, ISO, netboot tree)
os/buildroot/          BR2_EXTERNAL tree for production images (x86_64, i686)
os/dev/                dev image built from host (Ubuntu) packages + QEMU test harness
docs/                  user and developer documentation
```

Go module: `github.com/platteration/ewastesavior`, Go 1.24, `CGO_ENABLED=0`.
Allowed dependencies: the standard library, `golang.org/x/sys`, `golang.org/x/image`.
Nothing else without updating this document.

Target architectures for the node binary: `linux/amd64`, `linux/386`
(built with `GO386=softfloat` so it runs on CPUs without SSE2). The hive
and ctl code must also compile for `darwin/*` and `windows/*`: keep Linux-only
syscalls behind `_linux.go` files with stubs for other platforms.

## 4. The `savior` binary

One static binary installed at `/usr/bin/savior`. Subcommands:

| Command | Package entry point | Purpose |
|---|---|---|
| `savior node [flags]` | `node.Main(args) int` | node agent (PID managed by init) |
| `savior hive [flags]` | `hive.Main(args) int` | coordinator |
| `savior ctl <cmd> ...` | `ctl.Main(args) int` | operator CLI |
| `savior info [--json]` | `hwinfo.Main(args) int` | print inventory + metrics |
| `savior display <cmd> ...` | `display.Main(args) int` | `test` pattern, `render` to PNG, `show` a spec on fb |
| `savior config <cmd> ...` | `config.Main(args) int` | `env`, `get KEY`, `dump`, `keys`, `sample` |
| `savior console [flags]` | `node.ConsoleMain(args) int` | text status screen for tty1 |
| `savior sandbox-exec ...` | `runner.SandboxExecMain(args) int` | internal sandbox shim (not for humans) |
| `savior version` | (main) | print version |

Every `Main` takes the arguments *after* the subcommand name, parses its own
flags with `flag.NewFlagSet`, and returns a process exit code. `Main` functions
must not call `os.Exit` themselves (except `SandboxExecMain` after exec failure).

Logging: `log/slog` text handler to stderr. `--log-level` flag or `log_level`
config (debug|info|warn|error).

## 5. Configuration (`savior.conf`)

### 5.1 Sources and precedence

1. Built-in defaults (lowest)
2. Config files, in order: `/etc/savior/savior.conf` (image defaults), then
   `savior.conf` in the root of the first partition labeled `SAVIOR` (the
   stick; the init scripts mount it at `/media/savior`)
3. Kernel command line `savior.<key>=<value>` (highest). Values can't contain
   spaces on the cmdline; use `%20` for a space (URL-style percent decoding
   is applied to cmdline values only).

The init scripts merge everything into `/run/savior/savior.conf` using
`savior config dump --out /run/savior/savior.conf`. Services then read only that file.

### 5.2 File format

```
# comment
key = value          # inline comments are NOT supported (a # inside a value is kept)
key = "quoted value" # optional double quotes are stripped; no escapes except \" and \\
ssh_key = ...        # list keys: repeat the line, or comma-separate (see Type)
```

* Keys are lowercase `[a-z0-9_]+`. Leading/trailing whitespace is trimmed.
* Unknown keys produce a warning (logged), never a hard error.
* Invalid values for known keys: warning + the default is kept.
* The file may have CRLF line endings and a UTF-8 BOM (people edit it on Windows).

### 5.3 Keys

Type legend: `str`, `int`, `bool` (yes/no/true/false/1/0/on/off), `list`
(comma-separated and/or repeated lines; repeated lines append).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | str | "" | Node display name. Empty = `savior-<last 6 hex of primary MAC>`. The hive's stored name wins once an admin renames the node. |
| `node_id` | str | "" | Override the hardware-derived node ID. |
| `roles` | list | `auto` | Any of `auto`, `compute`, `display`, `hive`. `auto` = `compute` + (`display` if a framebuffer exists). `hive` can be combined with the others. |
| `labels` | list | "" | `key=value` pairs attached to the node (for job requirements). |
| `hive` | str | `auto` | `auto` = discover by UDP beacon; or `host`, `host:port`, `https://host:port`. |
| `swarm_key` | str | "" | Shared join secret. Must be at least 16 chars. Nodes don't join without it. |
| `hive_fingerprint` | str | "" | Optional `sha256:<hex>` pin of the hive TLS cert. |
| `net` | str | `dhcp` | `dhcp`, `static`, or `off`. |
| `ip` | str | "" | Static address in CIDR form (`10.77.0.1/24`) when `net=static`. |
| `gateway` | str | "" | Static default gateway. |
| `dns` | list | "" | Static DNS servers. |
| `wifi_ssid` | str | "" | Join this Wi-Fi network (WPA2-PSK or open). |
| `wifi_psk` | str | "" | Wi-Fi passphrase (empty = open network). |
| `wifi_country` | str | `US` | Regulatory domain. |
| `dhcp_server` | bool | no | Serve DHCP on the first wired interface. Needs `net=static`. For isolated swarms on a dumb switch. |
| `dhcp_range` | str | "" | `first-last` IPv4 range for `dhcp_server`. Empty = .100-.200 of the static subnet. |
| `netboot` | bool | no | Serve PXE boot (needs `hive` role and dnsmasq in the image). |
| `ntp` | list | `pool.ntp.org` | NTP servers; `off` disables. Nodes also correct their clock from the hive. |
| `ssh_key` | list | "" | Authorized public keys for `root` over SSH (dropbear). No keys = no SSH server. |
| `console_shell` | bool | no | Root shell on tty2 without password (physical access = trust). |
| `max_cpu_percent` | int | 100 | Percentage of logical CPUs offered to the swarm (1-100). |
| `max_mem_percent` | int | 75 | Percentage of RAM offered to tasks (10-95). |
| `sandbox` | str | `auto` | `auto` (use namespaces/cgroups if available), `strict` (refuse to run tasks without them), `none`. |
| `scratch` | str | `ram` | Task scratch space: `ram` (tmpfs), or a block device / `LABEL=x` holding an existing ext2/3/4 filesystem. |
| `scratch_wipe` | bool | no | Allow formatting the `scratch` device as ext4 (label `SAVIOR-SCRATCH`) at boot if it isn't already. DESTRUCTIVE. |
| `run_on_battery` | bool | no | Laptops: keep taking tasks while on battery. |
| `battery_min_percent` | int | 40 | Below this (on battery) running tasks are preempted and returned to the queue. |
| `max_temp_c` | int | 85 | Above this, tasks are paused (cgroup freeze) until the temperature is 10 °C lower. |
| `cpu_governor` | str | `ondemand` | cpufreq governor set at boot (`ondemand`, `schedutil`, `performance`, `powersave`). |
| `display_mode` | str | `status` | Local display mode until the hive sends one: `status`, `off`, `text`, `clock`, `test`. |
| `display_text` | str | "" | Text for `display_mode=text`. |
| `display_rotate` | int | 0 | 0, 90, 180, 270 (for portrait-mounted monitors). |
| `display_device` | str | `/dev/fb0` | Framebuffer device. |
| `hive_listen` | str | `:7700` | Hive HTTPS listen address. |
| `hive_data` | str | `auto` | Hive state directory. `auto` = `/media/savior/hive-data` if the stick is writable, else `/var/lib/savior/hive`. On non-SaviorOS hosts, `auto` = `$XDG_DATA_HOME/savior/hive` (or `~/.local/share/savior/hive`). |
| `admin_token` | str | "" | Admin API token. Empty = generated on first start and stored in `<hive_data>/admin_token`. |
| `beacon` | bool | yes | Hive broadcasts discovery beacons. |
| `log_level` | str | `info` | `debug`, `info`, `warn`, `error`. |
| `timezone` | str | `UTC` | IANA zone name for the clock display and logs (the binary embeds tzdata). |

### 5.4 Go API (`internal/config`)

```go
type Config struct { /* one exported field per key, typed as above */ }
func Default() Config
func Load(files []string, cmdline string) (Config, []string /*warnings*/, error)
func (c *Config) Set(key, value string) error   // parse one key into c; used by file/cmdline loaders
func (c Config) Get(key string) (string, bool)   // canonical string form
func (c Config) WriteFile(w io.Writer) error     // canonical savior.conf (all known keys)
func (c Config) ShellEnv() string                // SAVIOR_<UPPER_KEY>='value' lines, single-quote escaped
func Keys() []KeyInfo                            // name, type, default, help (drives docs + `config sample`)
func (c Config) EffectiveRoles(hasFramebuffer bool) []proto.Role
func Main(args []string) int
```

`savior config` subcommands:

* `env [--file F]... [--cmdline-file /proc/cmdline]` prints `ShellEnv()`. Init scripts do `eval "$(savior config env ...)"`.
* `get KEY [...]` prints the value.
* `dump [--out PATH]` writes the merged canonical file (mode 0600, it contains secrets).
* `keys` prints a table. `sample` prints a commented `savior.conf` template.

Default file list for all subcommands when no `--file` is given:
`/etc/savior/savior.conf`, `/media/savior/savior.conf`; default cmdline file `/proc/cmdline`
(missing files are skipped silently). `savior node` and `savior hive` default to
`--config /run/savior/savior.conf` if that file exists, else the default list.

## 6. Security model

Assets: the swarm (don't let strangers join or impersonate the hive), the
tasks and data (only the hive admin submits work), and the nodes themselves
(tasks shouldn't take over the node).

Trust assumptions: whoever has `swarm_key` is part of the swarm; whoever has
the admin token controls it; physical access to a node = root on that node.

### 6.1 Hive TLS identity

On first start the hive creates an ECDSA P-256 self-signed certificate
(`<hive_data>/tls/cert.pem`, `key.pem`, 20-year validity, CN `savior-hive`)
and a random 128-bit `hive_id`. The certificate fingerprint is
`sha256:` + lowercase hex SHA-256 of the DER certificate. Clients never use
CA-based verification. They verify the fingerprint (pin) and ignore the
certificate's validity dates, since old machines often have wrong clocks.

### 6.2 Node join handshake (swarm-key proof bound to TLS)

```
node                                   hive
 | TLS connect, record server cert fingerprint FP_seen (no CA check)
 | GET /api/v1/hello  ------------------>  issue hive_nonce (32 random bytes hex,
 |                                         single use, 60 s TTL)
 | <------------------ {hive_id, api_version, version, nonce, time, swarm_hint}
 | node_nonce = 32 random bytes hex
 | proof = HMAC(swarm_key, "savior-node-v1" | hive_nonce | node_nonce | node_id | FP_seen)
 | POST /api/v1/register {node_id, hive_nonce, node_nonce, proof, inventory, ...}
 |                                         consume hive_nonce (reject unknown/expired)
 |                                         recompute proof with FP_own; constant-time compare
 |                                         issue node token (32 random bytes hex); store SHA-256(token)
 | <------------------ {token, hive_proof = HMAC(swarm_key, "savior-hive-v1" | hive_nonce | node_nonce | node_id | FP_own), ...}
 | verify hive_proof with FP_seen; on success pin FP_seen for this process lifetime
```

`|` means concatenation with a single `0x00` byte separator; HMAC is
HMAC-SHA256 and proofs are lowercase hex. Implemented in `internal/auth`:

```go
func NodeProof(swarmKey, hiveNonce, nodeNonce, nodeID, fingerprint string) string
func HiveProof(swarmKey, hiveNonce, nodeNonce, nodeID, fingerprint string) string
func VerifyProof(expected, got string) bool      // constant time
func SwarmHint(swarmKey string) string           // first 16 hex chars of SHA-256("savior-swarm-hint-v1" 0x00 key)
func NewToken() string                           // 32 random bytes, hex
func HashToken(token string) string              // hex SHA-256
func NewNonce() string                           // 32 random bytes, hex
func Fingerprint(der []byte) string              // "sha256:<hex>"
func LoadOrCreateCert(dir string) (tls.Certificate, string /*fp*/, error)
func ClientTLSConfig(pin string, seen func(fp string)) *tls.Config
    // pin != "": the connection fails unless the leaf fingerprint == pin
    // pin == "": accept any cert (TOFU); seen(fp) is called for every handshake
```

Why this is sound: a man in the middle presents its own certificate, so
`FP_seen` is the attacker's fingerprint. Relaying the node's proof to the real hive fails
because the hive recomputes with its own fingerprint. The attacker can't
forge `hive_proof` for its own fingerprint without the key. Nonces make
proofs single-use. The swarm key must be high-entropy (min 16 chars;
`savior ctl genkey` makes 32 random base32 chars) because an observer of one
exchange can try offline guesses against the HMAC.

If `hive_fingerprint` is configured, the node also requires `FP_seen == hive_fingerprint`.

After registration every node request carries `Authorization: Bearer <token>`.
Tokens live in hive memory only (hashed). A hive restart invalidates them
and nodes re-register automatically on HTTP 401.

### 6.3 Admin access

The admin token (random 32 bytes hex, or `admin_token` from config) is sent as
`Authorization: Bearer <token>` to `/api/v1/admin/*`. `savior ctl` pins the hive
fingerprint: `--fingerprint`, or trust-on-first-use saved to the ctl config file.
The web dashboard is served from the same origin. It asks for the token and keeps
it in `sessionStorage`.

Rate limiting: after 5 failed auths from one IP within a minute, the hive
answers 429 for that IP for 60 s (admin and register endpoints).

### 6.4 Task sandbox

Tasks are arbitrary code submitted by the admin. On SaviorOS (agent runs as
root) the runner isolates each task:

* runs as uid/gid `900` (`savior-job`), `PR_SET_NO_NEW_PRIVS`,
* new mount, PID, IPC and UTS namespaces, plus a network namespace with
  only loopback when `network=false` (the default),
* private tmpfs on `/tmp`, fresh `/proc`, workdir owned by the job user,
* cgroup v2 limits: `cpu.max`, `memory.max`, `pids.max=1024`; frozen via
  `cgroup.freeze`, killed via `cgroup.kill`,
* rlimits: `RLIMIT_CORE=0`, `RLIMIT_NOFILE=4096`, `RLIMIT_FSIZE` = task disk quota.

`sandbox=auto` falls back to whatever is available (and logs what's missing),
`strict` refuses tasks if namespaces+cgroups aren't available, `none`
runs tasks as the agent's own user (dev machines).

## 7. Wire protocol (HTTPS + JSON)

All types live in `internal/proto` (see `proto.go`). JSON field names are
snake_case. Times are RFC 3339. Errors are
`{"error": "human readable"}` with a matching HTTP status. All endpoints are
under `/api/v1`. Request bodies are limited to 1 MiB except blob uploads.

### 7.1 Node endpoints

| Method + path | Auth | Request | Response |
|---|---|---|---|
| `GET /api/v1/hello` | none | - | `Hello` |
| `POST /api/v1/register` | proof | `RegisterRequest` | `RegisterResponse` |
| `POST /api/v1/heartbeat` | node | `HeartbeatRequest` | `HeartbeatResponse` |
| `POST /api/v1/claim` | node | `ClaimRequest` | `ClaimResponse` (long-poll up to `wait_s`, max 30) |
| `POST /api/v1/tasks/{id}/report` | node | `TaskReport` | `{}` |
| `POST /api/v1/tasks/{id}/log` | node | raw `text/plain` chunk | `{}` |
| `GET /api/v1/blobs/{sha256}` | node or admin | - | bytes (supports Range) |
| `PUT /api/v1/blobs/{sha256}` | node or admin | bytes | `BlobInfo` (hive verifies hash; 409-free/idempotent) |
| `GET /api/v1/stats` | node or admin | - | `SwarmStats` |

Node-authenticated requests for a node the hive no longer knows (e.g. after a
hive restart) get `401`. The node then re-runs the join handshake.

A node may only report on or log to tasks currently assigned to it (else `409`).

### 7.2 Admin endpoints (`Authorization: Bearer <admin token>`)

| Method + path | Request | Response |
|---|---|---|
| `GET /api/v1/admin/info` | - | `HiveInfo` |
| `GET /api/v1/admin/nodes` | - | `[]NodeView` |
| `GET /api/v1/admin/nodes/{ref}` | - | `NodeView` (`ref` = node ID or unique name) |
| `PATCH /api/v1/admin/nodes/{ref}` | `NodePatch` | `NodeView` |
| `DELETE /api/v1/admin/nodes/{ref}` | - | `{}` (forget; only when offline) |
| `POST /api/v1/admin/nodes/{ref}/action` | `NodeAction` | `{}` |
| `GET /api/v1/admin/jobs` | - | `[]JobView` (newest first) |
| `POST /api/v1/admin/jobs` | `JobSpec` | `JobView` (validated; 400 on error) |
| `GET /api/v1/admin/jobs/{id}` | - | `JobDetail` |
| `POST /api/v1/admin/jobs/{id}/cancel` | - | `JobView` |
| `DELETE /api/v1/admin/jobs/{id}` | - | `{}` (only finished jobs) |
| `GET /api/v1/admin/tasks/{id}` | - | `TaskView` |
| `GET /api/v1/admin/tasks/{id}/log` | - | `text/plain` |
| `GET /api/v1/admin/walls` | - | `[]WallSpec` |
| `POST /api/v1/admin/walls` | `WallSpec` | `WallSpec` (id assigned) |
| `GET/PUT/DELETE /api/v1/admin/walls/{id}` | `WallSpec` | `WallSpec` / `{}` |
| `GET /api/v1/admin/blobs` | - | `[]BlobInfo` |
| `DELETE /api/v1/admin/blobs/{sha256}` | - | `{}` |
| `POST /api/v1/admin/blobs/gc` | - | `{"deleted": n, "freed_bytes": n}` (unreferenced, older than 1 h) |

`GET /` serves the web dashboard (static, embedded, no external resources).

### 7.3 Discovery (UDP 7701)

The hive sends a `Beacon` JSON datagram every 5 s to `255.255.255.255:7701` and
to the directed broadcast address of every up, non-loopback IPv4 interface. It also
answers `Probe` datagrams (`{"svc":"savior-probe","v":1}`) with a unicast
`Beacon` to the sender. A node with `hive=auto` sends a probe on start and every
5 s, and listens on `:7701` for beacons whose `swarm_hint` matches its own
`SwarmHint(swarm_key)`. The first match wins. The beacon's `port` and the
datagram source IP give the hive address. Beacons are unauthenticated
hints; the handshake in 6.2 does the actual authentication. Datagrams over
1400 bytes or with unknown `svc` are ignored.

```go
// internal/discovery
type AnnounceOptions struct { Port int; Interval time.Duration; Targets []string /* override broadcast targets (tests) */ }
func Announce(ctx context.Context, b proto.Beacon, o AnnounceOptions) error  // blocks until ctx done
type DiscoverOptions struct { Port int; ProbeInterval time.Duration; Targets []string; ListenAddr string }
func Discover(ctx context.Context, swarmHint string, o DiscoverOptions) (hiveAddr string, b proto.Beacon, err error)
```

### 7.4 Timing

| Parameter | Value |
|---|---|
| heartbeat interval (sent by hive in `RegisterResponse`) | 5 s |
| node considered `offline` after no heartbeat for | 20 s |
| tasks of an offline node are requeued after | 60 s offline |
| assigned task missing from node's `running_tasks` for | 15 s → requeued (`lost`) |
| task hard deadline on hive | `timeout_s` + 60 s after start |
| claim long-poll max | 30 s |
| hive nonce TTL | 60 s |

## 8. Scheduling

* A **job** is a `JobSpec` plus `count` identical **tasks** (array job). Task `i`
  gets `SAVIOR_TASK_INDEX=i`, `SAVIOR_TASK_COUNT=count`, `SAVIOR_JOB_ID`,
  `SAVIOR_TASK_ID`, and in `command` args and `script` the literal `{{index}}` and
  `{{count}}` are replaced.
* Defaults: `kind=exec` if `command` is set, else `script`; `resources.cores=1`,
  `resources.mem_mb=128`, `resources.disk_mb=256`, `timeout_s=3600`, `retries=1`,
  `count=1`. Validation: `count` 1..100000, cores > 0 and ≤ 256, `timeout_s` ≤ 7 days,
  inputs must have a safe relative `name` (no `..`, not absolute) and either
  `blob` or (`url` + `sha256`).
* Queue order: higher `priority` first, then older job, then lower task index.
* **Claim:** a node sends its free resources. The hive walks pending tasks in
  queue order and assigns each one that fits (cores and memory ≤ remaining free,
  requirements satisfied), up to `max`. A task that doesn't fit is skipped, so smaller
  tasks can still backfill. Requirements: `arch` (any of), `min_mem_mb` (node total),
  `cpu_flags` (all of), `labels` (all equal), `nodes` (node ID or name, any of).
  A node that is draining, paused or offline gets nothing.
* A task needing more cores than any node has will sit in the queue forever. The hive marks
  such jobs with a `warning` in `JobView` but doesn't fail them (nodes may join later).
* **Task states:** `pending → assigned → running → succeeded | failed | canceled`.
  `lost` and `preempted` are transitions that put the task back to `pending`.
* **Retries:** a `failed` report (non-zero exit, timeout, input error) consumes an attempt.
  Up to `1 + retries` attempts are made. `lost` (node vanished) and `preempted`
  (battery/thermal/drain) requeue without consuming an attempt, up to a hard cap
  of `retries + 6` total attempts, after which the task fails with
  `"too many interruptions"`.
* **Job state:** `queued` (nothing started), `running`, `succeeded` (all tasks
  succeeded), `failed` (all tasks terminal, at least one failed), `canceled`.
* **Cancel:** pending tasks → `canceled` immediately. Assigned/running ones are
  listed in the node's next `HeartbeatResponse.directives.cancel_tasks`; the node kills
  them and reports `canceled`. The hive marks them canceled even if the report never arrives.
* **Logs:** nodes POST stdout+stderr chunks at most every 2 s. The hive keeps the
  last 1 MiB per task (older bytes dropped, with a `[... truncated ...]` marker).
* **Outputs:** after the process exits (any outcome), the node uploads files
  matching `outputs` globs (relative to workdir, `**` not supported, max 1000
  files, max total 1 GiB) as blobs and lists them in the report.
* **Retention:** the hive keeps the newest 500 finished jobs. Older ones are deleted
  (their blobs become GC-able).

## 9. Hive internals

* In-memory state guarded by one mutex. Persisted as a JSON snapshot
  `<hive_data>/state.json` (write to temp file, fsync, rename), at most every 2 s
  when dirty and on shutdown. Persisted: hive_id, node records (id, name,
  labels, roles, display spec, drain, first/last seen, last inventory), jobs +
  tasks (running tasks are restored as `pending` after a hive restart), walls, blob metadata.
  Not persisted: node tokens, nonces, logs of finished tasks beyond the 64 KiB tail kept in the task.
* Blobs: `<hive_data>/blobs/<first 2 hex>/<sha256>`. Upload streams to a temp file
  while hashing; a mismatch deletes it (400). Max blob size 8 GiB.
* Long-poll: claims wait on a broadcast channel that's closed and replaced
  whenever tasks become pending or a node's capacity may have changed.
* Background loop every 1 s: mark offline nodes, requeue tasks of long-offline
  nodes, enforce hive-side task deadlines, persist if dirty.
* Walls: saving a wall computes each cell node's `DisplaySpec{mode: "wall", wall: WallTile{...}}`
  and assigns it (bumping `rev`). Deleting a wall resets its nodes to `status`.

## 10. Node agent internals

* Main loop goroutines: discovery/registration, heartbeat, claim loop, task
  workers (one per running task), display controller, power monitor.
* **Identity:** `node_id` = `"n"` + first 12 hex of SHA-256 of the identity source:
  the permanent MAC of the first PCI (non-USB) Ethernet interface (sorted by name),
  else of any physical NIC, else `/sys/class/dmi/id/product_uuid`
  (skipped when all-zero, all-F or the well-known placeholder
  `03000200-0400-0500-0006-000700080009`), else a random ID (logged as unstable).
  Implemented as `hwinfo.Identity(root string) (id string, mac string)`.
* **Clock:** the node computes `offset = hive_time - local_time` from
  `Hello`/`HeartbeatResponse` times (with RTT/2 correction). If running as root on
  Linux and `|offset| > 5 s`, it sets the system clock (dead CMOS batteries are common).
  The display controller uses hive-adjusted time for wall/slideshow sync.
* **Capacity:** `total.cores = floor(logical_cpus * max_cpu_percent/100)` (min 1),
  `total.mem_mb = mem_total * max_mem_percent/100`, `disk_mb` from scratch space.
  Free = total - sum(running task resources). The power policy can force free to 0.
* **Claim loop:** when free cores ≥ 0.5 (or no tasks running), not draining, and the
  power decision allows it, POST `/claim` with `wait_s=25`. On error, back off
  exponentially (1 s → 30 s).
* **Power policy** (`internal/power`, pure function with hysteresis):

```go
type Policy struct { RunOnBattery bool; BatteryMinPercent int; MaxTempC float64 }
type Decision struct { Accept, Pause, Preempt bool; Reason string; LidClosed bool }
func Evaluate(p Policy, m proto.Metrics, prev Decision) Decision
```
  on battery && !RunOnBattery → Accept=false; on battery && percent < min → Preempt;
  temp ≥ MaxTempC → Pause (until temp ≤ MaxTempC-10); battery status unknown = mains.

* **Console:** `savior console` runs on tty1 (inittab respawn). It redraws
  every 2 s: node name, IPs, roles, hive state, CPU/mem/temp/battery, running tasks.
  It reads `/run/savior/status.json`, which the agent writes every heartbeat
  (atomic rename). If the display controller owns the framebuffer (KD_GRAPHICS), the
  text console isn't visible anyway.

## 11. Display subsystem

* Device: Linux fbdev (`/dev/fb0`). Our kernels enable `DRM_FBDEV_EMULATION` so every
  KMS driver (i915, radeon, nouveau, amdgpu, bochs, virtio-gpu) plus `simpledrm`,
  `efifb` and `vesafb` expose fbdev. The driver reads `FBIOGET_VSCREENINFO` and
  `FBIOGET_FSCREENINFO`, mmaps the framebuffer (falls back to `pwrite`), and
  converts an `*image.RGBA` back buffer into the device pixel format using the
  reported bitfields. Supported: 32, 24, 16 (565/555) bpp. 8 bpp is unsupported
  (error surfaced in `DisplayState.error`).
* On start the controller puts the active VT in graphics mode
  (`KDSETMODE KD_GRAPHICS` on `/dev/tty0`) so fbcon stops drawing, and restores
  `KD_TEXT` on exit. Blanking: `FBIOBLANK`, plus `/sys/class/backlight/*/bl_power`.
* Scenes (`DisplaySpec.mode`):
  * `status`: large node name, IPs, roles, hive link, CPU/RAM/temp/battery bars,
    running tasks. This is the default and the "which machine is this?" screen.
  * `off`: blank the screen (backlight off).
  * `color`: solid `bg`.
  * `text`: `text` centered, auto-sized to fill, word-wrapped, `fg` on `bg`, optional `title`.
  * `clock`: large time (`clock_format`, Go layout, default `15:04`) + date, `timezone` from config.
  * `image`: one `Media` scaled with `fit` (`contain` default, `cover`, `stretch`).
  * `slideshow`: `images` rotated every `interval_s` (default 10). The index is
    `floor(hive_time_unix / interval_s) mod len`, so all screens stay in sync.
  * `dashboard`: swarm totals from `GET /api/v1/stats`, refreshed every 5 s.
  * `wall`: this screen shows one tile of `wall.content` (image, slideshow, text
    or color) laid out over `rows × cols` screens with `bezel_px` compensation.
  * `test`: color bars, gradients, a 1-px border and the resolution (for checking dead pixels).
  * `identify` is not a mode. It's an action: for N seconds a huge node name on
    a flashing background is drawn over the current scene.
* `rotate` (0/90/180/270) renders at swapped dimensions and rotates on blit.
* Fonts: Go fonts (`golang.org/x/image/font/gofont`) via `opentype`, cached per size.
* Media fetch: blobs from the hive (node token) or `url` (http/https, max 64 MiB),
  decoded with `image/png`, `image/jpeg`, `image/gif` (first frame),
  `golang.org/x/image/bmp`, `golang.org/x/image/webp`. Cached in memory by key
  (LRU of 8 decoded images, max ~64 MiB).
* The renderer is pure (`Render(spec, w, h, env) *image.RGBA`) and fully testable
  without hardware. `savior display render --spec spec.json --size 800x600 --out x.png`
  exposes it.

```go
// internal/display
type Device interface { Size() (w, h int); Show(img *image.RGBA) error; Blank(on bool) error; Close() error }
func OpenFramebuffer(path string) (Device, error)        // linux; other OS: error
func NewPNGDevice(path string, w, h int) Device           // writes each frame to a PNG (tests, headless)
type Env struct {
    Now     func() time.Time                              // hive-adjusted clock
    Status  func() StatusInfo
    Stats   func(ctx context.Context) (*proto.SwarmStats, error)
    Fetch   func(ctx context.Context, m proto.Media) (image.Image, error)
    Location *time.Location
}
type StatusInfo struct { Name, NodeID, Version, HiveState, Message string; Addrs []string; Roles []proto.Role; Metrics proto.Metrics; Inventory proto.Inventory; RunningTasks int; PowerReason string }
func Render(ctx context.Context, spec proto.DisplaySpec, w, h int, env Env) (*image.RGBA, error)
type Controller struct { /* ... */ }
func NewController(dev Device, env Env, log *slog.Logger) *Controller
func (c *Controller) Apply(spec proto.DisplaySpec)        // non-blocking; takes effect next frame
func (c *Controller) Identify(d time.Duration)
func (c *Controller) State() proto.DisplayState
func (c *Controller) Run(ctx context.Context) error       // render loop; redraws on change or when the scene needs it (clock: 1 s, status: 2 s, dashboard: 5 s, slideshow: on boundary)
```

## 12. Task runner

```go
// internal/runner
type Transfer interface {
    FetchBlob(ctx context.Context, sha256, dst string) error
    FetchURL(ctx context.Context, url, sha256, dst string) error
    UploadFile(ctx context.Context, path string) (sha256 string, size int64, err error)
}
type Config struct { WorkRoot, CacheDir string; Sandbox string; JobUID, JobGID int; CgroupRoot string; SelfExe string; Log *slog.Logger }
func New(cfg Config, tr Transfer) (*Runner, error)
func (r *Runner) Caps() Caps                               // what isolation is available
func (r *Runner) Run(ctx context.Context, t proto.Task, logs io.Writer) proto.TaskReport // blocks; ctx cancel ⇒ kill, State=canceled
func (r *Runner) Freeze(taskID string, frozen bool) error
func (r *Runner) Preempt(taskID string) error              // kill ⇒ report State=preempted
func SandboxExecMain(args []string) int
```

Flow for one task: create `<WorkRoot>/<task_id>/` (mode 0700, chowned to the job
user) → fetch inputs (blob cache in `CacheDir`, then copy; executable bit if
`input.executable`) → create cgroup `<CgroupRoot>/task-<id>` with limits → start
`SelfExe sandbox-exec --uid .. --gid .. --workdir .. --rlimit-fsize .. [--no-net]
-- <command...>` in new namespaces (clone flags from Go's `SysProcAttr`, `CgroupFD`
when available) → stream combined output to `logs` → enforce `timeout_s` → collect
outputs → upload → remove workdir → return `TaskReport` (exit code, error,
outputs, cpu seconds from `cpu.stat`, peak memory from `memory.peak` when present).

`script` tasks write the script to `<workdir>/.savior-script` and run `/bin/sh
.savior-script`. Environment: only `PATH=/usr/local/bin:/usr/bin:/bin`, `HOME=<workdir>`,
`TMPDIR=/tmp`, `LANG=C.UTF-8`, the `SAVIOR_*` vars, plus `env` from the spec.

## 13. Boot and OS integration

### 13.1 Boot media

`os/image/mkimage.sh` builds, from one or two payloads (`x86_64`, `i686`, each a
`vmlinuz` + `initrd`):

* `savior.img`: MBR disk, GRUB `boot.img` in the MBR, GRUB `i386-pc` core image
  in the post-MBR gap (sector 1..2047), one FAT32 partition (type 0x0C, bootable,
  starts at 1 MiB, label `SAVIOR`) containing:
  ```
  /EFI/BOOT/BOOTX64.EFI  /EFI/BOOT/BOOTIA32.EFI  /EFI/BOOT/grub.cfg
  /boot/grub/grub.cfg    /boot/x86_64/{vmlinuz,initrd}  /boot/i686/{vmlinuz,initrd}
  /savior.conf           /README.txt
  ```
  GRUB picks the payload with `cpuid -l` (long mode → x86_64). On `i386-efi` it
  prefers i686. The menu also offers: safe graphics (`nomodeset`), force 32-bit,
  display-only, and a debug shell.
* `savior.iso`: El Torito BIOS (GRUB `i386-pc-eltorito`) + EFI (FAT `efi.img`)
  for machines that can only boot from CD.
* `netboot/`: GRUB PXE images (`i386-pc-pxe` core `pxegrub.0`, `grubx64.efi`
  net image), `grub.cfg`, and the payloads, for serving via TFTP/HTTP.

The kernel command line always includes `consoleblank=0 quiet loglevel=3`.

### 13.2 Init sequence (BusyBox init)

`/etc/inittab` (from `os/rootfs-overlay`):

```
::sysinit:/etc/init.d/rcS
tty1::respawn:/usr/bin/savior console
tty2::respawn:/usr/libexec/savior/tty2
tty3::respawn:/sbin/getty -L 0 tty3 linux     (only useful with a root password; locked by default)
::respawn:/usr/libexec/savior/run node
::ctrlaltdel:/sbin/reboot
::shutdown:/etc/init.d/rcK
```

`rcS` runs `/etc/init.d/S??*` with `start` in order:

| Script | Job |
|---|---|
| `S00mounts` | mount proc, sysfs, devtmpfs, /run, /tmp (tmpfs), devpts, cgroup2 at `/sys/fs/cgroup`; enable controllers |
| `S05mdev` | hotplug via mdev, coldplug (modalias → modprobe), firmware loading |
| `S08config` | find + mount the `SAVIOR` partition read-only at `/media/savior` (try vfat, ext4), run `savior config dump --out /run/savior/savior.conf`, write `/run/savior/env` (`savior config env`) |
| `S10system` | hostname, timezone, sysctl, cpufreq governor, zram swap (50% of RAM, lz4 or zstd) |
| `S20scratch` | set up task scratch (tmpfs or configured disk, `scratch_wipe` honored) at `/var/lib/savior/work` |
| `S30network` | lo; wired interfaces with udhcpc (background) or static; wifi via wpa_supplicant; resolv.conf |
| `S35dhcpd` | udhcpd when `dhcp_server=yes` |
| `S40time` | ntpd (if in image and `ntp` != off) |
| `S50sshd` | dropbear when `ssh_key` set (host keys generated in RAM each boot, or persisted on the stick under `/media/savior/ssh/` when writable) |
| `S60hive` | if roles include hive: remount stick rw if `hive_data` lives there, start `savior hive` (respawned by `/usr/libexec/savior/run hive` via start-stop-daemon loop) |
| `S65netboot` | dnsmasq in proxy-DHCP + TFTP mode when `netboot=yes` |

All scripts are POSIX `sh` (BusyBox ash), `set -u` safe, shellcheck-clean,
idempotent, and log with `logger -t savior-init` plus a short line on the
console. They source `/run/savior/env` for configuration.

`/usr/libexec/savior/run <service>` is a tiny supervisor wrapper: it sources
the env, execs `savior <service> --config /run/savior/savior.conf`, and appends
output to `/var/log/savior-<service>.log` (rotated at 1 MiB, one old copy).

### 13.3 Users

`root` (password locked), `savior-job` uid/gid 900 (no shell, home `/var/lib/savior/work`).

## 14. Build system

* `make` (top level): `build` (host binary into `build/`), `build-linux` (amd64 +
  386 softfloat into `build/linux-{amd64,386}/savior`), `test`, `vet`, `fmt-check`,
  `lint-sh` (shellcheck), `dev-image` (os/dev), `dev-test` (QEMU tests),
  `image ARCH=x86_64|i686` (Buildroot), `universal-image` (both + mkimage), `clean`.
* Buildroot (`os/buildroot`, BR2_EXTERNAL name `SAVIOR`): pinned Buildroot release
  downloaded to `build/buildroot-<ver>`; defconfigs `savior_x86_64_defconfig` and
  `savior_i686_defconfig`; kernel = arch defconfig + fragments in
  `board/savior/linux/*.config`; musl toolchain; BusyBox init with mdev; rootfs =
  initramfs (cpio, xz); packages: busybox, dropbear, wpa_supplicant, iw,
  wireless-regdb, linux-firmware (selected), dnsmasq, e2fsprogs (mke2fs),
  dosfstools, ca-certificates, grub2 (i386-pc, i386-efi, x86_64-efi), zstd?;
  post-build installs the prebuilt `savior` binary for the target arch; post-image
  calls `os/image/mkimage.sh`. `os/buildroot/scripts/check-defconfig.sh` fails if
  any `BR2_` symbol in a defconfig didn't survive `olddefconfig`, which catches
  renamed or dropped Buildroot options.
* Dev image (`os/dev/build.sh`): no Buildroot. Uses the host's (Ubuntu 24.04) kernel
  (`linux-image-*-generic`, downloaded with `apt-get download`), a needed subset of
  its modules (decompressed, depmod'ed), `busybox-static`, optionally dropbear and
  dnsmasq with their shared libraries, the same `os/rootfs-overlay`, and the amd64
  `savior` binary. It's used for fast iteration and automated QEMU tests
  (`os/dev/qemu-test.sh`): BIOS boot, UEFI boot, and a multi-VM swarm on a
  QEMU socket-multicast LAN (hive + compute + display) that runs a job end to end
  and verifies the display through a QEMU `screendump`.

## 15. Testing strategy

* Unit tests per package. `hwinfo` uses fixture trees under
  `internal/hwinfo/testdata/<machine>/{proc,sys}`. `display` renders to images and
  checks pixels. `runner` runs real processes in `sandbox=none` mode, and
  namespace/cgroup paths are tested when running as root with cgroup2.
* `internal/node` integration test: a real `hive.Server` on `httptest` TLS + 3 in-process
  agents with fake hardware roots and PNG display devices. It covers join, heartbeat,
  job with count=5 and outputs, cancellation, node loss → requeue, display assignment,
  walls, a wrong swarm key being rejected, and a MITM (wrong cert) being rejected.
* Shell: shellcheck on all scripts.
* QEMU tests (see 14), run in CI.
