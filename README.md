# auto-tunnel

Watch a remote server's Docker containers over SSH and automatically forward every
published container port to your local machine. Tunnels appear when containers start,
disappear when they stop, and survive SSH disconnects — with a live terminal dashboard
showing the state of each one.

> Status: in development. See [PLAN.md](PLAN.md) for the design and milestone list.

## Build

```sh
go build -o auto-tunnel .
```

## Usage

```sh
auto-tunnel myserver                 # host alias from ~/.ssh/config
auto-tunnel user@10.0.0.5:2222       # explicit user/host/port
```

Full flag reference and keybindings are documented once the corresponding milestones land.

## Requirements

- SSH access to the remote host, with the key loaded in `ssh-agent` (or referenced by
  `IdentityFile` in `~/.ssh/config`)
- The remote host must be in `~/.ssh/known_hosts` — unknown host keys are rejected
- The remote user must be able to run `docker ps` (typically via membership in the
  `docker` group)

## Limitations

- UDP published ports cannot be forwarded — SSH port forwarding is TCP only. They are
  listed in the dashboard but not tunneled.
