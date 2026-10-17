# HamBBS

A modern, easy-to-run bulletin board and message system for amateur radio.

* **Two binaries, no dependencies:** `hambbs-server` and `hambbs-client` for
  Linux, Windows and Raspberry Pi. SQLite is built in.
* **Setup in a minute:** the server asks four questions on first start.
  Users configure the client in its browser UI.
* **Built for slow links:** clients sync in one batch per session (one link
  turnaround) and you read and write offline. A typical sync is a few
  hundred bytes.
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
| 2b | Radio: AX.25 via Direwolf/AGWPE (VHF/UHF) | next |
| 3 | Forwarding between nodes (bulletin flooding, mail routing) | planned |
| 4 | VARA/ARDOP/FreeDATA, FBB/B2F gateway, message signatures | later |

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
internal/transport     link abstraction; tcp/, vara/ (+ varatest mock modem pair)
internal/identity      Ed25519 login keys derived from callsign+password
internal/web           admin and client web UIs (embedded templates)
internal/config        TOML config and setup wizard
```

## Development

```
go test ./...
GOOS=linux GOARCH=arm64 go build ./cmd/...   # e.g. Raspberry Pi
```
