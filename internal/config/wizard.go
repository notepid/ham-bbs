package config

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"hambbs/internal/callsign"
)

// Wizard asks the few questions needed for a working server config.
func Wizard(in io.Reader, out io.Writer) (Server, error) {
	r := bufio.NewReader(in)
	c := DefaultServer()
	ask := func(prompt, def string) (string, error) {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", prompt, def)
		} else {
			fmt.Fprintf(out, "%s: ", prompt)
		}
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		return line, nil
	}

	fmt.Fprintln(out, "HamBBS first-time setup. Press Enter to accept [defaults].")
	fmt.Fprintln(out)
	for {
		v, err := ask("Your callsign", "")
		if err != nil {
			return c, err
		}
		if callsign.Valid(v) {
			c.Callsign = callsign.Normalize(v)
			break
		}
		fmt.Fprintln(out, "  That doesn't look like a callsign, try again.")
	}
	var err error
	if c.Node, err = ask("Node name (used in addresses CALL@NODE)", callsign.Base(c.Callsign)); err != nil {
		return c, err
	}
	c.Node = strings.ToUpper(c.Node)
	if c.Name, err = ask("BBS name shown to users", c.Node+" BBS"); err != nil {
		return c, err
	}
	if c.TCP.Listen, err = ask("TCP listen address for clients/telnet", c.TCP.Listen); err != nil {
		return c, err
	}
	c.VARA.MyCalls = []string{c.Callsign}
	v, err := ask("Answer calls via a VARA-compatible modem (VARA FM/HF, VardeFM)? y/n", "n")
	if err != nil {
		return c, err
	}
	if strings.HasPrefix(strings.ToLower(v), "y") {
		c.VARA.Enabled = true
		if c.VARA.Addr, err = ask("Modem command port (data port is the next one)", c.VARA.Addr); err != nil {
			return c, err
		}
	}
	if c.Admin.Listen, err = ask("Admin web UI address", c.Admin.Listen); err != nil {
		return c, err
	}
	pw, err := ask("Admin web password (empty = none, only safe on 127.0.0.1)", "")
	if err != nil {
		return c, err
	}
	if pw != "" {
		if c.Admin.PasswordHash, err = HashPassword(pw); err != nil {
			return c, err
		}
	}
	return c, nil
}

// HashPassword bcrypt-hashes an admin password.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword compares a password with a bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
