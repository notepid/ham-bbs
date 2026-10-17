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

### VardeFM

[VardeFM](https://github.com/notepid/vardeFM) is an open-source data modem
for FM radios, from the same author as HamBBS. It runs on Windows, Linux,
macOS and Raspberry Pi, and offers VARA FM's TCP interface, so HamBBS uses it
exactly like the VARA modem above:

```
radio ── sound card interface ── vardefm ──(TCP 8300/8301)── hambbs-server
                                                             or hambbs-client
```

VardeFM sends the audio and keys the radio (PTT). It also handles
connecting, error correction and retries, and HamBBS sees a reliable
connection to the other station. Each station runs its own VardeFM: the BBS
next to the server, and each user next to the client.

| | 1200 baud packet (Direwolf) | VardeFM narrow | VardeFM wide |
|---|---|---|---|
| Radio connection | mic/speaker | mic/speaker | 9600 data port |
| Speed | about 1.2 kbit/s | 0.7 – 7.4 kbit/s | 1.9 – 18.7 kbit/s |
| Other side needs | any packet TNC or terminal | VardeFM | VardeFM |
| Digipeaters | yes | no | no |

VardeFM adjusts its speed to the signal quality. It does not work with VARA
FM over the air, so the BBS and its users must all use VardeFM to reach
each other this way. A BBS can offer packet and VardeFM at the same time
(`[ax25]` and `[vara]` both enabled), but each needs its own radio and
frequency.

VardeFM is included in every HamBBS download: the `vardefm` program sits
next to `hambbs-server` or `hambbs-client`, and its user guide is in
`vardefm-docs/`. The Linux `armv6` download carries the VardeFM build for
Raspberry Pi 2 and newer, and Windows on ARM gets the x86-64 build. You
can also get it separately from its
[releases page](https://github.com/notepid/vardeFM/releases).

To set it up:

1. Connect the radio and set up PTT as described in VardeFM's user guide
   (`vardefm-docs/README.md`). `vardefm devices` lists your sound cards and
   PTT devices.
2. Server: enable `[vara]` in `hambbs.toml` as shown above, with
   `addr = "127.0.0.1:8300"`. To have the BBS start VardeFM, add for
   example
   `autorun = "vardefm modem --audio-in USB --audio-out USB --ptt cm108"`
   (see [below](#starting-the-modem-or-tnc-automatically)).
3. Client: in **Settings**, choose *Radio via VARA modem*. Enter the BBS
   callsign as the address and `127.0.0.1:8300` as the modem. Fill in
   *VARA modem command* to have the client start VardeFM; the **List sound
   cards and PTT devices** button shows the names to use.

Neighbouring BBSes can forward over VardeFM too: `connect = "vara:LA9ABC"`.

Without radios, VardeFM's `sim-pair` runs two modems joined by a simulated
FM link, which is handy for trying out a BBS and a client on one computer.
See [vara.md](vara.md#testing-without-a-radio).

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

### Starting the modem or TNC automatically

HamBBS can start the modem or TNC program for you, so there is only one
program to start. This is off unless you set a command.

Server, in `hambbs.toml`:

```toml
[vara]
enabled = true
autorun = "vardefm modem --audio-in USB --audio-out USB --ptt cm108"

[ax25]
enabled = true
autorun = "direwolf -c /etc/direwolf.conf -t 0"
```

Client: in **Settings**, under *Start the modem or TNC with the client*.
Only the command for the chosen connection type runs.

* The command is a normal command line. Put quotes around arguments with
  spaces. In `hambbs.toml`, wrap a command with Windows paths in single
  quotes, so the backslashes stay as they are:
  `autorun = '"C:\Program Files\direwolf\direwolf.exe" -t 0'`.
* A plain `vardefm` means the copy next to the HamBBS program. Other
  names are looked up on the PATH.
* HamBBS starts the program before it connects, restarts it if it stops
  (after 2 s, then waiting longer each time up to a minute), and stops it
  when HamBBS stops. Its output goes to the HamBBS log, and the admin
  dashboard and the client's Settings page show its status and last lines.
* HamBBS stops the program the way Ctrl-C would, so it can release PTT.
  When HamBBS runs without a console window (as a Windows service or
  scheduled task), it can only end the program forcefully. Check that your
  PTT interface lets go of the radio when that happens.
* If HamBBS crashes, the operating system ends the program too (Linux and
  Windows).
* `hambbs-client -sync` starts the program, syncs and stops it again.

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

### Routes and scopes by example

A small network: two Norwegian BBSes hang off a hub, and the hub has a
link to Sweden that leads to the rest of the world.

```
  LA1OSL ──┐
           ├── LA2HUB ── SM5BBS ── (rest of the world)
  LA9BRG ──┘
     │
  LA9XYZ, LA9QRS   (small BBSes that only talk to LA9BRG)
```

**A BBS with a single neighbour** (LA1OSL here) sends everything it can't
deliver itself to that neighbour:

```toml
# hambbs.toml on LA1OSL
[[neighbor]]
node = "LA2HUB"
password = "osl-hub secret"
connect = "tcp:hub.example.org:6300"
interval = 30
routes = ["*"]       # default route: all mail for other nodes goes here
scopes = ["*"]       # and all scoped bulletins
```

**The hub** needs a line per neighbour:

```toml
# hambbs.toml on LA2HUB
[[neighbor]]
node = "LA1OSL"
password = "osl-hub secret"   # the same as on LA1OSL
# no connect: LA1OSL calls us
# no routes: mail for LA1OSL itself goes there anyway
scopes = ["*"]

[[neighbor]]
node = "LA9BRG"
call = "LA9ABC"
password = "brg-hub secret"
connect = "ax25:LA9ABC-1"
interval = 60
routes = ["LA9*"]             # the small LA9 BBSes behind LA9BRG
scopes = ["ALL", "LA"]        # network-wide and Norwegian bulletins only

[[neighbor]]
node = "SM5BBS"
password = "hub-sm5 secret"
connect = "tcp:sm5bbs.example.se:6300"
interval = 60
routes = ["*"]                # everything else
scopes = ["ALL", "EU"]
```

Where the hub sends things:

| Address | Goes to | Why |
|---------|---------|-----|
| `LA1ABC@LA1OSL` | LA1OSL | a neighbour's own node name |
| `LA1ABC@LA1OSL.OSL.NOR.EU` | LA1OSL | the first part of the address counts |
| `LA9ZZ@LA9XYZ` | LA9BRG | matches its route `LA9*` |
| `SM0AAA@SK0BBS` | SM5BBS | no other match: default route `*` |
| `LA2ZZ@LA2HUB`, `LA2ZZ` | stays | for the hub's own users |
| bulletin `TECH` | stays | no scope: only on the BBS where it was posted |
| bulletin `TECH@ALL` | all three | every neighbour's scopes match `ALL` |
| bulletin `TECH@LA` | LA1OSL, LA9BRG | SM5BBS only takes `ALL` and `EU` |
| bulletin `TECH@EU` | LA1OSL, SM5BBS | LA9BRG only takes `ALL` and `LA` |

Nothing is sent back to the node a message came from, or to any other node
it has already passed through.

How a BBS picks the neighbour for personal mail to `CALL@NODE`:

1. A neighbour whose `node` is `NODE` (only the part before the first dot
   counts, so `LA1OSL.OSL.NOR.EU` means `LA1OSL`).
2. Otherwise the first neighbour, in file order, with a `routes` pattern
   that matches. `*` matches any characters and `?` one character. The
   pattern is checked against the whole address after `@`, so `*.SE` or
   `*.EU` work for hierarchical addresses.
3. Otherwise the neighbour with the default route `"*"`.

Bulletin `scopes` use the same patterns on the part after `@`. `"*"` takes
every scope. A neighbour without `scopes` in the file gets every scope.

Tips:

* Give each BBS **at most one default route**, pointing towards the
  bigger network. If two BBSes have default routes pointing at each
  other, mail for an unknown node is not sent back and forth: it stops at
  the second one and shows up as *held* on its **Neighbours** page.
* A user can only post mail to a node that their BBS has some route to.
  On a BBS with a default route, everything is accepted, and mail for a
  node nobody knows ends up *held* at the last BBS on the way.
* Agree on the bulletin scopes in your network (for example `ALL`, a
  country prefix like `LA`, a region like `EU`), and use them in
  `scopes`.

The admin **Neighbours** page shows queues, last results and errors, and
has a **Forward now** button. Restart the BBS after changing neighbours.
The protocol is described in [protocol.md](protocol.md) §7.

## Updating

The program files hold no data. Settings and messages live in files next
to them, so updating means replacing the program and keeping everything
else. Releases are on the
[Releases](https://github.com/notepid/ham-bbs/releases) page.

### Server

1. Download the new `hambbs-server` archive for your system.
2. Stop the server: `sudo systemctl stop hambbs`, stop the scheduled task,
   or press Ctrl+C in its window.
3. Back up `hambbs.toml` and the `data` folder.
4. Replace `hambbs-server` (`hambbs-server.exe`) with the new one, and
   `vardefm` and `vardefm-docs` if you use them. Leave `hambbs.toml` and
   `data` where they are.
5. Start the server again. It updates the database by itself on start.
   `hambbs-server -version` shows the version you are running.

On Linux with the systemd setup above:

```
sudo systemctl stop hambbs
sudo cp -a /opt/hambbs/data /opt/hambbs/data.bak
sudo tar -xzf hambbs-server_*_linux_arm64.tar.gz -C /opt/hambbs hambbs-server vardefm vardefm-docs
sudo systemctl start hambbs
```

### Client

1. Download the new `hambbs-client` archive for your system.
2. Close the client: press Ctrl+C in its window, or close the window.
3. Replace `hambbs-client` (`hambbs-client.exe`) with the new one, in the
   same folder. Replace `vardefm` and `vardefm-docs` too, if you use them. `hambbs-client.toml` and the `client-data` folder (your
   messages and outbox) stay as they are.
4. Start it again.

### Good to know

* Clients and servers of different versions work together as long as they
  speak the same protocol version (currently 1). Users and neighbouring
  BBSes don't have to update at the same time as you.
* Settings that are new in a release get their default value if they are
  missing from your configuration file. The release notes say when one is
  worth setting.
* To go back to an older version, stop the program, put back the old
  program file, and restore your backup. An older version may not
  understand data written by a newer one.

## Legal checklist

* No encryption is used. The protocol is public (`docs/protocol.md`).
* The station identifies with its callsign in the banner of every session.
* Check your national rules for **unattended/automatic stations**,
  especially on HF, and for third-party traffic. As sysop you are
  responsible for content transmitted by your station; the admin UI lets you
  delete messages.
