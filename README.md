# auto-tunnel

[![CI](https://github.com/beyto1974/auto-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/beyto1974/auto-tunnel/actions/workflows/ci.yml)
![coverage](coverage.svg)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Watch a remote server's Docker containers over SSH and automatically forward every
published container port to your local machine. Tunnels appear when containers start,
disappear when they stop, and survive SSH disconnects — with a live terminal dashboard
showing the state of each one.

It forwards more than containers: the ports the remote host itself is listening on can be
listed and picked from the dashboard, and any port at all can be declared on the command
line with `-forward`.

```
auto-tunnel · deploy@10.0.0.5:22 · ssh connected 4ms · up 12m · next scan 3s · 3 container(s) · 5 tunnel(s): 1 active, 2 idle, 0 degraded, 1 broken · 1 available

STATE        SRC    NAME                DETAIL            LOCAL        REMOTE / URL               CONNS         IN        OUT
LISTENING    docker db                  postgres:16       25432        127.0.0.1:5432                 0         0B         0B
UNSUPPORTED  docker dns                 coredns:latest    -            127.0.0.1:53                   0         0B         0B
ACTIVE       docker web                 nginx:alpine      8080         http://127.0.0.1:8080          2      2.0KB       512B
AVAILABLE    host   node_exporter       10.0.0.5          -            10.0.0.5:9100                  0         0B         0B
LISTENING    static prod-db             prod-db=5432@db1  15432        db1:5432                       0         0B         0B
  open: http://127.0.0.1:8080  (o)   via 127.0.0.1:8080

↑/↓ select · enter on/off · o open · f forwarded (off) · a all shown · t source (all) · p pause · r rescan · s sort (name) · / filter · l log · q quit
```

## Install

Linux and macOS, amd64 and arm64. Windows is not supported: auto-tunnel finds your key through
`SSH_AUTH_SOCK`, which Windows OpenSSH does not set — it exposes the agent as a named pipe instead.

### Prebuilt binary

Archive names carry no version, so `latest/download` always points at the newest release:

```sh
curl -fsSL https://github.com/beyto1974/auto-tunnel/releases/latest/download/auto-tunnel_linux_amd64.tar.gz \
  | tar -xz -C ~/.local/bin auto-tunnel
```

Swap in `linux_arm64`, `darwin_amd64`, or `darwin_arm64`, or let the shell work it out:

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
curl -fsSL "https://github.com/beyto1974/auto-tunnel/releases/latest/download/auto-tunnel_${os}_${arch}.tar.gz" \
  | tar -xz -C ~/.local/bin auto-tunnel
```

The binaries are statically linked and need no Go toolchain. Every `v*` tag also publishes a
`checksums.txt` alongside the archives on the
[releases page](https://github.com/beyto1974/auto-tunnel/releases); to check a download against it, keep
the archive instead of piping it straight into `tar`:

```sh
base=https://github.com/beyto1974/auto-tunnel/releases/latest/download
curl -fsSLO "$base/auto-tunnel_linux_amd64.tar.gz"
curl -fsSL "$base/checksums.txt" | sha256sum -c --ignore-missing
tar -xzf auto-tunnel_linux_amd64.tar.gz -C ~/.local/bin auto-tunnel
```

### With the Go toolchain

```sh
go install github.com/beyto1974/auto-tunnel@latest
```

### From source

```sh
go build -o auto-tunnel .
```

`auto-tunnel -version` prints the build version, whichever way you installed it.

## Usage

```sh
auto-tunnel myserver                 # host alias from ~/.ssh/config
auto-tunnel deploy@10.0.0.5:2222     # explicit user/host/port
auto-tunnel myserver -include '^api' # only containers whose name starts with api

auto-tunnel myserver -forward 15432:5432       # a forward that discovery cannot take away
auto-tunnel myserver -host-ports off           # containers only, do not list host sockets
auto-tunnel myserver -no-docker -host-ports all -host-exclude '^(22|25)$'
```

It runs in the foreground. `q` or `Ctrl-C` shuts everything down cleanly.

## Sources

Every row on the dashboard comes from one of three sources, shown in the `SRC` column.
`t` cycles the table between them, `f` hides everything not actually being forwarded, and
`a` switches every row currently shown on or off. The three narrow independently, so
`f` + `t` is "the host ports I actually tunnel".

| Source | Where it comes from | Shown | Forwarded by default |
|---|---|---|---|
| `docker` | Remote `docker ps` | Always | Yes |
| `host` | The sockets the remote host is listening on (`ss`, falling back to `netstat`) | Always | No — press `enter` |
| `static` | Declared with `-forward` | Always | Yes, always — no discovery can remove it |

**Host ports** are listed but not forwarded (`-host-ports select`, the default). They
appear as `AVAILABLE` with no local port: press `enter` on a row to start it, `enter`
again to stop it. `-host-ports all` forwards every one it finds; `-host-ports off` stops
asking the remote host what it is listening on at all. Docker's own published ports are
filtered out of this list, so a container port never appears twice.

Your picks are remembered between runs in one file per remote target, under
`$XDG_CONFIG_HOME/auto-tunnel/` (`~/.config/auto-tunnel/` if that is unset):

```
~/.config/auto-tunnel/deploy@10.0.0.5_22.json
```

One file *per target*, because the keys inside are not host-qualified — a shared file
would apply one host's picks to every other host. Nothing is written until you actually
switch a row on or off, so a run that never touches the keys leaves no file behind.
`-selection <path>` puts it somewhere else; `-selection off` remembers nothing.

Dashboard settings — currently the sort order — live beside it in
`auto-tunnel/view.json`. That one is *not* per target: a sort order is a habit, not a
property of a host. `-selection off` switches this file off too.

**Declared forwards** need nothing running on the other end and never disappear:

```sh
-forward 5432                   # local 5432  -> remote 127.0.0.1:5432
-forward 15432:5432             # local 15432 -> remote 127.0.0.1:5432
-forward 8000-8010              # a range, one for one
-forward 15432-15442:8000-8010  # two ranges, which must be the same length
-forward prod-db=5432@db1       # a named forward dialed at db1 from the remote side
```

The flag is repeatable, and a bad spec is a startup error rather than a row that never
appears. Ranges obey the same 64-port cap as published container ranges.

`-no-docker` skips Docker discovery altogether, for a host that runs none.

## How it works

Every `-interval` (5s by default) auto-tunnel polls the remote host over the existing SSH
connection and turns what it finds into local listeners:

- **Local port choice.** It tries the same port number the container publishes on the
  remote (remote 5432 → local 5432). If that port is already busy locally, it takes the
  next free port from `-fallback-base` onwards (20000+) and shows the real mapping in the
  table. A container that restarts keeps the local port it had, even if its published
  port changed — so anything already pointed at that port keeps working.
- **Reconciliation.** Only genuinely new, removed, or re-targeted ports cause a change.
  Unrelated container churn never disturbs a working tunnel. A source that fails a poll
  contributes what it last reported, so one bad `docker ps` cannot drop your tunnels —
  or take the declared forwards, which depend on no remote command, down with it.
- **Disconnects.** If SSH drops, local ports stay bound (shown as `DEGRADED`) while the
  connection is retried with exponential backoff. Local port numbers never move under
  your feet, and traffic resumes as soon as the link is back.

### Web services

Every port that is actually forwarded is asked, once, whether it speaks HTTP: a TLS
handshake, then a single `HEAD /`, over the SSH connection. A port that answers shows its
URL in the `REMOTE / URL` column in place of the address it points at — the far side moves
to the line under the table, beside the `o` hint.

The URL is plain text, not an OSC 8 hyperlink. That escape carries no style of its own and
every terminal that honours it underlines the link, which draws a dotted rule through the
table. Terminals that detect bare URLs make it clickable anyway, and `o` opens the selected
row through `xdg-open`/`open` regardless.

Only forwarded ports are probed — an `AVAILABLE` row is never touched, so browsing the
list sends nothing anywhere. `-probe-http=false` turns the probe off entirely.

### Tunnel states

| State | Meaning |
|---|---|
| `ACTIVE` | At least one connection is currently flowing |
| `LISTENING` | Local port is bound and idle |
| `DEGRADED` | Port is bound but SSH is down, so new connections cannot get through yet |
| `PAUSED` | You stopped this tunnel from accepting connections (`p`) |
| `ERROR` | No local port could be bound; the reason is shown under the table |
| `UNSUPPORTED` | Discovered but not forwardable — a UDP or SCTP port |
| `AVAILABLE` | Discovered but not forwarded; press `enter` to start it |

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
| `-no-docker` | `false` | Skip Docker discovery entirely, for a host that runs none |
| `-host-ports` | `select` | What to do with the host's own listening ports: `select` (list them, forward on request), `all` (forward them; see [Security](#security)), or `off` (never ask) |
| `-host-cmd` | `ss -Hltnp \|\| netstat -tlnp` | Remote command listing listening sockets |
| `-host-exclude` | none | Never forward host ports whose process name or port matches this regexp |
| `-forward` | none | Declare a forward discovery cannot remove; repeatable (see [Sources](#sources)) |
| `-probe-http` | `true` | Ask each forwarded port whether it speaks HTTP, so the dashboard can offer a URL |
| `-selection` | `$XDG_CONFIG_HOME/auto-tunnel/<target>.json` | Where the forwarding picks are remembered; `off` remembers nothing |
| `-connect-timeout` | `10s` | SSH connect timeout |
| `-log` | `auto-tunnel.log` | Log file path (`-` writes to stderr, requires `-no-tui`) |
| `-no-tui` | `false` | Print plain text instead of the live dashboard |
| `-verbose` | `false` | Log at debug level |
| `-version` | — | Print the version and exit |

## Keys

| Key | Action |
|---|---|
| `↑` / `↓` (or `k` / `j`) | Move the selection |
| `g` / `G` | Jump to the first / last row |
| `enter` / `space` | Start or stop forwarding the selected row |
| `o` | Open the selected row in a browser, when it is a web service |
| `a` | Switch every row currently shown on — or off, once they are all on |
| `f` | Show only the rows being forwarded, hiding every `AVAILABLE` candidate |
| `t` | Cycle the table by source: all → docker → host → static |
| `p` | Pause or resume the selected tunnel |
| `r` | Force an immediate rescan |
| `s` | Cycle sorting: name → local port → traffic → remote/url |
| `/` | Filter rows (enter applies, esc clears) |
| `l` | Toggle the log pane |
| `q` / `esc` / `Ctrl-C` | Quit |

The sort order is remembered between runs; `f`, `t` and `/` reset every time, since a
filter you forgot you set is worse than one you have to set again.

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

Found a vulnerability? [SECURITY.md](SECURITY.md) says how to report it — privately,
through this repository.

- **Forwarded ports carry no authentication of their own.** They inherit whatever the
  remote service does. The default `-bind 127.0.0.1` keeps them on your machine; any
  other bind address republishes every discovered remote service to that network, and
  auto-tunnel warns at startup when you do it.
- **`-include-unpublished` overrides the remote's exposure policy.** Those ports were
  deliberately left unpublished on the remote; forwarding them reaches services their
  operator did not intend to expose.
- **Host ports are every service on the box, not only the published ones** — databases,
  metrics agents, and admin interfaces the operator deliberately left on loopback. The
  default `select` *lists* them and forwards none of them, so seeing a row costs nothing;
  `all` forwards them all and warns at startup. The SSH port you are connected through is
  always skipped. Combining `-host-ports all` with a non-loopback `-bind` republishes the
  whole remote host to your network. `-host-ports off` stops the listing entirely, for a
  host where even enumerating services is more than you want to do.
- **Probing sends a real request to the services you forward.** Each forwarded port gets
  one TLS handshake and one `HEAD /`, once, from the remote side. Ports you have not
  forwarded are never touched. The probe carries no credentials and reads only the status
  line, but it does appear in the service's access log, and the TLS certificate is
  deliberately not verified — the point is to identify a service, not to trust it, and a
  self-signed certificate is the normal case behind a tunnel. `-probe-http=false` turns
  it off entirely.
- **The selection file records what you chose to forward**, keyed by container and
  service name — so it names remote infrastructure, like the log does. It is written
  `0600` in a `0700` directory, and only once you actually pick something. `-selection
  off` if you would rather nothing were kept.
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
  remote cannot open hundreds of local listeners. `-forward` ranges obey the same cap.
- **Host port discovery needs `ss` or `netstat` on the remote.** Without privilege they
  still list every socket, only without process names, so rows appear as `port 5432`
  instead of `postgres`.
- **Host ports are matched by number.** A service that moves to a different port comes
  back as a new row, and an `AVAILABLE` choice does not follow it.
- **A port is probed once.** A service that starts speaking HTTP later keeps its row
  without a URL until the tunnel is restarted, and one that stops keeps the URL.
- **HTTP/2-only endpoints read as not-HTTP.** The probe offers `http/1.1` in ALPN and
  sends an HTTP/1.1 request, so a TLS endpoint that refuses everything else gets no URL.
- **The URL points at the local port**, which is not always the remote number: if the
  preferred port was busy, the URL carries the fallback port the tunnel actually bound.
- Chained `ProxyJump` (`a,b`) is not supported; a single jump host is.

## Development

```sh
gofmt -l .                    # must print nothing
go vet ./...
go test ./...                 # unit tests, no remote host needed
go test ./... -race
./scripts/coverage-badge.sh   # refresh coverage.svg; CI fails if it is stale

go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...             # also run on every push by CI
```

The design and the milestone history live in [PLAN.md](PLAN.md).

## License

[MIT](LICENSE).
