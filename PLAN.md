# auto-tunnel — Plan

## Context

Greenfield. `/home/mustiko/claude/auto-tunnel` empty, not git repo.

Problem: remote server run Docker containers. Reaching them from local machine mean hand-writing `ssh -L` per port, redoing it every time container start/stop/change port, and no way to see which tunnel alive. `autossh` not installed and it not container-aware anyway.

Goal: one Go binary. Point at remote host. It watch remote `docker ps`, open local TCP tunnel for every published container port, close tunnel when container die, survive SSH drops, and show live table in terminal.

Decisions locked by user:
- Discovery: Docker-aware (remote `docker ps`), not `ss` scan
- Language: Go, single static binary, native SSH (no shelling out to `ssh`)
- Monitoring: terminal TUI
- Run mode: foreground process, no systemd
- Hosts: single remote per process
- Auth: ssh-agent + `~/.ssh/config`
- Port conflict: same port if free, else auto-offset from fallback base

## Usage target

```
auto-tunnel myserver              # alias resolved from ~/.ssh/config
auto-tunnel user@10.0.0.5:2222
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `--interval` | `5s` | docker poll period |
| `--bind` | `127.0.0.1` | local listen address |
| `--fallback-base` | `20000` | start of offset range on conflict |
| `--include` / `--exclude` | none | regex on container name |
| `--include-unpublished` | false | also tunnel EXPOSEd-only ports via container IP |
| `--docker-cmd` | `docker ps --format '{{json .}}'` | override for `sudo docker` / `podman` |
| `--log` | `./auto-tunnel.log` | log file (TUI owns stdout) |
| `--no-tui` | false | plain line output, for scripting/debug |

## Layout

```
auto-tunnel/
  go.mod
  main.go                  flags, wiring, signal handling
  internal/
    sshconn/
      dial.go              ssh_config + agent + knownhosts -> *ssh.Client
      keepalive.go         keepalive@openssh.com ping, RTT, reconnect w/ backoff
    discovery/
      docker.go            run remote cmd, parse output
      ports.go             Ports-string parser (unit tested)
      types.go             Container, PortMap
    tunnel/
      manager.go           reconcile desired vs live set
      forwarder.go         listener -> ssh.Dial -> io.Copy + counters
      portalloc.go         same-port-else-offset allocator
    state/
      snapshot.go          immutable snapshot sent to UI
    ui/
      model.go             bubbletea Model, Update, keys
      view.go              lipgloss render, table + header + log pane
```

Deps:
- `golang.org/x/crypto/ssh` + `/agent` + `/knownhosts`
- `github.com/kevinburke/ssh_config`
- `github.com/charmbracelet/bubbletea`, `bubbles/table`, `lipgloss`

## Design

### 1. SSH layer (`internal/sshconn`)

`Dial(target string)`:
1. Parse `user@host:port`. If no user/port, look up alias in `~/.ssh/config` via `ssh_config.Get(alias, "HostName"|"User"|"Port"|"IdentityFile"|"ProxyJump")`.
2. Auth chain, in order: ssh-agent from `$SSH_AUTH_SOCK` -> `IdentityFile` from ssh_config -> `~/.ssh/id_ed25519`, `id_rsa`. Encrypted key with no agent = prompt passphrase before TUI starts.
3. Host key check via `knownhosts.New(~/.ssh/known_hosts)`. Unknown host = hard fail with the fingerprint printed, plus hint to `ssh-keyscan`. No `InsecureIgnoreHostKey`.
4. `ProxyJump` support optional — if present, dial jump host first, `Dial` through it.

`Keepalive` goroutine: every 15s `client.SendRequest("keepalive@openssh.com", true, nil)`, measure RTT. Two consecutive failures = connection dead.

Reconnect: exponential backoff 1s..30s, jitter. On new client, tunnel manager swaps client reference — **local listeners stay open across reconnect**, they just fail dials meanwhile. That keeps local port numbers stable for whatever apps already point at them. State during outage = `DEGRADED`.

### 2. Discovery (`internal/discovery`)

Each tick, one SSH exec session running `docker ps --format '{{json .}}'`. One round trip, no `docker inspect` fan-out.

Parse each JSON line; `Ports` field is a string like:
```
0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp, 8080/tcp
```
Regex `(?:(\[[^\]]+\]|[\d.]+):)?(\d+)->(\d+)/(tcp|udp)` plus bare `(\d+)/(tcp|udp)` for unpublished.

Rules:
- Dedupe by remote host port (IPv4 + IPv6 rows collapse to one).
- `udp` published ports listed in TUI as `UDP (unsupported)` — SSH direct-tcpip is TCP only. Never silently dropped.
- Published port -> tunnel target `127.0.0.1:hostPort` on remote (works even when published to `127.0.0.1` only, since dial originates on the remote).
- Unpublished port, only with `--include-unpublished`: needs container IP, so an extra `docker inspect -f '{{.Id}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}'` for those containers; target `containerIP:containerPort`.
- Apply `--include`/`--exclude` regex on container name.
- Discovery error (docker missing, permission denied) does not kill the process — surfaced as header banner in TUI, existing tunnels stay up.

Output: `[]PortMap{ContainerID, Name, Image, RemotePort, ContainerPort, Proto}`. Stable key = `containerID:containerPort`.

### 3. Tunnel manager (`internal/tunnel`)

Reconcile loop on each discovery result:
- desired keys minus live keys = start
- live keys minus desired keys = stop (close listener, close active conns)
- unchanged = leave alone (never churn a working tunnel)

Port allocation (`portalloc.go`): try `net.Listen(bind, remotePort)`. Success = reservation, no TOCTOU window. On `EADDRINUSE`, walk `fallbackBase..fallbackBase+999` for first free. Remember mapping per key so a container restart reclaims the same local port when possible.

Forwarder per accepted conn:
```
localConn := listener.Accept()
remoteConn := sshClient.Dial("tcp", target)
go copy w/ counters both directions
```
Counters per tunnel: `ActiveConns`, `TotalConns`, `BytesIn`, `BytesOut`, `LastError`. `atomic` for counters, mutex only around the tunnel map.

Tunnel states: `LISTENING`, `ACTIVE` (>=1 conn), `DEGRADED` (SSH down), `ERROR` (bind or dial failed persistently), `STOPPED`.

### 4. TUI (`internal/ui`)

Manager pushes a `state.Snapshot` on a channel every 500ms; `main.go` forwards to `tea.Program.Send`. UI never touches manager internals — snapshot only, so no data race.

Header: `host  •  SSH: connected 4ms  •  up 12m  •  next scan 3s  •  N tunnels`. Red banner line when SSH down or discovery erroring.

Table columns: `CONTAINER | IMAGE | REMOTE | LOCAL | STATE | CONNS | IN | OUT | NOTE`.

Keys: `q` quit (graceful close all), `r` force rescan now, `p` pause/resume selected tunnel, `/` filter rows, `s` cycle sort (name/local port/traffic), `l` toggle log pane, `↑/↓` navigate.

**Logging must go to file, never stdout** — writing to stdout corrupts the bubbletea frame. `--no-tui` flips it back to line-oriented stdout for debugging.

Shutdown: SIGINT/SIGTERM and `q` both run the same path — close listeners, close conns, close SSH, restore terminal.

## Repo setup (step 0)

- `mkdir -p /home/mustiko/claude/auto-tunnel && git init` (dir does not exist yet)
- `.gitignore`: `auto-tunnel`, `*.log`, `.tokensave/`
- Copy this plan to `/home/mustiko/claude/auto-tunnel/PLAN.md` — lives with the code, updated as milestones land
- `README.md` at root: what it does, install/build, usage examples, full flag table, TUI keybindings, SSH auth requirements, Docker permission note, UDP limitation. Written for a user, not a developer — PLAN.md holds the internals. Stub it in milestone 1, fill it out in milestone 7
- Commit per logical iteration, one commit per milestone below (`feat:` / `test:` / `chore:`), message written normal English, not caveman

## Milestones

Each numbered item = one commit.

1. `git init`, `PLAN.md`, `README.md` stub, `.gitignore`, `go mod init`, flag parsing, `sshconn.Dial` + keepalive. Prove: connect and print remote `uname -a`, exit.
2. `discovery` package + `ports.go` parser with table-driven unit tests over real `docker ps` output strings. Prove: `--no-tui` prints discovered port maps each tick.
3. `tunnel` manager + portalloc + forwarder. Prove: `curl localhost:PORT` reaches remote container.
4. Reconcile churn correctness: start/stop containers on remote, tunnels appear/vanish, untouched ones keep their conns.
5. Reconnect: kill network / `ssh` server, confirm backoff, DEGRADED state, listeners survive, recovery re-dials.
6. TUI on top. Keys, sorting, log pane.
7. Polish: `--include-unpublished`, regex filters, full `README.md`, `PLAN.md` updated to match what shipped.

## Verification

- Unit: `go test ./internal/discovery` — Ports-string parser (IPv4+IPv6 dupes, UDP, unpublished, multi-port, empty). `go test ./internal/tunnel` — allocator picks same port, then offset, then rejects exhausted range.
- Manual, needs remote host with Docker:
  1. `go build -o auto-tunnel . && ./auto-tunnel <host> --no-tui` — confirm discovery list matches remote `docker ps`.
  2. `nc -z 127.0.0.1 <local>` / `curl` a real HTTP container. Confirm bytes counters move.
  3. Remote `docker stop <c>` — row disappears within one interval, local port released.
  4. Remote `docker start <c>` — row returns, same local port.
  5. Bind conflict: run local `nc -l 5432` first, start tool, confirm offset port used and shown in TUI.
  6. Kill SSH (`sudo ss -K` on remote or drop wifi) — state DEGRADED, backoff logged, recovers without changing local ports.
  7. `q` and `Ctrl-C` both exit clean, no orphan listeners (`ss -tlnp | grep auto-tunnel` empty).

## Risks

- `docker ps` needs remote user in `docker` group; else `--docker-cmd "sudo docker ps ..."` with NOPASSWD sudo. Error must be readable, not a raw exit-1.
- UDP services can't be tunneled by SSH. Displayed, not forwarded.
- Polling every 5s is N SSH exec sessions/min. Cheap, but `docker events` streaming is the later upgrade if it matters — design keeps discovery behind an interface so swapping is local.
