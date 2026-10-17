package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
)

// NewMessageID returns a 12 character message ID. Twelve characters match
// the classic BID/MID limit, which eases a later FBB gateway.
func NewMessageID(from string, created int64) string {
	salt := make([]byte, 8)
	rand.Read(salt)
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|", from, created)
	h.Write(salt)
	return base32.StdEncoding.EncodeToString(h.Sum(nil))[:12]
}
