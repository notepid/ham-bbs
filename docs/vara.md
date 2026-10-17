# VARA modem interface (as used by HamBBS)

HamBBS can run over any modem that offers the VARA TCP host interface:
VARA FM, VARA HF, or VardeFM. The modem handles the radio link, error
correction and retransmission. HamBBS only sees a byte stream.

* **Command port** (default 8300): ASCII lines ending in CR.
* **Data port** (command port + 1): the session's raw bytes.

A modem carries one session at a time.

## Commands HamBBS sends

| Command | When |
|---|---|
| `MYCALL <call> [<call>…]` | after connecting to the modem |
| `LISTEN ON` / `LISTEN OFF` | server: answer calls; client: don't answer |
| `CONNECT <mycall> <target>` | client: call the BBS |
| `DISCONNECT` | end of session, after `BUFFER 0` |
| `ABORT` | when DISCONNECT doesn't finish within a minute |

HamBBS expects `OK` (or `WRONG`) after each command. It tolerates modems
that don't answer.

## Events HamBBS uses

| Event | Meaning |
|---|---|
| `CONNECTED <source> <destination> …` | link up. `source` is the calling station; the server uses it as the verified callsign. |
| `DISCONNECTED` | link down. A failed `CONNECT` also ends with this. |
| `BUFFER <n>` | bytes queued and not yet acknowledged |

HamBBS ignores every other line (`PTT`, `BUSY`, `PENDING`, `IAMALIVE`, …).

## Edge cases the transport handles

* **Stale `BUFFER 0`.** Data and `BUFFER` reports use different sockets, so a
  `BUFFER 0` seen just after a write may predate it. Before disconnecting,
  HamBBS waits for a `BUFFER` report newer than its last write (up to 5 s)
  and then for `BUFFER 0`.
* **Late data.** The last bytes of a session can arrive just after
  `DISCONNECTED`. Reads keep waiting 0.5 s for them.
* **Stale `DISCONNECT`.** A `DISCONNECT` meant for a link that has already
  ended must not affect a new link. Modems should ignore it.

## Testing without a radio

Package `internal/transport/vara/varatest` is a pair of mock modems
connected to each other, with configurable bitrate and latency.
`internal/client/vara_test.go` runs full BBS sessions through it. For
realistic radio conditions (noise, retries, turnarounds), use VardeFM's
virtual radio pair (`vardefm sim-pair`, planned): point the server at one
modem and the client at the other.
