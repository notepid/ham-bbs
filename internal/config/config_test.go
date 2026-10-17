package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestServerRoundTrip: saving the server config (as -set-admin-password
// does) keeps every setting, including neighbours.
func TestServerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hambbs.toml")
	os.WriteFile(path, []byte(`
callsign = "la1abc"
[ax25]
enabled = true
port = 1
[[neighbor]]
node = "la9bbs"
password = "secret"
connect = "ax25:LA9ABC-1 via LA1DIG"
interval = 30
routes = ["*"]
[[neighbor]]
node = "LA8BBS"
call = "LA8XYZ"
password = "other"
scopes = ["LA", "EU"]
`), 0o600)
	c, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := c.Neighbors[0]; n.Node != "LA9BBS" || n.Call != "LA9BBS" || !reflect.DeepEqual(n.Scopes, []string{"*"}) {
		t.Fatalf("defaults not applied: %+v", n)
	}
	if err := SaveServer(path, c); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	c.path, c2.path = "", ""
	if !reflect.DeepEqual(c, c2) {
		t.Fatalf("round trip changed the config:\n%+v\n%+v", c, c2)
	}
}

func TestNeighborValidation(t *testing.T) {
	for _, tc := range []struct{ toml, err string }{
		{`[[neighbor]]` + "\n" + `password = "x"`, "node is not set"},
		{`[[neighbor]]` + "\n" + `node = "LA9BBS"`, "password is not set"},
		{`[[neighbor]]` + "\n" + `node = "LA1ABC"` + "\n" + `password = "x"`, "own name"},
		{`[[neighbor]]` + "\n" + `node = "LA9BBS"` + "\n" + `password = "x"` + "\n" + `connect = "telnet:x"`, "must start with"},
	} {
		path := filepath.Join(t.TempDir(), "hambbs.toml")
		os.WriteFile(path, []byte("callsign = \"LA1ABC\"\n"+tc.toml+"\n"), 0o600)
		if _, err := LoadServer(path); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%q: err = %v, want %q", tc.toml, err, tc.err)
		}
	}
}
