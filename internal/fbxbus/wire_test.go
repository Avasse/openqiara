package fbxbus

import (
	"bytes"
	"testing"
)

// helloFrame is the hello header seen on the wire (strace of fbxbusctl,
// 2026-05), serial 0.
var helloFrame = []byte{
	0x6c, 0x01, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x73, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x2f, 0x66, 0x62, 0x78, 0x62, 0x75, 0x73, 0x00,
	0x03, 0x73, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x00,
	0x08, 0x73, 0x01, 0x00, 0x00, 0x00, 0x69, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func TestHelloHeaderMatchesTheWire(t *testing.T) {
	m := &Message{Type: TypeCall, Path: "/fbxbus", Member: "hello", Signature: "i", Body: make([]byte, 4)}
	if got := m.header(); !bytes.Equal(got, helloFrame) {
		t.Fatalf("hello header\n got % x\nwant % x", got, helloFrame)
	}
}

// The filter add body worked out in the RE report for new_event.
func TestFilterBody(t *testing.T) {
	want := []byte{
		0x2a, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0,
		0x12, 0, 0, 0, '/', 'h', 'l', '_', 'e', 'v', 'e', 'n', 't', '_', 'c', 'o', 'l', 'l', 'e', 'c', 't', 'd', 0, 0,
		9, 0, 0, 0, 'n', 'e', 'w', '_', 'e', 'v', 'e', 'n', 't', 0,
	}
	if got := filterBody(1, "/hl_event_collectd", "new_event"); !bytes.Equal(got, want) {
		t.Fatalf("filter body\n got % x\nwant % x", got, want)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, m := range []Message{
		{Type: TypeReply, Serial: 7, ReplySerial: 1234},
		{Type: TypeError, Serial: 8, ReplySerial: 99, ErrorCode: ErrNotImplemented, Signature: "s", Body: appendString(nil, "no")},
		{Type: TypeCall, Serial: 9, Path: "/hl_event_collectd", Member: "new_notification", Signature: "s", Body: appendString(nil, `{"type":"iv_event"}`)},
	} {
		h := m.header()
		if len(h)%8 != 0 {
			t.Fatalf("header of %d bytes, not a multiple of 8", len(h))
		}
		got, hdrLen, bodyLen, err := parseHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if hdrLen != len(h) || bodyLen != len(m.Body) {
			t.Fatalf("lengths %d/%d, want %d/%d", hdrLen, bodyLen, len(h), len(m.Body))
		}
		got.Body = m.Body
		if got.Type != m.Type || got.Serial != m.Serial || got.ReplySerial != m.ReplySerial ||
			got.Path != m.Path || got.Member != m.Member || got.ErrorCode != m.ErrorCode || got.Signature != m.Signature {
			t.Fatalf("round trip\n got %+v\nwant %+v", got, m)
		}
	}
	if s, err := bodyString(appendString(nil, `{"a":1}`)); err != nil || s != `{"a":1}` {
		t.Fatalf("bodyString = %q, %v", s, err)
	}
}
