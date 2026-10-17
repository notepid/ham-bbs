package agwpe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// AGWPE frames: a 36-byte header followed by DataLen bytes.
//
//	0     port (0 = first radio port)
//	1-3   reserved
//	4     data kind (a letter, e.g. 'C', 'D', 'd')
//	5     reserved
//	6     PID
//	7     reserved
//	8-17  call from, NUL padded
//	18-27 call to, NUL padded
//	28-31 data length, little endian
//	32-35 user (reserved)
const (
	headerLen = 36
	callLen   = 10
	// maxData bounds what we accept from the TNC. AX.25 frames are at most
	// a few kB; port info and monitoring text are smaller still.
	maxData = 64 << 10
)

// Frame is one AGWPE message.
type Frame struct {
	Port byte
	Kind byte
	PID  byte
	From string
	To   string
	Data []byte
}

func (f Frame) String() string {
	return fmt.Sprintf("%c port=%d %s>%s len=%d", f.Kind, f.Port, f.From, f.To, len(f.Data))
}

// WriteFrame encodes f to w in one Write call.
func WriteFrame(w io.Writer, f Frame) error {
	buf := make([]byte, headerLen+len(f.Data))
	buf[0] = f.Port
	buf[4] = f.Kind
	buf[6] = f.PID
	copy(buf[8:8+callLen], f.From)
	copy(buf[18:18+callLen], f.To)
	binary.LittleEndian.PutUint32(buf[28:32], uint32(len(f.Data)))
	copy(buf[headerLen:], f.Data)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame from r.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.LittleEndian.Uint32(h[28:32])
	if n > maxData {
		return Frame{}, fmt.Errorf("agwpe: frame of %d bytes is too large", n)
	}
	f := Frame{Port: h[0], Kind: h[4], PID: h[6], From: cstring(h[8 : 8+callLen]), To: cstring(h[18 : 18+callLen])}
	if n > 0 {
		f.Data = make([]byte, n)
		if _, err := io.ReadFull(r, f.Data); err != nil {
			return Frame{}, err
		}
	}
	return f, nil
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToUpper(strings.TrimSpace(string(b)))
}

// viaData encodes digipeaters for a 'v' (connect via) frame: a count byte
// followed by 10 bytes per callsign.
func viaData(digis []string) []byte {
	b := make([]byte, 1+callLen*len(digis))
	b[0] = byte(len(digis))
	for i, d := range digis {
		copy(b[1+i*callLen:], d)
	}
	return b
}

// DecodeVia decodes the digipeater list of a 'v' frame.
func DecodeVia(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	n := int(b[0])
	var out []string
	for i := 0; i < n && 1+(i+1)*callLen <= len(b); i++ {
		out = append(out, cstring(b[1+i*callLen:1+(i+1)*callLen]))
	}
	return out
}
