# HamBBS

A modern, easy-to-run bulletin board and message system for amateur radio.

* **Two binaries, no dependencies:** `hambbs-server` and `hambbs-client` for
  Linux, Windows and Raspberry Pi. SQLite is built in.
* **Setup in a minute:** the server asks four questions on first start.
  Users configure the client in its browser UI.
* **Built for slow links:** clients sync in one batch per session (one link
  turnaround) and you read and write offline. A typical sync is a few
  hundred bytes.
* **Radio and internet:** AX.25 packet through Direwolf or UZ7HO
  soundmodem, VARA FM/HF and [VardeFM](https://github.com/notepid/vardeFM)
  modems, and TCP/IP.
* **Works with a plain terminal too:** connect with telnet or a packet
  terminal to get a classic BBS command line.
* **Ham-legal:** no encryption, publicly documented protocol
  ([docs/protocol.md](docs/protocol.md)). Login uses signatures, so no
  password ever goes over the air.

## Status

| Phase | Content | State |
|-------|---------|-------|
| 1 | Core over TCP: store, sync protocol, terminal mode, admin UI, client UI | **done** |
| 2a | Radio: VARA modems (VARA FM/HF, VardeFM) | **done** (tested with mock modem) |
| 2b | Radio: AX.25 via Direwolf/AGWPE (VHF/UHF) | **done** (tested with mock TNC) |
| 3 | Forwarding between nodes (bulletin flooding, mail routing) | **done** |
| 4 | VARA/ARDOP/FreeDATA, FBB/B2F gateway, message signatures | later |

## VardeFM: a faster, open modem for FM

[VardeFM](https://github.com/notepid/vardeFM) is a sister project: an
open-source, high-speed data modem for FM radios, written for Windows,
Linux and Raspberry Pi. It turns a sound card interface and an ordinary FM
radio into a link of up to 7.4 kbit/s through the mic/speaker jacks, or
18.7 kbit/s through a 9600 data port. Classic packet runs at 1200 bit/s
on the air, and less in practice.

VardeFM offers the same TCP interface as VARA FM, so HamBBS uses it through
its VARA transport; nothing VardeFM-specific is needed. Unlike VARA, its
waveform and protocol are openly documented. Both stations need VardeFM: it
can't talk to VARA FM over the air. See
[docs/setup.md](docs/setup.md#vardefm).

## Download

Prebuilt binaries for Linux (x86-64, ARM64, ARMv6 for every Raspberry Pi),
Windows and macOS are on the [Releases](https://github.com/notepid/ham-bbs/releases) page.
Users download `hambbs-client_<version>_<system>`, sysops
`hambbs-server_<version>_<system>`. Both come from the same release and
always share a version. To update, replace the program file and keep
its configuration and data folder; see
[docs/setup.md](docs/setup.md#updating).

With Go installed you can also run:

```
go install github.com/notepid/ham-bbs/cmd/...@latest
```

## Quick start

```
go build ./cmd/...
./hambbs-server          # first start runs the setup wizard
./hambbs-client          # opens http://127.0.0.1:8081/
```

See [docs/setup.md](docs/setup.md).

## Layout

```
cmd/hambbs-server      server binary
cmd/hambbs-client      client binary
internal/proto         wire protocol (frames, banner, line reader)
internal/bbs           server core: sync sessions, terminal mode, rules
internal/client        client sync engine
internal/store         SQLite storage (server and client)
internal/transport     link abstraction; tcp/, vara/ (+ varatest mock modem pair),
                       agwpe/ (AX.25 via Direwolf; + agwpetest mock TNCs)
internal/identity      Ed25519 login keys derived from callsign+password
internal/web           admin and client web UIs (embedded templates)
internal/config        TOML config and setup wizard
```

## Development

CI (`.github/workflows/ci.yml`) runs the following on every push and pull request:
- gofmt, vet and tests on Linux and Windows, plus the race detector on Linux
- a snapshot build of all release targets

To release, push a version tag. GoReleaser builds and publishes the GitHub
release:

```
git tag v0.1.0
git push origin v0.1.0
```

Tags like `v0.2.0-rc1` become pre-releases. `hambbs-server -version` shows
the version a binary was built from.

```
go test ./...
GOOS=linux GOARCH=arm64 go build ./cmd/...   # e.g. Raspberry Pi
```

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
