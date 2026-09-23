# Security model

SaviorOS runs code sent over the network on machines you don't watch. This
page summarizes what protects what. The normative details are in
[DESIGN.md](DESIGN.md), section 6.

## Who is trusted

| Holder of... | Can... |
|---|---|
| the **swarm key** | join machines to the swarm. Without a `hive_fingerprint` pin, it can also pose as the hive to nodes that re-join. |
| the **admin token** or an admin session | control the swarm: run code on every node, change screens, reboot machines. |
| **physical access** to a node or its stick | become root on that node and read the swarm key from the stick. |

Use `savior ctl node-config` (or the dashboard download) to make stick
configs. It always includes the hive's fingerprint pin, which removes the
"pose as the hive" risk.

## Joining (node ↔ hive)

* The hive has a self-signed certificate. Nodes identify it by the SHA-256
  fingerprint of that certificate, never by certificate authorities or dates.
  Old machines often have wrong clocks.
* The swarm key is stretched with PBKDF2 (200,000 rounds) and never sent.
  Node and hive each send a proof: an HMAC over fresh nonces, the node ID,
  and the certificate fingerprint the node actually saw.
* After the first contact the node pins that certificate. The proof is only
  sent over pinned connections, so a man in the middle never receives a
  proof bound to its own certificate, and can't replay one either.
* The LAN beacon carries only a 32-bit hint derived from the stretched key.
* Failed joins are rate-limited per source network.

## Administration

* `savior ctl login` also sends a proof instead of the admin token, then
  keeps a 12-hour session. The hive's fingerprint is pinned in `ctl.json`.
* The web dashboard logs in with a single-use pairing code shown on the
  hive's screen. It never takes the admin token. Sessions are HttpOnly,
  Secure, SameSite=Strict cookies. State-changing requests also need a
  custom header and a matching Origin (CSRF protection).
* The dashboard is served with a strict Content-Security-Policy (no inline
  script, no external resources). Data from nodes is only ever inserted as
  text. Blobs and task outputs are served as downloads under a sandbox CSP.

## What a node token can do

A node's token is scoped. The node can download only blobs its current tasks
or screen need. It can upload only while running a task, up to 1 GiB per
task. It can claim tasks only within its real hardware capacity. Labels and
names a node reports are advisory, and admin settings win.

## Task sandbox

On SaviorOS (`sandbox = strict`), every task:

* runs in new mount, PID, IPC, UTS and network namespaces (only loopback
  unless the job asks for network);
* sees a minimal root: read-only system directories, a generated `/etc`,
  its own `/work` and a small `/tmp`. There is no `/sys`, and no stick, logs or
  agent state. `/proc/cmdline` and other sensitive `/proc` files are masked;
* runs as an unprivileged per-slot user with no capabilities and
  `no_new_privs`, under a seccomp filter that blocks namespace creation,
  mounting, kernel keyrings, BPF, io_uring, module loading, ptrace and more.
  The filter checks the architecture, so 32-bit or x32 syscalls on 64-bit
  can't bypass it;
* has cgroup limits on CPU, memory (swap disabled), process count and
  scratch space, and is killed as a whole group.

Result files are collected only after every task process has exited. Only
regular files owned by the task, with a single link, are collected, and
symlinks, FIFOs and hard links are refused. This stops a task from
exporting files it doesn't own.

The kernel side: user namespaces, unprivileged BPF and userfaultfd are
disabled, `dmesg` is restricted, and CPU vulnerability mitigations stay on.

## Limits you should know about

* Tasks share the node's CPU caches and memory bus with the agent. CPU
  side channels on pre-2018 processors can't be fully mitigated.
* Anyone who can reach port 7700 can attempt to join or log in, so rate
  limits are the defense against guessing. Use a swarm key from `savior ctl
  genkey` (160 bits), not a memorable phrase.
* Netboot is unauthenticated by nature. Netbooted nodes join keyless and
  need approval, but use netboot only on networks you trust.
* Secure Boot isn't supported yet.

## Reporting problems

Please report security issues privately to the maintainers instead of in
public issues.
