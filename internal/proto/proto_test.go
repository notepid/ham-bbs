package proto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	small := Hello{Call: "LA1ABC", Version: 1, Sig: []byte{1, 2, 3}}
	big := Messages{Messages: []Message{{ID: "ABCDEFGHIJKL", Kind: "B", From: "LA1ABC", To: "TECH", Subject: "hi",
		Body: strings.Repeat("compressible text ", 200), Created: 1}}}
	if err := WriteFrame(&buf, THello, small); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TMessages, big); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TBye, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 400 {
		t.Errorf("expected compression, got %d bytes", buf.Len())
	}

	r := bufio.NewReader(&buf)
	f, err := ReadFrame(r)
	if err != nil || f.Type != THello {
		t.Fatalf("frame 1: %v %v", f.Type, err)
	}
	var h Hello
	if err := f.Decode(&h); err != nil || h.Call != "LA1ABC" || !bytes.Equal(h.Sig, []byte{1, 2, 3}) {
		t.Fatalf("hello: %+v %v", h, err)
	}
	f, err = ReadFrame(r)
	if err != nil || f.Type != TMessages {
		t.Fatalf("frame 2: %v %v", f.Type, err)
	}
	var ms Messages
	if err := f.Decode(&ms); err != nil || ms.Messages[0].Body != big.Messages[0].Body {
		t.Fatalf("messages mismatch: %v", err)
	}
	f, err = ReadFrame(r)
	if err != nil || f.Type != TBye || len(f.Payload) != 0 {
		t.Fatalf("frame 3: %v %v", f.Type, err)
	}
}

func TestBanner(t *testing.T) {
	line := FormatBanner("LA1ABC", "00ff", "Oslo BBS de LA1ABC")
	b, ok := ParseBanner("junk " + line)
	if !ok || b.Node != "LA1ABC" || b.Nonce != "00ff" {
		t.Fatalf("got %+v %v", b, ok)
	}
	if _, ok := ParseBanner("[HBBS9:X:00] future"); ok {
		t.Fatal("accepted unknown version")
	}
}

func TestLineReader(t *testing.T) {
	in := "cr\rcrlf\r\nlf\n\r\nblank-before\r\rlast"
	lr := &LineReader{R: bufio.NewReader(strings.NewReader(in)), Max: 100}
	want := []string{"cr", "crlf", "lf", "", "blank-before", "", "last"}
	for i, w := range want {
		got, err := lr.ReadLine()
		if err != nil || got != w {
			t.Fatalf("line %d: got %q, %v; want %q", i, got, err, w)
		}
	}
}

func TestMessageID(t *testing.T) {
	a, b := NewMessageID("LA1ABC", 1), NewMessageID("LA1ABC", 1)
	if len(a) != 12 || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
}
