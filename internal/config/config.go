// Package config loads and saves the server and client TOML files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
)

// Server is hambbs.toml.
type Server struct {
	Callsign         string   `toml:"callsign"`
	Node             string   `toml:"node"`
	Name             string   `toml:"name"`
	MOTD             string   `toml:"motd"`
	DataDir          string   `toml:"data_dir"`
	OpenRegistration bool     `toml:"open_registration"`
	BulletinDays     int      `toml:"bulletin_days"`
	MaxMessageBytes  int      `toml:"max_message_bytes"`
	SyncLimit        int      `toml:"sync_limit"`
	TCP              TCP      `toml:"tcp"`
	VARA             VARA     `toml:"vara"`
	AX25             AX25     `toml:"ax25"`
	Admin            Admin    `toml:"admin"`
	Areas            []string `toml:"areas"`

	path string
}

type TCP struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

// VARA connects the BBS to a VARA-compatible modem (VARA FM/HF, VardeFM).
type VARA struct {
	Enabled bool     `toml:"enabled"`
	Addr    string   `toml:"addr"`
	MyCalls []string `toml:"mycalls"`
}

// AX25 connects the BBS to an AX.25 TNC with an AGWPE interface (Direwolf,
// UZ7HO soundmodem).
type AX25 struct {
	Enabled bool     `toml:"enabled"`
	Addr    string   `toml:"addr"`
	Port    int      `toml:"port"`
	MyCalls []string `toml:"mycalls"`
}

type Admin struct {
	Listen       string `toml:"listen"`
	PasswordHash string `toml:"password_hash"`
}

// DefaultServer returns the defaults every missing setting falls back to.
func DefaultServer() Server {
	return Server{
		DataDir:          "data",
		OpenRegistration: true,
		BulletinDays:     30,
		MaxMessageBytes:  32 * 1024,
		SyncLimit:        200,
		TCP:              TCP{Enabled: true, Listen: ":6300"},
		VARA:             VARA{Addr: "127.0.0.1:8300"},
		AX25:             AX25{Addr: "127.0.0.1:8000"},
		Admin:            Admin{Listen: "127.0.0.1:8080"},
		Areas:            []string{"ALL", "LOCAL", "TECH", "SALE"},
	}
}

// ErrNotExist is returned by the loaders when the file does not exist.
var ErrNotExist = fs.ErrNotExist

// LoadServer reads path on top of the defaults.
func LoadServer(path string) (Server, error) {
	c := DefaultServer()
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return c, err
	}
	c.path = path
	if c.Node == "" {
		c.Node = c.Callsign
	}
	c.Callsign = strings.ToUpper(c.Callsign)
	c.Node = strings.ToUpper(c.Node)
	if c.Callsign == "" {
		return c, errors.New("callsign is not set")
	}
	if len(c.VARA.MyCalls) == 0 {
		c.VARA.MyCalls = []string{c.Callsign}
	}
	if len(c.AX25.MyCalls) == 0 {
		c.AX25.MyCalls = []string{c.Callsign}
	}
	return c, nil
}

// DataPath resolves a file inside data_dir, relative to the config file.
func (c Server) DataPath(name string) string {
	return resolve(c.path, c.DataDir, name)
}

func resolve(cfgPath, dir, name string) string {
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(cfgPath), dir)
	}
	return filepath.Join(dir, name)
}

var serverTmpl = template.Must(template.New("cfg").Parse(`# HamBBS server configuration.
# Everything not set here uses a sensible default; see docs/setup.md.

# Your callsign. Used for station identification on every connection.
callsign = {{printf "%q" .Callsign}}
# Node name used in addresses (CALL@NODE). Defaults to the callsign.
node = {{printf "%q" .Node}}
# Friendly name shown in the welcome banner.
name = {{printf "%q" .Name}}
# Message of the day, shown after login.
motd = {{printf "%q" .MOTD}}

# Where the database lives (relative to this file).
data_dir = {{printf "%q" .DataDir}}
# Let unknown callsigns create an account on first login.
open_registration = {{.OpenRegistration}}
# Bulletins expire after this many days (0 = never).
bulletin_days = {{.BulletinDays}}
# Largest accepted message body in bytes.
max_message_bytes = {{.MaxMessageBytes}}
# Max messages per sync response.
sync_limit = {{.SyncLimit}}
# Suggested bulletin areas (users may post to others too).
areas = [{{range $i, $a := .Areas}}{{if $i}}, {{end}}{{printf "%q" $a}}{{end}}]

[tcp]
# Accept clients and telnet over TCP/IP (internet, HamNet, LAN).
enabled = {{.TCP.Enabled}}
listen = {{printf "%q" .TCP.Listen}}

[vara]
# Answer calls through a VARA-compatible modem (VARA FM/HF, VardeFM).
# The modem must be running; its data port is addr's port + 1.
enabled = {{.VARA.Enabled}}
addr = {{printf "%q" .VARA.Addr}}
# Callsigns to answer on (default: callsign above), e.g. ["LA1ABC-1"].
mycalls = [{{range $i, $a := .VARA.MyCalls}}{{if $i}}, {{end}}{{printf "%q" $a}}{{end}}]

[ax25]
# Answer AX.25 packet calls (VHF/UHF 1200/9600 baud) through a TNC with an
# AGWPE interface: Direwolf (default port 8000) or UZ7HO soundmodem.
enabled = {{.AX25.Enabled}}
addr = {{printf "%q" .AX25.Addr}}
# TNC radio port: 0 = the first (Direwolf channel 0).
port = {{.AX25.Port}}
# Callsigns to answer on (default: callsign above), e.g. ["LA1ABC-1"].
mycalls = [{{range $i, $a := .AX25.MyCalls}}{{if $i}}, {{end}}{{printf "%q" $a}}{{end}}]

[admin]
# Web admin UI. Keep it on 127.0.0.1 unless you set a password.
listen = {{printf "%q" .Admin.Listen}}
# bcrypt hash; set with: hambbs-server -set-admin-password
password_hash = {{printf "%q" .Admin.PasswordHash}}
`))

// SaveServer writes c to path, with comments.
func SaveServer(path string, c Server) error {
	var buf bytes.Buffer
	if err := serverTmpl.Execute(&buf, c); err != nil {
		return err
	}
	return writeFile(path, buf.Bytes())
}

// Client is hambbs-client.toml.
type Client struct {
	Callsign      string   `toml:"callsign"`
	Key           string   `toml:"key"`       // base64 Ed25519 seed derived from the password
	Transport     string   `toml:"transport"` // "tcp", "vara" or "ax25"
	Address       string   `toml:"address"`   // tcp: host:port; vara: the BBS callsign; ax25: "CALL [via DIGI,...]"
	VaraAddr      string   `toml:"vara_addr"` // vara: modem command port host:port
	AGWAddr       string   `toml:"agw_addr"`  // ax25: TNC AGWPE port host:port
	AGWPort       int      `toml:"agw_port"`  // ax25: TNC radio port, 0 = first
	Listen        string   `toml:"listen"`
	DataDir       string   `toml:"data_dir"`
	FetchPersonal bool     `toml:"fetch_personal"`
	FetchAreas    []string `toml:"fetch_areas"`
	Areas         []string `toml:"areas"`
	InitialDays   int      `toml:"initial_days"`

	path string
}

func DefaultClient() Client {
	return Client{
		Transport:     "tcp",
		Address:       "localhost:6300",
		VaraAddr:      "127.0.0.1:8300",
		AGWAddr:       "127.0.0.1:8000",
		Listen:        "127.0.0.1:8081",
		DataDir:       "client-data",
		FetchPersonal: true,
		InitialDays:   14,
	}
}

// LoadClient reads path on top of the defaults. A missing file yields the
// defaults and ErrNotExist.
func LoadClient(path string) (Client, error) {
	c := DefaultClient()
	c.path = path
	if _, err := os.Stat(path); err != nil {
		return c, err
	}
	_, err := toml.DecodeFile(path, &c)
	c.path = path
	c.Callsign = strings.ToUpper(c.Callsign)
	return c, err
}

func (c Client) DataPath(name string) string { return resolve(c.path, c.DataDir, name) }
func (c Client) Path() string                { return c.path }
func (c *Client) SetPath(p string)           { c.path = p }

// Configured reports whether the client has enough settings to sync.
func (c Client) Configured() bool {
	return c.Callsign != "" && c.Key != "" && c.Address != ""
}

// SaveClient writes c to its path.
func SaveClient(c Client) error {
	var buf bytes.Buffer
	buf.WriteString("# HamBBS client configuration. Easiest edited from the client's Settings page.\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	return writeFile(c.path, buf.Bytes())
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}
