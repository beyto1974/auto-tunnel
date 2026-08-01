# auto-tunnel

[![CI](https://github.com/beyto1974/auto-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/beyto1974/auto-tunnel/actions/workflows/ci.yml)
![coverage](coverage.svg)

Watch a remote server's Docker containers over SSH and automatically forward every
published container port to your local machine. Tunnels appear when containers start,
disappear when they stop, and survive SSH disconnects — with a live terminal dashboard
showing the state of each one.

```
auto-tunnel · deploy@10.0.0.5:22 · ssh connected 4ms · up 12m · next scan 3s · 3 container(s) · 3 tunnel(s): 1 active, 1 idle, 0 degraded, 1 broken

STATE        CONTAINER              IMAGE             LOCAL        REMOTE                   CONNS         IN        OUT
LISTENING    db                     postgres:16       25432        127.0.0.1:5432               0         0B         0B
UNSUPPORTED  dns                    coredns:latest    -            127.0.0.1:53                 0         0B         0B
ACTIVE       web                    nginx:alpine      8080         127.0.0.1:8080               2      2.0KB       512B

↑/↓ select · p pause · r rescan · s sort (name) · / filter · l log · q quit
```

## Build

```sh
go build -o auto-tunnel .
```

## Usage

```sh
auto-tunnel myserver                 # host alias from ~/.ssh/config
auto-tunnel deploy@10.0.0.5:2222     # explicit user/host/port
auto-tunnel myserver -include '^api' # only containers whose name starts with api
```

It runs in the foreground. `q` or `Ctrl-C` shuts everything down cleanly.

## How it works

Every `-interval` (5s by default) auto-tunnel runs `docker ps` on the remote host over
the existing SSH connection and turns the published ports into local listeners:

- **Local port choice.** It tries the same port number the container publishes on the
  remote (remote 5432 → local 5432). If that port is already busy locally, it takes the
  next free port from `-fallback-base` onwards (20000+) and shows the real mapping in the
  table. A container that restarts keeps the local port it had, even if its published
  port changed — so anything already pointed at that port keeps working.
- **Reconciliation.** Only genuinely new, removed, or re-targeted ports cause a change.
  Unrelated container churn never disturbs a working tunnel.
- **Disconnects.** If SSH drops, local ports stay bound (shown as `DEGRADED`) while the
  connection is retried with exponential backoff. Local port numbers never move under
  your feet, and traffic resumes as soon as the link is back.

### Tunnel states

| State | Meaning |
|---|---|
| `ACTIVE` | At least one connection is currently flowing |
| `LISTENING` | Local port is bound and idle |
| `DEGRADED` | Port is bound but SSH is down, so new connections cannot get through yet |
| `PAUSED` | You stopped this tunnel from accepting connections (`p`) |
| `ERROR` | No local port could be bound; the reason is shown under the table |
| `UNSUPPORTED` | Discovered but not forwardable — a UDP or SCTP port |

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-interval` | `5s` | How often to poll the remote Docker daemon |
| `-bind` | `127.0.0.1` | Local address to bind forwarded ports on (see [Security](#security) before changing it) |
| `-fallback-base` | `20000` | First port tried when the preferred local port is taken |
| `-include` | none | Only forward containers whose name matches this regexp |
| `-exclude` | none | Never forward containers whose name matches this regexp |
| `-include-unpublished` | `false` | Also forward `EXPOSE`d-but-unpublished ports, via the container IP — these are ports the remote operator chose *not* to publish |
| `-docker-cmd` | `docker ps --format '{{json .}}'` | Remote command listing containers as JSON |
| `-docker-inspect-cmd` | `docker inspect --format '…'` | Remote command prefix used to resolve container IPs |
| `-connect-timeout` | `10s` | SSH connect timeout |
| `-log` | `auto-tunnel.log` | Log file path (`-` writes to stderr, requires `-no-tui`) |
| `-no-tui` | `false` | Print plain text instead of the live dashboard |
| `-verbose` | `false` | Log at debug level |

## Keys

| Key | Action |
|---|---|
| `↑` / `↓` (or `k` / `j`) | Move the selection |
| `g` / `G` | Jump to the first / last row |
| `p` | Pause or resume the selected tunnel |
| `r` | Force an immediate rescan |
| `s` | Cycle sorting: name → local port → traffic |
| `/` | Filter rows (enter applies, esc clears) |
| `l` | Toggle the log pane |
| `q` / `Ctrl-C` | Quit |

## Requirements

- SSH access to the remote host, with the key loaded in `ssh-agent` (or referenced by
  `IdentityFile` in `~/.ssh/config`). Host aliases, `User`, `Port`, `IdentityFile`, and
  single-hop `ProxyJump` are read from `~/.ssh/config`.
- The remote host must already be in `~/.ssh/known_hosts`. Unknown host keys are
  rejected rather than trusted on first sight; the error tells you the fingerprint and
  the `ssh-keyscan` command to accept it.
- The remote user must be able to run `docker ps`, normally via membership in the
  `docker` group. If it needs sudo:

  ```sh
  auto-tunnel myserver \
    -docker-cmd "sudo docker ps --format '{{json .}}'" \
    -docker-inspect-cmd "sudo docker inspect --format '{{.Id}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}'"
  ```

  (That needs passwordless sudo on the remote — there is no prompt to answer.)

## Security

- **Forwarded ports carry no authentication of their own.** They inherit whatever the
  remote service does. The default `-bind 127.0.0.1` keeps them on your machine; any
  other bind address republishes every discovered remote service to that network, and
  auto-tunnel warns at startup when you do it.
- **`-include-unpublished` overrides the remote's exposure policy.** Those ports were
  deliberately left unpublished on the remote; forwarding them reaches services their
  operator did not intend to expose.
- **Host keys are never trusted on first sight.** An unknown host is a hard error with
  the fingerprint and the `ssh-keyscan` line to accept it, because unattended
  forwarding is exactly where a silent MITM would matter.
- **The log file records your infrastructure**: remote host, login user, SSH port, and
  every container name discovered there. It is created `0600`, and the default path is
  `./auto-tunnel.log` — scrub it before attaching it to a bug report.
- **Remote output is treated as untrusted.** Container names, images, and remote stderr
  are stripped of terminal control sequences before they reach your terminal or the
  log, and container IDs are validated before they can appear on a remote command line
  (that command may be prefixed with `sudo`).

## Limitations

- **UDP and SCTP cannot be forwarded.** SSH port forwarding is TCP only. Those ports are
  listed as `UNSUPPORTED` rather than silently dropped.
- **One remote host per process.** Run a second instance for a second host.
- **Published port ranges are capped at 64 ports** per range, so one careless `-p` on the
  remote cannot open hundreds of local listeners.
- Chained `ProxyJump` (`a,b`) is not supported; a single jump host is.

## Development

```sh
go test ./...                 # unit tests, no remote host needed
go test ./... -race
./scripts/coverage-badge.sh   # refresh coverage.svg; CI fails if it is stale

go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...             # also run on every push by CI
```

The design and the milestone history live in [PLAN.md](PLAN.md).
