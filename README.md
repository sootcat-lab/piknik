# piknik — automatic, end-to-end encrypted clipboard sync

![Piknik](./piknik.png)

Fork of [jedisct1/piknik](https://github.com/jedisct1/piknik) that adds a
`-watch` mode: the clipboard syncs by itself between Linux, Windows and macOS,
through a relay server you control. Android (Termux) uses manual push/pull.

The wire protocol and crypto are unchanged; fork and upstream clients and
servers interoperate. This is an independent fork, not affiliated with or
endorsed by the original author.

## Features

- **Two-way sync**: local changes are pushed, remote changes applied, with no
  echo loops. Copying an earlier value again still syncs.
- **Low idle cost**: the local clipboard is read only when it changes (Wayland,
  Windows, X11 with `clipnotify`), the server is polled with backoff, and
  unchanged server content is not downloaded again.
- **Skips secrets**: content marked by password managers is never pushed
  (`x-kde-passwordManagerHint` on Linux; the clipboard history and cloud
  clipboard exclusion markers on Windows).
- **Safe by default**: text only. Remote data that is not valid text or a
  well-formed image is dropped, never pasted. Images are opt-in
  (`-watch-images`).
- **Windows**: non-ASCII text survives any console code page.

| Platform | Text | Images | Needs |
| --- | --- | --- | --- |
| Linux (Wayland) | ✔ | ✔ | `wl-clipboard` |
| Linux (X11) | ✔ | ✔ | `xclip`, optionally `clipnotify` |
| Windows | ✔ | ✔ | nothing (PowerShell) |
| macOS | ✔ | — | nothing (`pbpaste`/`pbcopy`) |
| Android (Termux) | manual | — | `termux-api` |

## Setup

Build (Go 1.24+):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o piknik .
```

Generate keys with `piknik -genkeys` and copy each section where it belongs.

Server, `/etc/piknik/piknik.toml`:

```toml
Listen = "0.0.0.0:8075"
Psk    = "..."
SignPk = "..."
```

Client, `~/.piknik.toml` (`~/piknik.toml` on Windows):

```toml
Connect   = "your.server:8075"
Psk       = "..."
SignPk    = "..."
SignSk    = "..."
EncryptSk = "..."
```

`SignSk` and `EncryptSk` are client secrets: keep them off the server. Keep
config files at mode `600`.

Run:

```sh
piknik -server -config /etc/piknik/piknik.toml   # relay
piknik -watch                                     # each desktop
piknik -watch -watch-images                       # with image sync
```

Manual mode still works, including on Android:

```sh
piknik -copy < file          # or: termux-clipboard-get | piknik -copy
piknik -paste > file         # or: piknik -paste | termux-clipboard-set
```

## Flags added by this fork

| Flag | Default | Meaning |
| --- | --- | --- |
| `-watch` | | sync the clipboard continuously |
| `-watch-interval` | `700ms` | base interval for clipboard and server checks |
| `-watch-images` | `false` | also sync images |

See `piknik -h` for the upstream flags.

## Running as a service

Client, `~/.config/systemd/user/piknik-watch.service`:

```ini
[Unit]
Description=Piknik clipboard watcher
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/piknik -watch -config %h/.piknik.toml
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now piknik-watch
```

The server unit is the same with `-server -config /etc/piknik/piknik.toml`,
as a system service.

## How `-watch` works

- A local change is pushed if the server does not already hold it; a server
  change is applied if the clipboard does not already hold it.
- At startup the current clipboard is recorded, not pushed, so a restart never
  overwrites newer content.
- The server is polled every `-watch-interval` after a change, backing off to
  4× while idle. The server sends a signature ahead of the content, so an
  unchanged value is not downloaded: with a 5.9 MB image stored, idle traffic
  is about 30 KB/s instead of 2.3 MB/s.
- Images travel as one ASCII line, `PiknikImage1 <mime> <base64>`, which
  clients without image sync ignore.

## Troubleshooting

- **`the server rejected the connection`**: usually a `Psk` mismatch between
  client and server, or the wrong service on that port.
- **Nothing syncs**: the watcher logs errors and keeps retrying; check
  `journalctl --user -u piknik-watch`.
- **Which build is running?** The version string does not change between fork
  builds; compare the hash of the running binary
  (`sha256sum /proc/<pid>/exe`, or `Get-FileHash` on Windows).
- **Updating on Windows**: a running `piknik.exe` is locked. Download the new
  build under another name, stop every `piknik` process, then replace it.

## Security and limitations

- Content is encrypted end to end (XChaCha20, Ed25519); the relay sees only
  ciphertext.
- Every device holding the keys receives what you copy and can write to your
  clipboard. Share keys only between your own devices.
- macOS cannot detect password-manager markers, so secrets copied there sync.
- One clipboard slot, no history; when two devices copy at once, the last push
  wins.
- On GNOME, X11 without `clipnotify`, and macOS, the clipboard is read on every
  tick.

## Development

```sh
go test -race ./...    # sync logic and a real server round trip
./test-watch.sh        # end to end on Wayland
```

`test-watch.sh` uses your real clipboard: stop any running watcher first, or
the test data reaches your real server.

## License

Based on [piknik](https://github.com/jedisct1/piknik) by Frank Denis. See
[LICENSE](./LICENSE) (labeled ISC; the text is BSD 2-Clause) and
[NOTICE](./NOTICE).
