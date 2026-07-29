# Falkn architecture

## Repository boundary

This repository contains the `falkn` CLI, `falknd`, their protocol, and
host-side release tooling. Mobile applications and the hosted notification
relay are implemented and maintained separately.

## Connection and process model

SSH remains the authenticated route into a user's machine. A remote client
sends a bounded, newline-delimited JSON request to the standard input of
`falknd rpc`. The request is carried as SSH channel data rather than in the
remote command or its process arguments. That short-lived command communicates
with the persistent daemon over a user-only Unix socket.

```text
Falkn mobile app -> SSH -> falknd rpc --+
                                         +-> 0600 Unix socket -> persistent falknd
Mac/Linux terminal -> falkn attach ------+                       -> shell PTY -> agent
                                                                 -> agent PTY
```

The RPC client automatically starts the server in a detached Unix session if it is not already running. Its standard streams are redirected to a user-only log, so closing SSH does not retain an SSH channel or send a hangup to the daemon.

The `falkn` CLI uses the same daemon on macOS and Linux. With no arguments it
creates or reconnects to a persistent login shell for the current directory.
Terminal output streams over the Unix socket and input is framed separately;
Ctrl+B then Q detaches only the client. The daemon inspects the shell's process
tree to expose the currently running Codex or Claude process to other clients.
The CLI reserves one local terminal row for a Falkn footer and reports the
remaining native dimensions to the PTY. Child output is passed through
byte-for-byte. The footer's local control sequences are emitted only at complete
ANSI and UTF-8 boundaries, with cursor and rendition state saved and restored,
so it cannot split an agent escape sequence or cancel its colours.

Tagged releases contain `falkn` and `falknd` binaries for macOS and Linux on
ARM64 and x86-64. The installer verifies the release checksum before replacing
both user-local binaries. Replacing the files does not disturb a running daemon.
`falkn daemon restart` loads the installed version only when no sessions are
running. For the one-time upgrade of a daemon that predates the shutdown RPC,
the CLI identifies the process serving the user-owned Unix socket and signals
it only after two session checks confirm that it is idle.

## Daemon responsibilities

`falknd` owns:

- One real PTY and child process per agent session.
- Stable, random `falkn-*` session identifiers.
- Session title, directory, agent, process and lifecycle state.
- A bounded two-megabyte raw-output ring per live session.
- Ordered attachment queues that disconnect a lagging client rather than
  silently dropping bytes from its terminal stream.
- A headless VT500 terminal model with 2,000 lines of scrollback.
- Stable alternate-screen history for transcript clients.
- Literal input, paste handling and a small allowlist of control keys.
- Agent discovery through a modular registry.

Agent resolution merges the user's interactive-shell `PATH` with the daemon's
inherited `PATH`. The resolved absolute executable and that same environment are
used for the child, which supports Node-based CLIs installed through NVM and
similar version managers without launching the agent through a shell.

The terminal model resolves cursor movement, screen clearing, alternate buffers and ANSI control sequences before text reaches a transcript client. This is necessary for full-screen applications such as Claude Code; merely removing escape codes from raw PTY bytes does not reconstruct the visible screen. Because alternate terminal buffers have no native scrollback, `falknd` also journals rows as they leave the live screen and ignores repaints of already-recorded rows. Scrolling inside one attached terminal therefore does not replace the transcript another client is reading.

PTY sizing belongs to an attached terminal client. Opening a conversation in a
mobile app does not resize the shared PTY; the app lays out, wraps, and styles
the stable transcript for its own screen. This keeps a Mac or Linux terminal at
its native dimensions while the mobile app maintains an independent scroll
position. App-created sessions begin with a mobile-friendly 60-by-44 size until
a terminal client attaches.

## Protocol v1

Every request contains:

```json
{
  "version": 1,
  "request_id": "opaque-client-id",
  "method": "list",
  "params": {}
}
```

Every response echoes the version and request ID and contains either `result` or a structured `error`. The initial methods are:

- `preflight`
- `list`
- `directories.list`
- `create`
- `shell.create`
- `agent.start`
- `attach` (switches from JSON to a bounded terminal stream after its response)
- `transcript`
- `resize`
- `stop`
- `delete` (`end` remains a backwards-compatible alias)
- `archive`
- `restore`
- `rename`
- `daemon.shutdown` (refuses while any session is running)
- `send`
- `key`
- `notifications.configure` (handled by the current `falknd rpc` client so an upgraded watcher can attach to an already-running daemon)

`transcript` is a bounded, backwards-compatible window API. A request asks for
the latest line count or supplies `before_line` to page toward older history.
The response includes `start_line`, `end_line`, `total_lines`, `has_earlier`,
and an opaque `history_id`. Clients use that fingerprint to discard stale pages
if the daemon's bounded 2,000-line history advances while they are reading.

The server rejects incompatible protocol versions, unknown methods, missing sessions, unsupported agents, missing directories, oversized prompts and unknown control keys. User titles, directories and prompts are JSON data rather than shell fragments.

New Session can browse the remote filesystem through the read-only
`directories.list` method. It returns folders only and resolves paths using the
same home-relative rules as `create`. Manual paths remain supported and are
validated for existence, directory type, and access before an agent starts.

Session creation also carries a typed permission mode and an array of optional
agent arguments. Full Access maps to each supported agent's documented bypass
flag inside `falknd`. Additional arguments are passed to `exec.Command` as
individual values and never interpolated into a shell command. Known bypass
arguments are rejected unless the client explicitly selects Full Access.

A persistent shell record remembers the last supported agent observed in its
process tree. When that agent exits, `list` and `transcript` report the shell as
`idle`, expose the remembered agent, and advertise whether fresh start and
resume are available. `agent.start` accepts only the session ID, a fresh/resume
choice, and the typed permission mode. It refuses while an agent or any other
foreground process still owns the terminal. The daemon resolves the remembered
executable and constructs the adapter-specific invocation; clients cannot send
an executable, shell command, or bypass flag through this method. Claude resumes
with its current-directory continue flow and Codex resumes the most recent
current-directory conversation.

## Client boundary

The versioned RPC protocol is client-neutral. Mobile implementations are
distributed separately and communicate with `falknd` only through the
documented request, response, transcript, and attach-stream boundaries. This
repository contains no mobile application source, signing material, or store
credentials.

## Reconnection and persistence boundary

The SSH connection is disposable; the daemon-owned PTYs are not. After a remote
client is suspended, closed, moved between networks, or connected through a new
SSH session, it calls `list` and `transcript` to rebuild its visible session
state.

`falknd` persists session metadata, user-edited titles, archive state, and the final transcript of stopped or completed sessions in its user-only cache directory. A host reboot or daemon crash still closes live PTY masters; sessions that were running are therefore restored as stopped. A later milestone can persist agent-native session identifiers and use each adapter's supported resume mechanism after a daemon restart. That is distinct from reconnecting the phone to an already-running daemon.

## Security properties

- The daemon binds only a local Unix socket inside a `0700` user cache directory; the socket is `0600`.
- Network access remains behind SSH and the user's existing Tailscale or network policy.
- RPC requests are limited to one megabyte and responses to four megabytes.
- Prompt input is limited to 256 KiB and terminal history is bounded.
- Agent executables are selected from the registry. Direct sessions use
  `exec.Command`; persistent-shell restarts use a fully quoted invocation made
  only from the daemon-resolved absolute executable and adapter-owned arguments.
- Directories are resolved and validated before process creation.
- Only allowlisted control keys are accepted.

## Notifications

Reliable completion notifications do not depend on a mobile app maintaining a
background SSH connection. A separate `falknd notify-watch` process polls the
daemon's user-only socket, applies the selected agent adapter's attention
detector, and sends an encrypted event to the configured relay:

```text
agent state
  -> falknd watcher
    -> AES-256-GCM encrypted event
      -> Falkn relay
        -> platform notification service
          -> Falkn mobile app decrypts route on tap
```

The mobile app creates a random per-device encryption key and transfers it to
each configured `falknd` over the existing SSH connection. The relay stores a
platform delivery token plus a hash of a random write credential. It sees
delivery metadata and ciphertext, but it cannot read the encrypted host ID,
session ID, session title, agent ID, or attention state. Prompts, source, file
paths, terminal output, SSH credentials, and encryption keys never reach the
relay.

The watcher persists its deduplication state in the user's cache directory. An attention episode produces one notification even if the terminal redraws or changes classification between completion and approval. A successful `send` or actionable control key records local activity; the watcher re-arms after it observes the agent working again. If a very fast turn starts and finishes between polls, a changed transcript after a short grace period releases that new attention event without reopening the duplicate-notification loop.

A notification is how the mobile app reports on work it is already carrying, so
the watcher only considers sessions the app has taken up: one it created, or one
it opened or answered through `create`, `shell.create`, `transcript`, `send`, or
`key`. Those requests arrive through `falkn rpc`, which is the app's entry point
over SSH; the CLI answers its own list, create, and attach requests in process
and never adopts a session on the phone's behalf. A shell created and finished at
the terminal therefore stays silent, and the watcher records its turns as
reported so opening it in the app later cannot replay them. Ending a Falkn shell
is an ordinary way to stop working rather than an agent failure, and produces no
event at all.

Whatever is already settled when a watcher starts is history: its first poll
records every session's state and delivers nothing. An upgrade, a crash, or a
sleeping machine would otherwise announce hours of finished work at once, in the
one moment the watcher is least able to say what just happened. Per-session
activity, presence, and follow markers for sessions the daemon has forgotten are
removed once they are old enough not to belong to a session created since the
poll's own list.

Delivery is suppressed while the relevant mobile conversation is actively polling,
while any `falkn` terminal is attached, or while the session's persistent mute
is enabled. Detaching the final terminal hands notification ownership back to
the mobile app. The attach stream carries a dedicated mute frame for Ctrl+B N;
that control state is session metadata and never reaches the child PTY. The
mobile platform owns notification presentation and companion-device routing for
an eventual alert.
The CLI also refreshes the existing per-session presence marker while attached,
which preserves suppression if an older watcher is still finishing an upgrade.
The installer replaces the standalone watcher with the installed binary, and
current watchers release their singleton lock when that binary changes.

Each registration is one device's claim on this machine's events, and every
registration that survives is another copy of every notification. A phone that
reinstalls Falkn arrives under new relay credentials and a new encryption key,
and it cannot withdraw the ones it left behind: those still resolve to a live
delivery token, so the machine sends the same event twice and the phone can only
decrypt one of them. The app introduces itself to every reachable host each time
it opens, so `notifications.configure` records when a registration was last
refreshed and drops the ones that have gone quiet for thirty days. A
registration saved before Falkn recorded that time is given a full window rather
than expiring on a timestamp it never had. `falkn notifications` lists the
devices a machine sends to, and `falkn notifications forget <device-id|all>`
releases one immediately; the app registers again the next time it opens. A
relay that answers a delivery with 401, 403, 404, or 410 has retired that device
for good, so the watcher releases the registration rather than repeating the
request every two seconds for as long as the session waits.

The hosted default is `https://relay.falkn.dev`. `notifications.configure`
accepts another HTTPS base URL for compatible self-hosted relays. The previous
`https://falkn.punklabs.ai` hostname remains as a compatibility alias for
existing installations.
