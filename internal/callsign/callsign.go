// Package callsign normalizes and validates amateur radio callsigns and
// HamBBS addresses of the form NAME[@AT].
package callsign

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	callRe = regexp.MustCompile(`^[A-Z0-9]{3,7}(-([0-9]|1[0-5]))?$`)
	areaRe = regexp.MustCompile(`^[A-Z0-9_]{1,12}$`)
	atRe   = regexp.MustCompile(`^[A-Z0-9_.#-]{1,40}$`)
)

// Normalize upper-cases and trims a callsign.
func Normalize(call string) string {
	return strings.ToUpper(strings.TrimSpace(call))
}

// Valid reports whether call looks like a callsign (optionally with SSID).
// A callsign must contain at least one digit and one letter.
func Valid(call string) bool {
	call = Normalize(call)
	if !callRe.MatchString(call) {
		return false
	}
	base := Base(call)
	return strings.ContainsAny(base, "0123456789") && strings.ContainsAny(base, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// Base strips the SSID: "LA1ABC-7" -> "LA1ABC".
func Base(call string) string {
	call = Normalize(call)
	if i := strings.IndexByte(call, '-'); i >= 0 {
		return call[:i]
	}
	return call
}

// ValidArea reports whether name is a valid bulletin area name.
func ValidArea(name string) bool {
	return areaRe.MatchString(Normalize(name))
}

// Address is a parsed NAME[@AT] address. For personal mail NAME is a
// callsign and AT the home node; for bulletins NAME is the area and AT the
// distribution scope.
type Address struct {
	Name string
	At   string
}

func (a Address) String() string {
	if a.At == "" {
		return a.Name
	}
	return a.Name + "@" + a.At
}

// ParseAddress splits and normalizes "NAME@AT".
func ParseAddress(s string) Address {
	s = Normalize(s)
	name, at, _ := strings.Cut(s, "@")
	return Address{Name: name, At: at}
}

// ParsePersonal parses and validates a personal-mail address.
func ParsePersonal(s string) (Address, error) {
	a := ParseAddress(s)
	if !Valid(a.Name) {
		return a, fmt.Errorf("invalid callsign %q", a.Name)
	}
	a.Name = Base(a.Name)
	if a.At != "" && !atRe.MatchString(a.At) {
		return a, fmt.Errorf("invalid node %q", a.At)
	}
	return a, nil
}

// ParseBulletin parses and validates a bulletin address.
func ParseBulletin(s string) (Address, error) {
	a := ParseAddress(s)
	if !ValidArea(a.Name) {
		return a, fmt.Errorf("invalid area %q (1-12 of A-Z, 0-9, _)", a.Name)
	}
	if a.At != "" && !atRe.MatchString(a.At) {
		return a, fmt.Errorf("invalid scope %q", a.At)
	}
	return a, nil
}
