// Package fbxbus speaks the camera's IPC bus, fbxbusd, enough to serve a
// name on it: openqiarad answers the calls hlcamd makes to
// hl_event_collectd, the vendor's daemon that forwarded them to the dead
// cloud (RE 2026-10-10, re_reports/20261010/fbxbus_server.md).
//
// The bus is D-Bus-like over SOCK_SEQPACKET on the abstract socket
// @fbxbus_daemon. A message is one header packet, then its body in
// packets of at most 0x3f40 bytes.
package fbxbus

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Message types.
const (
	TypeCall   = 1
	TypeReply  = 2
	TypeError  = 3 // carries an error code (field 4)
	TypeSignal = 4
)

// Header field codes.
const (
	fieldPath        = 1
	fieldMember      = 3
	fieldErrorCode   = 4
	fieldReplySerial = 5
	fieldSender      = 7
	fieldSignature   = 8
)

// Error codes.
const (
	ErrNotImplemented = 0x70000004
)

const (
	magic        = 'l'
	maxBodyChunk = 0x3f40
)

// Message is one bus message.
type Message struct {
	Type        byte
	Serial      uint32
	ReplySerial uint32 // replies and errors
	Path        string
	Member      string
	ErrorCode   uint32 // errors
	Signature   string
	Sender      string // set by the daemon
	Body        []byte
}

// align pads b with zeros to a multiple of n.
func align(b []byte, n int) []byte {
	for len(b)%n != 0 {
		b = append(b, 0)
	}
	return b
}

// appendString appends a string value: aligned u32 length, bytes, NUL,
// nothing after. Alignment is relative to the start of b.
func appendString(b []byte, s string) []byte {
	b = align(b, 4)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	b = append(b, s...)
	return append(b, 0)
}

// header encodes m's header, its body length set to len(m.Body). The
// fields go in the order the vendor's library writes them.
func (m *Message) header() []byte {
	b := []byte{magic, m.Type, 0, 0}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m.Body)))
	b = binary.LittleEndian.AppendUint32(b, m.Serial)
	uint := func(fc byte, v uint32) {
		b = append(b, fc, 'u')
		b = align(b, 4)
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	str := func(fc byte, s string) {
		b = append(b, fc, 's')
		b = appendString(b, s)
	}
	if m.ReplySerial != 0 {
		uint(fieldReplySerial, m.ReplySerial)
	}
	str(fieldPath, m.Path)
	str(fieldMember, m.Member)
	if m.Type == TypeError {
		uint(fieldErrorCode, m.ErrorCode)
	}
	str(fieldSignature, m.Signature)
	b = append(b, 0) // end of fields
	return align(b, 8)
}

// parseHeader decodes a header at the start of p and returns its length
// and the body length it announces.
func parseHeader(p []byte) (m Message, hdrLen, bodyLen int, err error) {
	if len(p) < 12 || p[0] != magic {
		return m, 0, 0, errors.New("fbxbus: bad header")
	}
	m.Type = p[1]
	bodyLen = int(binary.LittleEndian.Uint32(p[4:]))
	m.Serial = binary.LittleEndian.Uint32(p[8:])
	off := 12
	for {
		if off >= len(p) {
			return m, 0, 0, errors.New("fbxbus: header truncated")
		}
		fc := p[off]
		if fc == 0 {
			off++
			break
		}
		if off+2 > len(p) {
			return m, 0, 0, errors.New("fbxbus: header truncated")
		}
		sig := p[off+1]
		off = (off + 2 + 3) &^ 3
		if off+4 > len(p) {
			return m, 0, 0, errors.New("fbxbus: header truncated")
		}
		v := binary.LittleEndian.Uint32(p[off:])
		off += 4
		switch sig {
		case 'u':
			switch fc {
			case fieldReplySerial:
				m.ReplySerial = v
			case fieldErrorCode:
				m.ErrorCode = v
			}
		case 's':
			end := off + int(v)
			if end+1 > len(p) {
				return m, 0, 0, errors.New("fbxbus: header string truncated")
			}
			s := string(p[off:end])
			off = end + 1
			switch fc {
			case fieldPath:
				m.Path = s
			case fieldMember:
				m.Member = s
			case fieldSignature:
				m.Signature = s
			case fieldSender:
				m.Sender = s
			}
		default:
			return m, 0, 0, fmt.Errorf("fbxbus: field %d of unknown type %q", fc, sig)
		}
	}
	return m, (off + 7) &^ 7, bodyLen, nil
}

// bodyString reads the string at the start of a body.
func bodyString(body []byte) (string, error) {
	if len(body) < 4 {
		return "", errors.New("fbxbus: body too short")
	}
	n := int(binary.LittleEndian.Uint32(body))
	if 4+n > len(body) {
		return "", errors.New("fbxbus: body string truncated")
	}
	return string(body[4 : 4+n]), nil
}

// bodyInt32 reads the int32 at the start of a body.
func bodyInt32(body []byte) (int32, error) {
	if len(body) < 4 {
		return 0, errors.New("fbxbus: body too short")
	}
	return int32(binary.LittleEndian.Uint32(body)), nil
}

// filterBody is the body of /fbxbus/filter add, signature a(iss()): one
// rule, kind 1 for a method served, 4 for a signal followed.
func filterBody(kind int32, path, member string) []byte {
	elems := []byte{0, 0, 0, 0, 0, 0, 0, 0} // bytelen and count, written below
	elems = binary.LittleEndian.AppendUint32(elems, uint32(kind))
	elems = appendString(elems, path)
	elems = appendString(elems, member)
	binary.LittleEndian.PutUint32(elems[0:], uint32(len(elems)-8))
	binary.LittleEndian.PutUint32(elems[4:], 1)
	return elems
}
