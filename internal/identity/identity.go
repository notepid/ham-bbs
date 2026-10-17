// Package identity implements HamBBS login keys.
//
// A user's Ed25519 key pair is derived deterministically from callsign and
// password with Argon2id, so a user only needs to remember a password and
// the server only stores the public key. Login is a signature over a
// server-provided nonce. Nothing is encrypted: the signature proves who is
// talking without obscuring the meaning of any transmission.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/argon2"

	"hambbs/internal/callsign"
)

// Argon2id parameters. Changing these changes every derived key.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	saltPrefix   = "hambbs-v1:"
)

// DeriveKey derives the user's private key from callsign and password.
func DeriveKey(call, password string) ed25519.PrivateKey {
	salt := []byte(saltPrefix + callsign.Base(call))
	seed := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

// PublicKey derives just the public key (used by the server when a sysop
// sets a password, or when a terminal user logs in with a password).
func PublicKey(call, password string) ed25519.PublicKey {
	return DeriveKey(call, password).Public().(ed25519.PublicKey)
}

// NewNonce returns a fresh random login nonce, hex encoded.
func NewNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func challenge(node, call, nonce string) []byte {
	return []byte("HBBS1|" + callsign.Normalize(node) + "|" + callsign.Base(call) + "|" + nonce)
}

// Sign signs a login challenge.
func Sign(key ed25519.PrivateKey, node, call, nonce string) []byte {
	return ed25519.Sign(key, challenge(node, call, nonce))
}

// Verify checks a login signature.
func Verify(pub ed25519.PublicKey, node, call, nonce string, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pub, challenge(node, call, nonce), sig)
}
