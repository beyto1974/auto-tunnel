# Security policy

## Reporting a vulnerability

Report it through GitHub, on this repository — there is no email contact:

- **Preferred:** [open a private security advisory](https://github.com/beyto1974/auto-tunnel/security/advisories/new).
  Only the maintainers see it, and it stays private until a fix is released.
- If private reporting is unavailable to you, open a normal
  [issue](https://github.com/beyto1974/auto-tunnel/issues) saying only that you have a
  security report and how to reach you. **Do not put the details in a public issue.**

Useful things to include: what an attacker gains, the auto-tunnel version
(`auto-tunnel -version`), the flags in use, and the smallest reproduction you have.
Scrub hostnames, usernames, and container names before attaching anything — the log
file records all three.

Expect an acknowledgement within a week. This is a spare-time project, not a product with
an on-call rota, so please allow reasonable time for a fix before disclosing publicly.

## Supported versions

Only the latest release. Fixes go into a new release rather than back into older tags.

## Scope

auto-tunnel is a local command-line tool. It holds an SSH connection to a host you already
have access to, runs read-only commands there (`docker ps`, `ss`), and binds local
listeners. Reports that concern that trust boundary are in scope — for example: remote
output escaping into your terminal or a remote command line, host key verification being
bypassable, forwarded ports reachable from further than `-bind` promises, or the files
under `$XDG_CONFIG_HOME/auto-tunnel/` being written with permissions that leak them.

Out of scope, because they are the documented behaviour rather than a flaw:

- Forwarded ports carry no authentication of their own; they inherit whatever the remote
  service does. `-bind` on a non-loopback address exposes them to that network, and
  auto-tunnel warns at startup when you ask for it.
- `-host-ports all` forwards every listening service on the remote host, including ones
  its operator deliberately kept on loopback.
- `-include-unpublished` reaches container ports that were never published.
- HTTP probing sends one `HEAD /` to each port you forward, without verifying TLS
  certificates. `-probe-http=false` turns it off.
- `-docker-cmd`, `-docker-inspect-cmd`, and `-host-cmd` run whatever you put in them on
  the remote host. They are your commands, run as you.

See the [Security section of the README](README.md#security) for the whole threat model.
