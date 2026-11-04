# Security

zpinit runs as PID 1 in a container. This document describes the
security properties it tries to maintain, and the ones it deliberately
does not.

## Threat model

zpinit assumes a single trusted operator (root inside the container)
and possibly less-trusted child services running as other UIDs. The
goals are:

- A non-root process inside the container cannot impersonate the
  operator over the control socket.
- A misbehaving or hostile service config cannot trick zpinit into
  writing into operator-only files (e.g. `/etc/shadow`).
- A service writing crafted log lines cannot corrupt the wire protocol
  back to `zpctl`.

zpinit is **not** a sandbox. It does not isolate services from each
other beyond what the kernel already does for separate UIDs. Container
escape, kernel exploits, and host-level threats are out of scope.

## Control socket

`/run/zpinit.sock` is the only IPC surface, and it has two independent
access gates:

1. **Filesystem permissions.** The socket is bound under
   `syscall.Umask(0o077)` so it is born `0700`, then explicitly set to
   `0600`. Without the umask flip there is a brief window where the
   socket is `0755` and a non-root local process can win a TOCTOU race.
2. **Peer credentials.** Every accepted connection is gated by
   `SO_PEERCRED` (Linux). The peer's effective UID must match the
   daemon's effective UID; otherwise the connection is dropped before
   any command is read. macOS dev builds skip this check (they only
   exist for compile-testing, not production).

These gates are layered, not redundant. If you ever loosen the
filesystem permissions to allow a non-root operator UID, you must keep
the peer-cred check, not widen it. Non-root services in the same
container cannot use `zpctl`.

The client resolves the socket path from `--socket PATH`, then the
`ZPINIT_SOCKET` environment variable, then `/run/zpinit.sock`. This only
changes which socket `zpctl` dials; it does not relax either access
gate. The daemon binds the path from `control_socket` in
`zpinit.toml`, so a non-default socket must be configured on both
ends.

## Log file handling

Service log destinations (`log.stdout`, `log.stderr`), `zpctl tail`
one-shot reads, and `zpctl tail --follow` streaming reads all open
the configured log path with `O_NOFOLLOW`. A symlink at the leaf of
a configured path is rejected at open time; the follow loop's
inode-comparison rotation detection cannot bypass this because every
reopen goes through the same `O_NOFOLLOW` path. `readLastBytes` and
the follow loop additionally require `Mode().IsRegular()`.

Without this hardening, an operator typo or a hostile service config
could plant a symlink at a log path and cause zpinit to append a
child's stdout into a sensitive file. Symlinked parent directories
still resolve normally; only the leaf is gated.

## Wire-protocol sanitization

`zpctl tail` and `zpctl tail --follow` surface service-controlled
log content; config-error responses can contain multi-line TOML
messages. All flow through `ctlproto.WriteResponse` (one-shot) or
`ctlproto.WriteBodyLine` (streaming), which sanitize every line
identically: CR and LF become spaces, and a lone `.` is prefixed
with a space (the line-based protocol uses `.` as a body
terminator).

Without this, a service that writes a crafted log line could split a
single field across frames or end the response body early at the
client. The streaming path inherits the same guarantee because it
shares the sanitizer with the one-shot path.

## Env injection and `/proc`

Environment variables set via `[env]` in `zpinit.toml` (or written to
`/run/zpinit/env` by an entrypoint script) are exposed to the wrapped
CMD or to managed services through the standard Linux exec path. Once
a process is running with those variables in its environment, they are
readable via `/proc/<pid>/environ` by **any process running as the same
UID** (and by root).

This is a property of the Linux process model, not a zpinit-specific
weakness. Implications:

- Do not put secrets in `[env]` if other processes inside the
  container, running under the same UID, should not see them. A second
  service in the same TOML, or a `docker exec` shell as the same user,
  can read the environ file directly.
- Different UIDs are protected: `/proc/<pid>/environ` is `0400` owned
  by the process's UID, so a service running as `app:app` cannot read
  the environ of a service running as `db:db`.
- For real secrets, prefer mounted files with restrictive permissions
  (e.g. a Kubernetes secret mounted as `0400` and owned by the
  consuming service's UID), not env vars.

zpinit does not try to scrub `/proc/<pid>/environ`; doing so is not
possible from userspace once the process has started. The mitigation
is operator hygiene, not runtime enforcement.

## Parent-death signal and `pdeathsig = false`

Every supervised child gets `Pdeathsig: SIGKILL`: if zpinit dies, the
kernel kills its children, so nothing keeps running without a
supervisor. A service can opt out with `pdeathsig = false`. Such a
service **outlives its supervisor**: after a zpinit crash or restart
it keeps running with no zpinit tracking it, and a restarted zpinit
starts a second copy unless the service itself guards against that
(a pidfile, a lock, re-adoption). The opt-out exists for a host-side
zpinit supervising long-lived per-VM shims, where an in-place zpinit
upgrade must not take every VM down; it has no use inside an ordinary
container.

## Machine mode

As the kernel's init (see [machine-mode.md](machine-mode.md)) zpinit
gains three powers a container must never have: `reboot(2)`, a vsock
listener that runs commands as root, and acting on
`/run/zpinit/machine.json`. All three are gated on detection alone
(PID 1 in the kernel's initial PID namespace), never on a flag or an
environment variable, so a container cannot switch them on by mistake.
A container cannot change its PID namespace link; faking it by
mounting over its own `/proc` only fools that container's own zpinit,
which still is not the kernel's init and gets `EPERM`/`EINVAL` from
`reboot`. A fooled zpinit would also open the vsock exec listener if a
planted `machine.json` has an `exec` block, which is why the fake needs
control of the container's own `/proc` and PID 1 to begin with: it buys
nothing that container did not already have, except reachability from
a hypervisor when the host is itself a VM.

- **`machine.json` in a container** is reported with a warning and
  never read. In machine mode it is decoded strictly; a file that fails
  validation parks the machine and opens no exec listener.
- **vsock exec** accepts only peer CID 2 (the host). vsock has no peer
  credentials, so this is the whole authentication; a local process
  connecting over vsock loopback (CID 1) is refused. It is opened only
  with an `exec` block in `machine.json`, a double gate because vsock is
  not network-namespaced: in a container on a host that is itself a VM,
  a listener would be reachable from that VM's hypervisor.
- **The reset line is forgeable** by anything that can write the
  console. That is acceptable only because it carries nothing the host
  acts on more strongly than "the guest wants to end", which the guest
  can always achieve by exiting.
- An exec session's process group is killed when its connection drops.
  A process that leaves the group with its own `setsid` survives until
  the machine ends, as a daemonised process would under `docker exec`.

## Non-goals

zpinit does not provide:

- Per-service filesystem isolation, namespaces, or seccomp policies.
  Use the container runtime for that.
- Encryption or authentication on the control socket beyond
  `SO_PEERCRED`. The socket is never exposed over the network.
- Audit logging of operator commands.
- Integrity verification of the binary or its config files. Use image
  signing and read-only root filesystems if you need this.
