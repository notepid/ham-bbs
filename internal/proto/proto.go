// Package proto implements the HamBBS sync protocol (see docs/protocol.md).
//
// After the server banner and the client's magic line, both sides exchange
// frames:
//
//	uvarint length | type byte | payload
//
// length counts the type byte plus payload. The payload is CBOR (RFC 8949).
// If bit 0x80 of the type byte is set, the payload is raw DEFLATE
// (RFC 1951) compressed CBOR.
package proto

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

const (
	// Version is the protocol version carried in the banner and HELLO.
	Version = 1
	// Magic is the line a sync client sends to switch to frame mode.
	Magic = "#HBBS1"
	// ReadyLine is sent by the server right before its first frame.
	ReadyLine = "#HBBS1 OK"
	// MaxFrame bounds a single frame (after decompression).
	MaxFrame = 1 << 20

	flagDeflate     = 0x80
	compressMinSize = 64
)

// Type identifies a frame.
type Type uint8

const (
	THello     Type = 1  // C->S: identify and authenticate
	TWelcome   Type = 2  // S->C: login accepted
	TSync      Type = 3  // C->S: request headers/bodies since cursor
	THeaders   Type = 4  // S->C: message headers (no bodies)
	TGet       Type = 5  // C->S: request bodies by ID
	TPut       Type = 6  // C->S: upload messages
	TMessages  Type = 7  // S->C: full messages
	TPutResult Type = 8  // S->C: result of PUT
	TEnd       Type = 9  // either: end of batch, other side's turn
	TBye       Type = 10 // either: end of batch and session
	TError     Type = 11 // S->C: fatal error, session ends
)

func (t Type) String() string {
	switch t {
	case THello:
		return "HELLO"
	case TWelcome:
		return "WELCOME"
	case TSync:
		return "SYNC"
	case THeaders:
		return "HEADERS"
	case TGet:
		return "GET"
	case TPut:
		return "PUT"
	case TMessages:
		return "MESSAGES"
	case TPutResult:
		return "PUTRESULT"
	case TEnd:
		return "END"
	case TBye:
		return "BYE"
	case TError:
		return "ERROR"
	}
	return fmt.Sprintf("TYPE(%d)", uint8(t))
}

// Hello authenticates the client. Sig is an Ed25519 signature over
// "HBBS1|<node>|<call>|<nonce>" (see package identity). PubKey is only
// needed the first time, when the server allows open registration.
type Hello struct {
	Call    string `cbor:"1,keyasint"`
	Version int    `cbor:"2,keyasint"`
	Sig     []byte `cbor:"3,keyasint"`
	PubKey  []byte `cbor:"4,keyasint,omitempty"`
	Client  string `cbor:"5,keyasint,omitempty"`
}

// Welcome confirms login.
type Welcome struct {
	Call       string `cbor:"1,keyasint"`
	Node       string `cbor:"2,keyasint"`
	Registered bool   `cbor:"3,keyasint,omitempty"` // account was created now
	MOTD       string `cbor:"4,keyasint,omitempty"`
}

// Sync requests everything visible to the user with Seq > Since.
// Bodies are included directly for personal mail if FetchPersonal is set,
// and for bulletins in FetchAreas ("*" = all areas). Everything else is
// sent as headers only.
type Sync struct {
	Since         int64    `cbor:"1,keyasint"`
	Areas         []string `cbor:"2,keyasint,omitempty"` // bulletin areas to include; empty = all
	FetchPersonal bool     `cbor:"3,keyasint,omitempty"`
	FetchAreas    []string `cbor:"4,keyasint,omitempty"`
	After         int64    `cbor:"5,keyasint,omitempty"` // only messages created after (unix s)
	Limit         int      `cbor:"6,keyasint,omitempty"` // max messages in response; 0 = server default
}

// Message is a personal message or bulletin. In a HEADERS frame Body is
// empty and Size gives the body length.
type Message struct {
	ID      string   `cbor:"1,keyasint"`
	Seq     int64    `cbor:"2,keyasint,omitempty"` // server sequence (set by server)
	Kind    string   `cbor:"3,keyasint"`           // "P" personal, "B" bulletin
	From    string   `cbor:"4,keyasint"`
	To      string   `cbor:"5,keyasint"` // CALL[@NODE] or AREA[@SCOPE]
	Subject string   `cbor:"6,keyasint"`
	Body    string   `cbor:"7,keyasint,omitempty"`
	Size    int      `cbor:"8,keyasint,omitempty"`
	Created int64    `cbor:"9,keyasint"`            // unix seconds
	Expires int64    `cbor:"10,keyasint,omitempty"` // unix seconds, 0 = never
	Origin  string   `cbor:"11,keyasint,omitempty"` // node where message entered the network
	Path    []string `cbor:"12,keyasint,omitempty"` // nodes traversed
}

// Headers answers a Sync. Cursor is the value to send as Since next time.
// More means the limit was hit; sync again to continue.
type Headers struct {
	Cursor  int64     `cbor:"1,keyasint"`
	More    bool      `cbor:"2,keyasint,omitempty"`
	Headers []Message `cbor:"3,keyasint,omitempty"`
}

// Get requests full messages by ID.
type Get struct {
	IDs []string `cbor:"1,keyasint"`
}

// Put uploads messages composed by the client.
type Put struct {
	Messages []Message `cbor:"1,keyasint"`
}

// Messages carries full messages.
type Messages struct {
	Messages []Message `cbor:"1,keyasint"`
}

// Rejection explains why a PUT message was refused.
type Rejection struct {
	ID     string `cbor:"1,keyasint"`
	Reason string `cbor:"2,keyasint"`
}

// PutResult reports which uploaded messages were stored. Duplicates of
// already-stored IDs count as accepted, so a client can safely resend.
type PutResult struct {
	Accepted []string    `cbor:"1,keyasint,omitempty"`
	Rejected []Rejection `cbor:"2,keyasint,omitempty"`
}

// Error is a fatal error.
type Error struct {
	Code int    `cbor:"1,keyasint"`
	Text string `cbor:"2,keyasint"`
}

func (e *Error) Error() string { return fmt.Sprintf("server error %d: %s", e.Code, e.Text) }

// Error codes.
const (
	ErrBadRequest   = 400
	ErrAuth         = 401
	ErrForbidden    = 403
	ErrVersion      = 426
	ErrServer       = 500
	ErrUnregistered = 404
)

// Frame is a decoded frame.
type Frame struct {
	Type    Type
	Payload []byte // CBOR, already decompressed
}

// Decode unmarshals the frame payload into v.
func (f Frame) Decode(v any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	return cbor.Unmarshal(f.Payload, v)
}

// WriteFrame encodes v (may be nil) and writes one frame.
func WriteFrame(w io.Writer, t Type, v any) error {
	var payload []byte
	if v != nil {
		var err error
		payload, err = cbor.Marshal(v)
		if err != nil {
			return err
		}
	}
	tb := byte(t)
	if len(payload) >= compressMinSize {
		var buf bytes.Buffer
		zw, _ := flate.NewWriter(&buf, flate.BestCompression)
		zw.Write(payload)
		zw.Close()
		if buf.Len() < len(payload) {
			payload = buf.Bytes()
			tb |= flagDeflate
		}
	}
	hdr := binary.AppendUvarint(nil, uint64(len(payload)+1))
	hdr = append(hdr, tb)
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return Frame{}, err
	}
	if n == 0 || n > MaxFrame {
		return Frame{}, fmt.Errorf("bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Frame{}, err
	}
	f := Frame{Type: Type(buf[0] &^ flagDeflate), Payload: buf[1:]}
	if buf[0]&flagDeflate != 0 {
		zr := flate.NewReader(bytes.NewReader(f.Payload))
		defer zr.Close()
		p, err := io.ReadAll(io.LimitReader(zr, MaxFrame+1))
		if err != nil {
			return Frame{}, fmt.Errorf("decompress: %w", err)
		}
		if len(p) > MaxFrame {
			return Frame{}, errors.New("frame too large after decompression")
		}
		f.Payload = p
	}
	return f, nil
}

// Banner is the first line a server sends: "[HBBS1:<node>:<nonce>] <text>".
type Banner struct {
	Node  string
	Nonce string
}

var bannerRe = regexp.MustCompile(`\[HBBS(\d+):([A-Za-z0-9_.#-]+):([0-9a-f]+)\]`)

// FormatBanner renders the banner line (without line ending).
func FormatBanner(node, nonce, text string) string {
	return fmt.Sprintf("[HBBS%d:%s:%s] %s", Version, node, nonce, text)
}

// ParseBanner extracts node and nonce from a banner line.
func ParseBanner(line string) (Banner, bool) {
	m := bannerRe.FindStringSubmatch(line)
	if m == nil || m[1] != fmt.Sprint(Version) {
		return Banner{}, false
	}
	return Banner{Node: m[2], Nonce: m[3]}, true
}

// LineReader reads text lines terminated by CR, LF or CRLF. Packet radio
// uses bare CR, telnet uses CRLF. It never reads ahead past a terminator
// (which would block on a live link); instead an LF directly following a
// CR is skipped at the start of the next line.
type LineReader struct {
	R      *bufio.Reader
	Max    int // max line length kept; extra bytes are dropped
	lastCR bool
}

// ReadLine returns the next line without its terminator.
func (lr *LineReader) ReadLine() (string, error) {
	var sb strings.Builder
	for {
		b, err := lr.R.ReadByte()
		if err != nil {
			if sb.Len() > 0 && err == io.EOF {
				return sb.String(), nil
			}
			return sb.String(), err
		}
		wasCR := lr.lastCR
		lr.lastCR = b == '\r'
		switch b {
		case '\n':
			if wasCR && sb.Len() == 0 {
				continue
			}
			return sb.String(), nil
		case '\r':
			return sb.String(), nil
		}
		if lr.Max <= 0 || sb.Len() < lr.Max {
			sb.WriteByte(b)
		}
	}
}
