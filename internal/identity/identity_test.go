package identity

import (
	"bytes"
	"testing"
)

func TestDeriveAndVerify(t *testing.T) {
	k1 := DeriveKey("la1abc-7", "secret")
	k2 := DeriveKey("LA1ABC", "secret")
	if !bytes.Equal(k1, k2) {
		t.Fatal("key must depend on base callsign only, case-insensitive")
	}
	if bytes.Equal(DeriveKey("LA1ABC", "other"), k1) || bytes.Equal(DeriveKey("LA2ABC", "secret"), k1) {
		t.Fatal("key must depend on password and callsign")
	}
	pub := PublicKey("LA1ABC", "secret")
	sig := Sign(k1, "NODE", "LA1ABC", "abcd")
	if !Verify(pub, "NODE", "LA1ABC", "abcd", sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(pub, "NODE", "LA1ABC", "abce", sig) || Verify(pub, "OTHER", "LA1ABC", "abcd", sig) {
		t.Fatal("signature must bind nonce and node")
	}
	if Verify(nil, "NODE", "LA1ABC", "abcd", sig) {
		t.Fatal("nil key accepted")
	}
}
