# tsnet-proxy

`tsnet-proxy` is a small OpenSSH `ProxyCommand` helper for reaching a host on
a Tailscale tailnet. It embeds a persistent userspace `tsnet` node and bridges
OpenSSH's stdin/stdout byte stream to the requested TCP destination. It is not
an SSH client or a SOCKS proxy.

It is independent of an installed `tailscaled`: it neither reads nor uses the
local daemon, interfaces, account, routes, or state. The enrolled tsnet node
is a separate tailnet machine, with its own identity, subject to tailnet ACLs/
grants and Tailscale SSH policy.

In particular, an account visible in the installed client's account switcher
belongs to `tailscaled`; this isolated tsnet node cannot and should not import,
switch to, or reuse that client state or credential. First-run authentication
enrolls a separate node, and later runs reuse only this tool's state directory.
Tailscale fast user switching has one active tailnet at a time, so attempting
to switch the installed client would globally disrupt the active work tailnet;
LocalAPI profile switching does not provide a supported credential export, and
copying daemon state would duplicate a live node identity.

## Install and build

This release pins `tailscale.com` v1.102.2, the latest stable tag at release
time (v1.103.0-pre is a prerelease). That module requires Go 1.26.5, so this
repository pins Go 1.26.5; Go's toolchain selection can download it when your
installed Go is older.

```sh
# Preferred: download the archive and `checksums.txt` from the matching GitHub
# release, then verify its SHA-256 before placing the binary on PATH.
# Release assets are not code-signed or notarized yet; build from source when
# you need a locally auditable verification path.

# From a checkout, a plain Go build remains supported:
go build ./cmd/tsnet-proxy
# This stamped build avoids Tailscale's cosmetic `-ERR-BuildInfo` label:
make build BINARY=bin/tsnet-proxy

# Or install the module directly:
go install github.com/gautamg795/tsnet-proxy/cmd/tsnet-proxy@latest
```

Release and Makefile builds stamp the embedded Tailscale metadata as
`1.102.2-tsnet-proxy-0.1.0` (or the supplied `APP_VERSION`). Plain source
builds and `go install` do not add that release stamp, so they may show a
cosmetic development or `1.102.2-ERR-BuildInfo` label when Go build info lacks
a usable VCS revision and timestamp. That label is not a protocol or capability
failure.

For release artifacts without changing your host OS, use:

```sh
GOOS=linux GOARCH=amd64 go build -o tsnet-proxy-linux-amd64 ./cmd/tsnet-proxy
GOOS=darwin GOARCH=arm64 go build -o tsnet-proxy-darwin-arm64 ./cmd/tsnet-proxy
GOOS=windows GOARCH=amd64 go build -o tsnet-proxy.exe ./cmd/tsnet-proxy
# `make cross-build` produces all three under .tmp-build.
```

## First login and persistent state

On the first run without an auth key, run it from a terminal:

```sh
tsnet-proxy my-host.example.ts.net 22
```

It prints the interactive Tailscale login URL/instructions to stderr. Complete
that login, then stop the command. While readiness is pending, the helper
actively checks its embedded tsnet status and surfaces the URL on stderr once
the control plane supplies it; `ssh -v` is not required. The next OpenSSH
invocation reuses the stored identity. This is deliberately non-ephemeral:
enrollment is a one-time step, and an auth key is only an optional first-run
bootstrap, not something to
manage for each `ssh`, `scp`, or `sftp` invocation. Browser login may be quick
when your personal account is already signed in, but it still enrolls this
separate tsnet node.

Default state locations are the platform's Go user-config directory followed
by `tsnet-proxy/personal`: normally `%AppData%\tsnet-proxy\personal` on
Windows, `~/Library/Application Support/tsnet-proxy/personal` on macOS, and
`~/.config/tsnet-proxy/personal` on Linux. Set `--state-dir PATH` (or
`TSNET_PROXY_STATE_DIR`) to choose another location. To reset/re-enroll this
separate node, stop SSH users and remove that *specific* state directory; it
does not alter an installed Tailscale client.

For unattended first enrollment, put an auth key in an environment variable:

```sh
export TS_AUTHKEY='tskey-auth-...'
tsnet-proxy my-host.example.ts.net 22
```

Use `--auth-key-env NAME` or `TSNET_PROXY_AUTH_KEY_ENV` to choose a different
variable. There is intentionally no auth-key command-line flag. The helper
redacts the selected key from logs and errors.

`tsnet` v1.102.2 normally checks ambient `TS_AUTHKEY`, legacy `TS_AUTH_KEY`,
and OAuth's `TS_CLIENT_SECRET` when `Server.AuthKey` is empty. `tsnet-proxy`
temporarily suppresses all of those implicit credential fallbacks while its
server runs: only the variable selected by `--auth-key-env` can bootstrap this
node. This prevents a custom empty choice from unexpectedly consuming a shell
credential, while still allowing the interactive login flow.

### Disposable ephemeral nodes

Use `--ephemeral` (or `TSNET_PROXY_EPHEMERAL=true`) only for short-lived,
untrusted, or automation-specific connections:

```sh
export TS_AUTHKEY='tskey-auth-...'
tsnet-proxy --ephemeral my-host.example.ts.net 22
```

Ephemeral mode requires a nonempty value in the selected `--auth-key-env` on
every invocation and does not offer interactive login. Generate a reusable
auth key with **Ephemeral** enabled; a one-off key can enroll only one process.
Each concurrent SSH invocation creates a separate ephemeral node and IP, and
ephemeral-minute plan limits may apply. Normal exit asks Tailscale to remove
the node promptly; a crash or `SIGKILL` can leave it visible until Tailscale's
inactivity cleanup (currently roughly 30–60 minutes, subject to change).

Ephemeral identity state uses memory only and never reads or overwrites the
regular persistent state directory. OAuth-based ephemeral provisioning is not
configured in v1; provide an auth key via the selected environment variable.

## OpenSSH configuration

macOS/Linux (`~/.ssh/config`):

```
Host tail-*
  ProxyCommand /usr/local/bin/tsnet-proxy %h %p
```

Windows (`%USERPROFILE%/.ssh/config`), including a path containing spaces:

```
Host tail-*
  ProxyCommand "C:/Program Files/tsnet-proxy/tsnet-proxy.exe" %h %p
```

Use forward slashes and quote the executable path exactly as above. If your
OpenSSH build's command parser needs it, quote the entire command with its
native shell rules rather than putting status output on stdout.

Then normal OpenSSH workflows work unchanged:

```sh
ssh tail-db
scp ./backup.sql tail-db:/tmp/
sftp tail-db
ssh -L 5432:database.tailnet.ts.net:5432 tail-bastion
```

`HOST` may be a MagicDNS name, Tailscale IPv4 address, or IPv6 address. The
tool receives the final `HOST PORT` from OpenSSH and dials it through tsnet.

## Configuration

```
tsnet-proxy [flags] HOST PORT
  --state-dir PATH
  --hostname NAME
  --auth-key-env NAME
  --connect-timeout DURATION    # default 30s
  --verbose
  --ephemeral
  --version
```

Environment values seed the matching flag defaults, so explicit flags win:
`TSNET_PROXY_STATE_DIR`, `TSNET_PROXY_HOSTNAME`,
`TSNET_PROXY_AUTH_KEY_ENV`, `TSNET_PROXY_CONNECT_TIMEOUT`, and
`TSNET_PROXY_VERBOSE`, and `TSNET_PROXY_EPHEMERAL`. The default hostname
derives from the local hostname with `-tsnet-proxy`, normalized to a lowercase
DNS-compatible label.

## Security and policy

Protect the state directory like a credential: it holds the tsnet machine
identity and is created/tightened to mode 0700 where POSIX modes apply. Keep
auth keys in a protected environment or secret manager and rotate them under
your tailnet's policy. Do not share state directories between users or hosts.
The helper sends all diagnostics, debug logs, and interactive URLs to stderr;
stdout is reserved exclusively for SSH protocol bytes.

As part of normal tsnet behavior, diagnostic logs may be uploaded to Tailscale
under Tailscale's standard logging and privacy boundary. `--verbose` controls
the additional local debug diagnostics written to stderr.

Tailnet ACLs/grants must permit this enrolled tsnet node to reach the target
and port. Tailscale SSH policy is additionally relevant when the destination
uses Tailscale SSH; this program only transports a TCP connection and cannot
bypass either control plane policy.

## Troubleshooting

- **MagicDNS name fails:** verify the separate tsnet identity can resolve and
  reach it, and try the target's Tailscale IP to distinguish DNS from policy.
- **Policy denied:** inspect ACL/grant rules using the tsnet node's identity,
  not the identity of an installed local Tailscale client.
- **Expired or deleted identity:** remove only this helper's state directory
  and run interactively again, or supply a valid `TS_AUTHKEY` for enrollment.
- **Login URL is hidden:** run manually in a terminal. It is deliberately on
  stderr so it never corrupts OpenSSH's stdout protocol stream. The helper
  actively prints it while waiting once Tailscale provides it; `ssh -v` is not
  required. If no URL arrives, check network access to Tailscale's control
  plane and the readiness timeout.
- **Timeout:** increase `--connect-timeout` after checking connectivity,
  enrollment, and control-plane policy; it applies independently to readiness
  and the subsequent dial.
- **Windows quoting:** use `C:/...` and quote paths with spaces as in the SSH
  config example. Confirm with a direct command before relying on ProxyCommand.
