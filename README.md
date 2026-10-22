# piknik — automatic, end-to-end encrypted clipboard sync

![Piknik](./piknik.png)

> Fork of [jedisct1/piknik](https://github.com/jedisct1/piknik) that adds
> **automatic clipboard sync** between Linux, Windows, macOS and Android
> (Termux), relayed through a server you control.

Upstream piknik is deliberately manual: you push with `pkc`, pull with `pkp`.
This fork keeps the wire protocol, the crypto and the server untouched, and
adds a `-watch` mode that mirrors the OS clipboard in both directions.

This is an independent fork. It is **not** affiliated with, endorsed by, or
sponsored by the original author.

## What this fork adds

- **`-watch` mode** — poll the local clipboard, push changes to the staging
  server, and apply remote changes locally, without any command being typed.
- **`-watch-interval`** — polling interval (default `700ms`).
- **`-watch-images`** — opt-in clipboard image sync (see [Images](#images)).
- **Clipboard backends**:
  - Linux / Wayland: `wl-paste`, `wl-copy`
  - Linux / X11: `xclip`
  - Windows: PowerShell `Get-Clipboard` / `Set-Clipboard`
  - macOS: `pbpaste` / `pbcopy`
- **Echo suppression** — content this client pushed is never applied back to
  it, and content applied from the server is never pushed back. Recently synced
  values are tracked as a TTL- and size-bounded set (not a single "last value"),
  so peers bouncing several values in a row cannot cause a loop.
- **Text-only by default, binary firewall** — unless image sync is enabled, the
  watched clipboard is only read when it holds valid UTF-8 text, and remote
  content that is neither valid UTF-8 text nor a well-formed image envelope is
  dropped instead of being written to the clipboard. A peer that truncates
  binary at its first NUL byte can no longer clobber your clipboard.
- **Windows encoding fix** — clipboard text is read and written as raw UTF-8
  bytes through the standard streams, so non-ASCII text is no longer mangled by
  a GBK/CP936 console code page.
- **Refactor** — copy/paste operations now accept `io.Reader`/`io.Writer` and
  return errors (`runClientIO`), so they can be driven from the watch loop.
  The wire protocol and the crypto are unchanged.

## Platform support

| Platform | Text sync | Image sync | Notes |
| --- | --- | --- | --- |
| Linux (Wayland) | Yes | Yes | needs `wl-clipboard` |
| Linux (X11) | Yes | Yes | needs `xclip` |
| Windows | Yes | Yes | PowerShell + WinForms |
| macOS | Yes | No | `pbpaste` / `pbcopy` are text only |
| Android (Termux) | Manual | — | `termux-api`; no watch backend |

`-watch` is polling-based. Android background clipboard access is restricted by
the OS, so the phone uses the manual push/pull commands instead.

## Images

Sync is text-only unless `-watch-images` is passed:

```sh
piknik -watch -watch-images
```

Images travel as a single ASCII line, so they survive the text-only channel and
the protocol stays untouched:

```
PiknikImage1 image/png iVBORw0KGgoAAAANSUhEUg...
```

A client that does not understand the envelope, or that runs without
`-watch-images`, ignores it and leaves its clipboard alone. The MIME type is
re-derived from the image bytes on receipt, so a tampered or truncated payload
is rejected rather than pasted.

Limits worth knowing: the staging server stores one value, and large
screenshots (multi-MB PNGs) are base64-inflated by ~33% on the wire. Keep
`-watch-interval` at its default when pasting big images, so the initial read is
not racing the next poll.

## Build

Go 1.24 or newer:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o piknik .
```

Static binaries are recommended for deployment, since they do not depend on the
server's glibc version.

## Quick start

### 1. Generate keys

```sh
piknik -genkeys
```

The output contains a client section, a server section and a hybrid section.
Only copy the section you need; the layout is described under [Setup](#setup).

### 2. Staging server

Create `/etc/piknik/piknik.toml` from the **server** section:

```toml
Listen = "0.0.0.0:8075"
Psk    = "..."
SignPk = "..."
```

```sh
chmod 600 /etc/piknik/piknik.toml
piknik -server -config /etc/piknik/piknik.toml
```

The server only relays encrypted bytes. Open the chosen TCP port in your
firewall / cloud security group.

### 3. Desktop client

Create `~/.piknik.toml` from the **client** section:

```toml
Connect   = "your.server:8075"
Psk       = "..."
SignPk    = "..."
SignSk    = "..."
EncryptSk = "..."
```

```sh
chmod 600 ~/.piknik.toml
piknik -watch
```

On Windows the default config path is `~/piknik.toml`.

### 4. Manual mode

The original commands still work:

```sh
piknik -copy < file.bin
piknik -paste > file.bin
```

Aliases:

```sh
alias pkc='piknik -copy'
alias pkp='piknik -paste'
```

### 5. Android (Termux)

```sh
pkg install termux-api
termux-clipboard-get | piknik -copy     # push
piknik -paste | termux-clipboard-set    # pull
```

## Setup

Piknik requires a set of keys. Generate them with:

```sh
piknik -genkeys
```

The default config path is `~/.piknik.toml` (`~/piknik.toml` on Windows); use
`-config` to override it.

Server config (`/etc/piknik/piknik.toml`):

```toml
Listen = "0.0.0.0:8075"
Psk    = "bf82bab384697243fbf616d3428477a563e33268f0f2307dd14e7245dd8c995d"
SignPk = "0c41ca9b0a1b5fe4daae789534e72329a93a352a6ad73d6f1d368d8eff37271c"

# Optional streaming limits (defaults shown):
# MaxStreamBytes    = 10737418240
# MaxStreamDuration = 86400
# MaxWaitingPullers = 100
```

Client config (`~/.piknik.toml`):

```toml
Connect   = "your.server:8075"
Psk       = "bf82bab384697243fbf616d3428477a563e33268f0f2307dd14e7245dd8c995d"
SignPk    = "0c41ca9b0a1b5fe4daae789534e72329a93a352a6ad73d6f1d368d8eff37271c"
SignSk    = "cecf1d92052f7ba87da36ac3e4a745b64ade8f9e908e52b4f7cd41235dfe7481"
EncryptSk = "2f530eb85e59c1977fce726df9f87345206f2a3d40bf91f9e0e9eeec2c59a3e4"
```

> These are example keys. Generate your own with `piknik -genkeys`.
>
> `SignSk` and `EncryptSk` are client secrets. Keep them out of the server
> config and out of version control. The server only needs `Psk` and `SignPk`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-copy` | | store stdin (encrypted) in the clipboard slot |
| `-paste` | ✔ | write the clipboard slot to stdout |
| `-move` | | paste, then delete the slot |
| `-push` / `-pull` | | stream stdin to pullers / receive one stream |
| `-cid` | | label binding a push to its pullers |
| `-watch` | | continuously mirror the local clipboard |
| `-watch-interval` | `700ms` | clipboard polling interval |
| `-watch-images` | `false` | also sync clipboard images |
| `-server` | | run the staging server |
| `-genkeys` | | generate a key set |
| `-password` | | derive keys from a password instead of random keys |
| `-config` | `~/.piknik.toml` | config file |
| `-maxclients` | `10` | server: maximum simultaneous connections |
| `-maxlen` | `0` | server: max content length in MB (`0` = unlimited) |
| `-timeout` | `10` | connection timeout, seconds |
| `-datatimeout` | `3600` | data transmission timeout, seconds |
| `-version` | | print the version |

## Running as a service

Server (`/etc/systemd/system/piknik.service`):

```ini
[Unit]
Description=Piknik staging server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/piknik -server -config /etc/piknik/piknik.toml
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

Client (`~/.config/systemd/user/piknik-watch.service`):

```ini
[Unit]
Description=Piknik clipboard watcher
After=graphical-session.target

[Service]
Type=simple
ExecStart=%h/.local/bin/piknik -watch -watch-interval 700ms -config %h/.piknik.toml
# Add -watch-images if you want image sync.
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now piknik-watch
journalctl --user -u piknik-watch -f
```

## Troubleshooting

`the server rejected the connection - check that it is running the same Piknik
version or retry later` means the TCP connection opened but the handshake
failed. The server closes the socket without a message when the `Psk` does not
match, when the client speaks an unsupported protocol version (only 6 and 7 are
accepted), or when it is over `-maxclients`. In practice:

1. Make sure `Psk` (and `SignPk`) in the client config match the server config.
2. Compare `piknik -version` on both ends — protocol version must be 6 or 7.
3. Look at the server log: `journalctl -u piknik -n 50` (`Unsupported client
   version` and connection errors are logged there).
4. Confirm you are reaching the piknik server and not some other service on
   that port: a wrong `Psk` produces the exact same client error.

Note that `piknik -watch` logs and keeps retrying, so a silently rejected
server connection shows up as "nothing syncs" rather than an error dialog.

## Security

- Clipboard content is encrypted end-to-end with XChaCha20 and signed with
  Ed25519. The staging server relays ciphertext and cannot read it.
- Clients authenticate to the server with the shared `Psk`.
- A public staging port is therefore acceptable, but restricting it to your
  known source IPs is still recommended.
- Keep configuration files at mode `600`.

## Limitations

- Polling, not event-driven.
- Single clipboard slot: no history. Concurrent copies on two devices race.
- Windows spawns PowerShell on every poll; replace with a native clipboard
  library if wakeups matter.
- macOS image sync is not implemented (`pbpaste` / `pbcopy` are text only).
- Android is manual push/pull only.
- Streaming (`-push` / `-pull`) still uses `os.Stdin` / `os.Stdout`; only
  copy/paste were refactored for the watch loop.

## Upstream and license

Based on [piknik](https://github.com/jedisct1/piknik) by Frank Denis
(@jedisct1), Copyright (c) 2016-2026.

Licensed under the terms in [LICENSE](./LICENSE) (the file is labeled ISC; the
text is the BSD 2-Clause license). See [NOTICE](./NOTICE).
