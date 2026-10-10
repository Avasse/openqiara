// Package publisher — AAC over RTP packetizer.
//
// Implements RFC 3640 "mpeg4-generic" mode for streaming AAC frames
// over RTP, with one access unit per packet (the simplest profile): how
// HomeKit carries AAC-ELD camera audio.

package publisher

import (
	"github.com/pion/rtp"
)

// packetizeAAC builds one RTP packet containing a single AAC access
// unit per RFC 3640 (mpeg4-generic mode). The AU-headers-length field
// is fixed at 16 bits (one header), and the AU header itself encodes
// the AU size in 13 bits and an index of 0 in the low 3 bits.
//
// Returned packet has Marker=true (every audio frame is a complete
// access unit, RFC 3550 audio convention).
func packetizeAAC(aacRaw []byte, ssrc uint32, seq uint16, ts uint32, payloadType uint8) *rtp.Packet {
	if len(aacRaw) == 0 {
		return nil
	}
	// AU-headers-length: 16 bits, value = 16 (one header of 16 bits).
	// AU header: 13-bit AU size + 3-bit AU index (= 0).
	auSize := uint16(len(aacRaw))
	header := make([]byte, 4+len(aacRaw))
	header[0] = 0x00
	header[1] = 0x10 // AU-headers-length = 16 bits
	header[2] = byte(auSize >> 5)
	header[3] = byte(auSize<<3) & 0xF8 // index = 0 in low 3 bits
	copy(header[4:], aacRaw)

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         true,
		},
		Payload: header,
	}
}
