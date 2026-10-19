# piknik (fork)

> Fork of [jedisct1/piknik](https://github.com/jedisct1/piknik) — copy/paste
> anything over the network, with end-to-end encryption.

This is an independent fork. It is **not** affiliated with, endorsed by, or
sponsored by the original author.

## What's different in this fork

- Added `-watch` mode: continuously mirrors the OS clipboard to a Piknik
  staging server (automatic copy → sync on supported platforms).
- Added `-watch-interval` to tune the polling interval.
- Linux clipboard backend: `wl-clipboard` on Wayland, `xclip` on X11.
- Windows clipboard backend: PowerShell (`Get-Clipboard` / `Set-Clipboard`).
- Echo/loop suppression so synced content is not pushed back to the server.

## Upstream

- Original project: https://github.com/jedisct1/piknik
- Original author: Frank Denis (@jedisct1)
- Last upstream release: 0.10.2 (2024-12-12); this fork tracks `master`.

## License

This fork is distributed under the same terms as upstream. See
[LICENSE](./LICENSE) and [NOTICE](./NOTICE). Upstream code remains
Copyright (c) 2016-2026, Frank Denis.

## Build

```sh
go build -o piknik .
```

## Usage

Server (on your VPS):

```sh
piknik -server
```

Client, automatic sync:

```sh
piknik -watch
```

Client, manual (upstream behavior):

```sh
wl-paste | piknik -copy
piknik -paste | wl-copy
```
