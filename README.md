# Falkn

**Persistent terminal sessions for coding agents, on machines you already own.**

- **sessions outlive the terminal** — detach, close the laptop, reattach later from another terminal or over SSH. The agent keeps working.
- **the real terminal, not an interpretation** — the PTY stream is passed through byte for byte, so full-screen agents keep their colour, cursor movement, and alternate screen.
- **one prefix key** — `Control-\` for Falkn, every other key straight to the session.
- **no network listener** — the daemon binds a user-only Unix socket. Remote clients arrive through the host's own SSH service.
- **notifications when you have walked away** — optional, end-to-end encrypted, and quiet about anything you are watching yourself.
- **two Go binaries** — no tmux, no multiplexer, no runtime.

## install

```sh
curl -fsSL https://falkn.dev/install.sh | sh
```

The installer detects macOS or Linux on ARM64 or x86-64, downloads the matching
release, verifies its SHA-256 checksum, and installs `falkn` and `falknd` in
`~/.local/bin`. Building from source works too — see [development](#development).

## use

Start Falkn where the work lives:

```sh
cd ~/work/project
falkn
```

That creates a persistent login shell for the directory, or reconnects to the
one already there. Run Codex or Claude Code inside it as you normally would.

`Control-\` then `Q` detaches without stopping anything. Running `falkn` again
in the same directory picks the session back up.

```sh
falkn new "session name"     # another session in the same directory
falkn list                   # every session on this machine
falkn attach <session-id>
falkn notifications          # devices this machine sends events to
falkn daemon status          # client and daemon versions
falkn daemon restart         # load an installed update once sessions are idle
```

## hotkeys

Falkn reserves one control character, `Control-\`, and passes every other key
through. It is the escape byte used by `dtach` and `abduco`, chosen because
shells, editors, and coding agents leave it alone — unlike `Control-B`, which
belongs to tmux and to several terminal clients.

| Keys | Effect |
| --- | --- |
| `Control-\` then `Q` | Detach; the session keeps running |
| `Control-\` then `N` | Create and switch to a new session |
| `Control-\` then `M` | Mute or unmute notifications for the session |
| `Control-\` then `↑` | Open the session switcher |
| `Control-\` then `←` / `→` | Switch to the previous or next session |
| `Control-\` then `1`–`9` | Switch to a session by number |
| `Control-\` twice | Send a literal `Control-\` to the session |

The switcher lists running sessions, flagged `[w]` waiting on you, `[r]`
running, `[d]` done, `[!]` failed. It shrinks the session's terminal rather than
drawing over it, so an agent redraws into the smaller area and is restored when
the switcher closes.

Set `FALKN_PREFIX` to reassign the prefix, for example `FALKN_PREFIX=ctrl-a`.
`falkn --help` lists the hotkeys for whichever prefix is in effect.

## how it works

```text
Remote client -> SSH -> falknd rpc --+
                                     +-> user-only Unix socket -> falknd
Local terminal -> falkn -------------+                         -> PTY -> shell or agent
```

`falknd` owns the pseudo-terminals, child processes, bounded scrollback, and
session metadata, and keeps them alive across client disconnections. It reads
the shell's process tree to report which agent is running in it. Clients speak a
versioned JSON protocol over the socket; a remote client sends one bounded,
Base64-encoded request as an argument to `falknd rpc` over SSH.

Sessions survive client disconnections, not host reboots: a restart closes the
PTY masters, and sessions that were running are restored as stopped with their
final transcript intact.

## remote access

Falkn opens no port of its own, so reaching a machine from outside its network
is the host's problem rather than Falkn's. Put the machines on a WireGuard mesh
such as Tailscale and use the mesh address; SSH then works the same on cellular
as it does at home, and no traffic passes through anyone else's infrastructure.

## notifications

The optional watcher sends encrypted attention events to a relay, which forwards
them to a paired mobile app. Prompts, source code, file paths, terminal output,
SSH credentials, and encryption keys never reach the relay: it holds a delivery
token, a hash of a write credential, and ciphertext it cannot read.

Events are scoped to the sessions the app is carrying — the ones it started or
opened. A shell you create and finish at the terminal is never announced
elsewhere, and ending it is not an event. Delivery also stays quiet while a
terminal is attached, while the app is reading the session, and while the
session is muted.

`falkn notifications` lists the devices a machine sends to, and
`falkn notifications forget <device-id|all>` releases one. The hosted default is
`https://relay.falkn.dev`; a compatible self-hosted relay can be configured
through the protocol.

## what is here

`falkn`, `falknd`, the client protocol, and the installation and release
tooling. The mobile applications and the hosted relay are maintained separately
and are not part of this repository; they communicate with `falknd` only through
the documented protocol.

## docs

- [Architecture and protocol](docs/ARCHITECTURE.md)
- [Third-party dependencies](docs/THIRD_PARTY.md)

## development

Go 1.25.1 or later:

```sh
go test ./...
go build ./cmd/falkn
go build ./cmd/falknd
```

Release archives for every supported platform, with a checksum manifest:

```sh
sh scripts/package-release.sh v0.1.0 dist
```

## license

Apache License 2.0 — see [LICENSE](LICENSE). Dependencies and their licences are
listed in [docs/THIRD_PARTY.md](docs/THIRD_PARTY.md).
