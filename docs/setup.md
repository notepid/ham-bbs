# Setting up HamBBS

## Server (sysop)

1. Put `hambbs-server` (`hambbs-server.exe` on Windows) in a folder of its
   own and run it from a terminal:

   ```
   hambbs-server
   ```

   On the first start it asks for your callsign, a node name, the ports and
   an optional admin password, then writes `hambbs.toml` next to itself and
   starts.

2. Open the admin UI at <http://127.0.0.1:8080/>. It shows connected users,
   activity, users and messages.

3. Users connect to TCP port 6300 with the HamBBS client, or with
   `telnet yourhost 6300`.

Useful commands:

| Command | What it does |
|---------|--------------|
| `hambbs-server -init` | run the setup wizard again |
| `hambbs-server -set-admin-password` | protect the admin UI (user `sysop`) |
| `hambbs-server -config /path/hambbs.toml` | use another configuration file |

All settings live in `hambbs.toml`, which has comments. Restart after
editing it. Data lives in `data/hambbs.db` (SQLite). Back it up by copying it
while the server is stopped.

### Running as a service

**Linux (systemd)**, `/etc/systemd/system/hambbs.service`:

```ini
[Unit]
Description=HamBBS
After=network.target

[Service]
WorkingDirectory=/opt/hambbs
ExecStart=/opt/hambbs/hambbs-server
Restart=on-failure
User=hambbs

[Install]
WantedBy=multi-user.target
```

**Windows:** use the Task Scheduler ("At startup", start in the folder that
holds `hambbs.toml`) or a service wrapper such as NSSM.

### Accounts

With `open_registration = true` (the default), a new user picks a password
in the client and the first sync creates the account. With registration
closed, add users in the admin UI.

Clearing a user's password in the admin UI lets their next client login set
a new one. Use this when someone forgets their password.

## Client (user)

1. Run `hambbs-client`. It opens your browser on <http://127.0.0.1:8081/>.
2. In **Settings**, enter your callsign, choose a password and enter the BBS
   address (for example `bbs.example.org:6300`).
3. Press **Sync now**. Write and read offline; each sync sends your outbox
   and fetches what's new.

`hambbs-client -sync` syncs once without the UI (for scripts or scheduled
tasks). `-no-browser` starts the UI without opening a browser.

## Radio links

### VARA modems (VARA FM, VARA HF, VardeFM)

Server: start the modem, then enable it in `hambbs.toml` and restart.

```toml
[vara]
enabled = true
addr = "127.0.0.1:8300"   # modem command port; data port is 8301
mycalls = ["LA1ABC"]      # callsigns the BBS answers to
```

The BBS keeps trying to reach the modem until it is up, so the order you
start them in doesn't matter.

Client: in **Settings**, choose *Radio via VARA modem*. Enter the BBS
**callsign** as the address and the modem's command port (normally
`127.0.0.1:8300`).

Radio users are identified by the callsign of the radio link, the same as
on any packet BBS. Sync clients also sign in with their password-derived
key, and the server checks that the two callsigns match. See
[vara.md](vara.md) for the interface details.

### AX.25 packet (Direwolf, UZ7HO soundmodem)

For VHF/UHF packet at 1200 or 9600 baud. Any TNC with an AGWPE interface
works: Direwolf (Linux, Windows, Raspberry Pi) or UZ7HO soundmodem
(Windows). A minimal `direwolf.conf`:

```
ADEVICE plughw:1,0     # your sound card (Windows: ADEVICE 1 1, or leave out for the default)
CHANNEL 0
MYCALL LA1ABC
MODEM 1200
PTT /dev/ttyUSB0 RTS   # or PTT CM108 for USB sound card interfaces, or VOX
AGWPORT 8000
```

Server: enable AX.25 in `hambbs.toml` and restart.

```toml
[ax25]
enabled = true
addr = "127.0.0.1:8000"   # Direwolf's AGWPORT
port = 0                  # radio port: 0 = Direwolf channel 0
mycalls = ["LA1ABC"]      # callsigns the BBS answers to, e.g. ["LA1ABC-1"]
```

The BBS registers these callsigns with the TNC, so Direwolf passes their
calls on to it. Several users can be connected at once. As with VARA, the
BBS keeps trying until the TNC is up.

Client: run your own TNC (Direwolf or soundmodem with its AGW port on).
In **Settings**, choose *Packet radio via Direwolf or another AGWPE TNC*.
Enter the BBS **callsign** as the address and the TNC's AGWPE port
(normally `127.0.0.1:8000`). Through digipeaters:
`LA1ABC via LA2DIG,LA3DIG`.

Anyone with a plain packet terminal can also connect and use the BBS's
text commands. See [ax25.md](ax25.md) for the interface details.

## Forwarding between BBS nodes

BBS nodes can exchange bulletins and personal mail with their neighbours.
Each pair of neighbours lists each other in `hambbs.toml`, with the same
link password on both sides:

```toml
[[neighbor]]
node = "LA9BBS"                # their node name
call = "LA9ABC"                # their callsign (default: node)
password = "shared secret"     # the same on both nodes; never sent over the air
connect = "ax25:LA9ABC-1"      # how to call them; empty = they call us
interval = 60                  # minutes between calls (0 = only "Forward now")
routes = ["*"]                 # personal mail for these nodes goes via them
scopes = ["*"]                 # bulletin scopes they get
```

* `connect` is `tcp:host:port`, `ax25:CALL [via DIGI,...]` or `vara:CALL`.
  The AX.25 and VARA transports must be enabled; the BBS calls out through
  the same TNC or modem it answers on. Over VARA, a call waits until the
  modem is free.
* One side calling is enough: a session sends both ways. On a radio link,
  let one node call and leave `connect` empty on the other.
* **Bulletins:** posted as `AREA@SCOPE` (for example `TECH@ALL`) they flood
  to every neighbour whose `scopes` match (`*` = all). A bulletin without a
  scope stays on the BBS where it was posted.
* **Personal mail:** users address it as `CALL@NODE`. A neighbour gets
  mail for its own node name and for nodes matching its `routes` (`*`
  wildcards allowed, `"*"` alone = default route). Users cannot send to a
  node without a route. Forwarded mail without a route is shown as *held*
  on the admin **Neighbours** page and goes out after a matching route is
  added and the BBS restarted.
* Messages are de-duplicated by ID and never go back to a node they came
  through, so loops in the network are harmless.

The admin **Neighbours** page shows queues, last results and errors, and
has a **Forward now** button. Restart the BBS after changing neighbours.
The protocol is described in [protocol.md](protocol.md) §7.

## Legal checklist

* No encryption is used. The protocol is public (`docs/protocol.md`).
* The station identifies with its callsign in the banner of every session.
* Check your national rules for **unattended/automatic stations**,
  especially on HF, and for third-party traffic. As sysop you are
  responsible for content transmitted by your station; the admin UI lets you
  delete messages.
