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

- Discovery: Docker-aware (remote `docker ps`), not an `ss` port scan
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
      ports.go             Ports-column parser (unit tested)
      types.go             PortSpec, PortMap, tunnel keys
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

**Tunnels.** Binding *is* the reservation, so two tunnels cannot race into the same local
port. The tunnel key is `containerID:containerPort/proto` — tied to the container port,
not the host port, so a container republished on a different host port is recognised as
the same tunnel and reclaims its local port. Local listeners deliberately stay bound
across SSH outages so local port numbers never move under the user's feet.

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

## Verification

Unit tests, no remote host required:

```sh
go test ./... -race
```

- `internal/discovery` — Ports parser: IPv4/IPv6 duplicates, UDP, exposed-only, ranges,
  oversized ranges, malformed entries, plus a column captured verbatim from a real
  Temporal container
- `internal/tunnel` — allocator (preferred port, fallback, sticky reclaim, exhausted
  range) and the full data path against an in-process dialer: round trips and byte
  accounting, churn isolation, retargeting, pause, degraded-then-recovered, teardown
- `internal/ui` — header and table rendering, banners for SSH and Docker failures, key
  handling, filtering, sorting, selection stability across snapshots
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
