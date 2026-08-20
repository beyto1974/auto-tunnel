# auto-tunnel — design and status

## Context

Problem: the remote server runs Docker containers. Reaching them from a local machine
meant hand-writing `ssh -L` per port, redoing it every time a container started, stopped,
or changed port, with no way to see which tunnel was still alive. `autossh` is not
container-aware and was not installed.

Goal: one Go binary. Point it at a remote host. It watches remote `docker ps`, opens a
local TCP tunnel for every published container port, closes tunnels when containers die,
survives SSH drops, and shows a live table in the terminal.

Decisions:

- Discovery: Docker-aware (remote `docker ps`) first; the host's own listening sockets
  and hand-declared forwards were added later as two further sources
- Language: Go, single static binary, native SSH — no shelling out to the `ssh` client
- Monitoring: terminal TUI (bubbletea + lipgloss)
- Run mode: foreground process, no systemd
- Hosts: one remote per process
- Auth: ssh-agent + `~/.ssh/config`
- Port conflict: same port if free, otherwise auto-offset from a fallback base

User-facing usage, flags, and keys are documented in [README.md](README.md).

## Layout

```
auto-tunnel/
  main.go                  flags, logging, wiring, shutdown
  engine.go                poll/reconcile loop, snapshot publishing, UI actions
  internal/
    sshconn/
      dial.go              ssh_config + agent + knownhosts -> *ssh.Client
      keepalive.go         keepalive@openssh.com ping, RTT, reconnect with backoff
      exec.go              run a remote command, surface stderr in the error
    discovery/
      docker.go            poll `docker ps`, apply filters, resolve container IPs
      hostports.go         poll `ss`/`netstat`, the host's own listening sockets
      static.go            -forward spec parser: labels, local:remote, ranges, @host
      ports.go             Ports-column parser (unit tested)
      types.go             Source, PortSpec, PortMap, tunnel keys
    probe/
      probe.go             ask a forwarded port whether it speaks HTTP
    openurl/
      openurl.go           hand a URL to the desktop's browser
    tunnel/
      manager.go           reconcile desired vs live set
      forwarder.go         listener -> ssh.Dial -> io.Copy with counters
      portalloc.go         same-port-else-offset allocator
    state/
      snapshot.go          immutable view handed to the renderer
    logbuf/
      logbuf.go            slog handler mirroring records into a ring buffer
    ui/
      model.go             bubbletea model, keys, filter, sorting
      view.go              lipgloss rendering of header, table, log pane
```

Dependencies: `golang.org/x/crypto/ssh` (+ `agent`, `knownhosts`), `golang.org/x/term`,
`github.com/kevinburke/ssh_config`, `github.com/charmbracelet/bubbletea`,
`github.com/charmbracelet/lipgloss`.

## Design notes

**SSH layer.** `ResolveTarget` parses `user@host:port` and fills the gaps from
`~/.ssh/config` (`HostName`, `User`, `Port`, `IdentityFile`, single-hop `ProxyJump`).
Auth prefers ssh-agent, then lazily loads on-disk keys — lazily so an encrypted key only
prompts for a passphrase if the agent could not authenticate first. Host keys are checked
against `known_hosts` with no trust-on-first-use, since unattended forwarding is exactly
where a silent MITM window would matter. `Conn` keeps the connection alive with
`keepalive@openssh.com` pings (which also give the RTT shown in the header) and
reconnects with jittered exponential backoff, 1s to 30s.

**Discovery.** One `docker ps --format '{{json .}}'` per tick — a single round trip, no
`docker inspect` fan-out unless unpublished ports are being forwarded. The Ports column
parser handles published ports, collapsed ranges, exposed-only ports, and the duplicate
IPv4/IPv6 rows of a single publish. Published ports are dialed as `127.0.0.1:hostPort`
from the remote side, which works even for ports published only to loopback. A discovery
failure is surfaced as a banner and leaves existing tunnels running.

**Sources.** Every candidate carries a `Source`: `docker`, `host` (a socket the remote
host itself is listening on), or `static` (declared with `-forward`). The source is part
of the tunnel key, so a container port and a host socket on the same number stay two rows
with two local ports rather than one row that flickers between descriptions. Host rows
are deduplicated against Docker's published ports, because `docker-proxy` listens on
every one of them and would otherwise double each container row. Host discovery defaults
to `select`, which lists rows as `AVAILABLE` without binding anything: a remote host
listens on far more than a user wants republished, so the default shows the choice rather
than making it. `Offered` is true for every mode except `all`, so a `HostMode` assembled
in code can never forward a host socket by accident. Static rows are published on every poll from memory
and depend on no remote command, which is what makes them survive a failing `docker ps`.

**Failure isolation.** Each source caches its last good answer and contributes that when
a poll fails, so one source failing never tears down another's tunnels. `-no-docker`
drops Docker entirely for hosts that run none, rather than living with a permanent
failure banner.

**Tunnels.** Binding *is* the reservation, so two tunnels cannot race into the same local
port. The tunnel key is `source:owner:port/proto` — tied to the container port, not the
host port, so a container republished on a different host port is recognised as the same
tunnel and reclaims its local port. Host rows share one owner (`host`) so a service that
restarts under a new PID keeps its tunnel. Local listeners deliberately stay bound across
SSH outages so local port numbers never move under the user's feet, and switching a row
off releases its port while the allocator keeps the assignment, so switching it back on
reclaims the same number.

**UI.** The engine publishes immutable snapshots twice a second; the UI only reads
snapshots and calls back for actions, so rendering can never race the forwarders. Logs go
to a file and an in-memory ring buffer, never stdout — writing to stdout would corrupt the
frame.

## Milestones

All shipped, one commit each:

1. Repo scaffold, flags, `sshconn.Dial` + keepalive + reconnect
2. `discovery` package and Ports-column parser with table-driven tests
3. `tunnel` manager, port allocator, forwarder
4. Reconcile churn correctness (covered by manager tests)
5. Reconnect behaviour: DEGRADED state, listeners survive, recovery re-dials
6. TUI: table, keys, sorting, filter, log pane
7. Polish: target-parsing tests, full README

Milestones 4 and 5 produced no separate commit: their behaviour is implemented in the
manager and verified by `internal/tunnel` tests rather than by extra code.

Since then, non-Docker sources:

8. `Source`/`Owner`/`Offered`/`LocalPref` on `PortMap`, generalized tunnel keys, `SRC`
   column
9. `-forward`: individual ports, `local:remote`, ranges, labels, `@host`
10. `-host-ports off|select|all` with the `ss`/`netstat` parser and the dedupe against
    Docker's published ports
11. `AVAILABLE` rows and the dashboard keys that switch them: `enter`, `a`, `t`
12. `-selection` persistence, `-no-docker`, `-host-exclude`
13. HTTP probing of forwarded ports, the URL column, and the `o` key
14. `f` forwarded-only filter, sort by remote/url, and a remembered sort order

**Probing.** A forwarded port is asked once what it speaks: TLS first, then plaintext,
each on its own connection, then a single `HEAD /`. TLS has to come first because a
plaintext request to an HTTPS port does not fail cleanly — servers answer it with a
plaintext "400, you sent an HTTP request to an HTTPS port", which is a valid status line
and would report every HTTPS service as http. Probes go through the SSH client rather
than through our own local listener, so the dashboard's byte and connection counters
only ever reflect the user's traffic. Only forwarded ports are probed; a row the user is
merely looking at is never touched.

## Verification

Unit tests, no remote host required:

```sh
go test ./... -race
```

- `internal/discovery` — Ports parser: IPv4/IPv6 duplicates, UDP, exposed-only, ranges,
  oversized ranges, malformed entries, plus a column captured verbatim from a real
  Temporal container; the `ss` and `netstat` layouts, their IPv4/IPv6 collapse and
  unprivileged output; `-forward` specs, ranges, labels, and every way to get one wrong
- `internal/tunnel` — allocator (preferred port, fallback, sticky reclaim, exhausted
  range) and the full data path against an in-process dialer: round trips and byte
  accounting, churn isolation, retargeting, pause, degraded-then-recovered, teardown
- `internal/ui` — header and table rendering, banners for SSH and Docker failures, key
  handling, filtering, sorting, selection stability across snapshots, the source cycle
  and the enable/bulk keys acting on exactly the visible rows
- `internal/sshconn` — target parsing, including IPv6 and malformed input

Manual checks against a real remote host with Docker (not yet run — no remote host was
available in the environment where this was built):

1. `go build -o auto-tunnel . && ./auto-tunnel <host> -no-tui` — the listed ports match
   remote `docker ps`
2. `curl` a forwarded HTTP container; byte counters move
3. Remote `docker stop <c>` — the row disappears within one interval, the local port is
   released
4. Remote `docker start <c>` — the row returns on the same local port
5. Occupy the preferred port locally first — the tunnel takes a fallback port and the
   table shows the real mapping
6. Kill the network — state goes DEGRADED, backoff is logged, recovery does not change
   local port numbers
7. `q` and `Ctrl-C` both exit clean, leaving no orphan listeners

## Risks and follow-ups

- `docker ps` needs the remote user in the `docker` group, or `-docker-cmd` with
  passwordless sudo. The failure is surfaced with the remote stderr, not a bare exit code.
- UDP and SCTP services cannot be tunneled by SSH. They are displayed, not forwarded.
- Polling costs one SSH exec session per interval. Cheap, but `docker events` streaming is
  the obvious upgrade; discovery is isolated enough that swapping it is a local change.
- The allocator remembers every tunnel key it has seen so restarts reclaim their port.
  On a host with very heavy container churn that map grows slowly and is never pruned.
- Host ports are identified by number alone, so a service that moves ports comes back as
  a new row and an `AVAILABLE` choice does not follow it. Matching on the process name
  would follow the service but break when the name is unavailable without privilege.
- `-host-ports` adds one more remote command per interval. It is a single round trip and
  is only paid when the mode is not `off`.
- The selection file is written on every change, whole, and defaults to one file per
  target under the user's config directory. It is a handful of keys, but a dashboard used
  as a click-through would rewrite it often. Nothing is written until a key is pressed,
  so a run that only watches leaves no trace.
