package camera

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
)

// hlcamd sends its encoded streams to hls over UDP multicast on the
// loopback (RE 2026-10-10, re_reports/20261010/hlcamd.md, checked on
// captures): 224.0.0.1, one port per stream, nothing leaves the camera.
// Anyone may join next to hls. Ports: 9600 the main H.264 1920×1080 stream
// (~1 Mbit/s), 9601 a second, lighter 1920×1080 one (what hls segments),
// 9700 the audio as raw PCM (s16le, 16 kHz, mono), 9610 a 1280×720 JPEG
// a second.
const (
	MulticastGroup     = "224.0.0.1"
	MulticastVideoMain = 9600
)

// multicastHeader is the 24-byte header before each fragment (packed,
// little-endian): ts u64 (µs), flags u8 (bit 2 keyframe), n/m u8 (fragment
// number, 1-based, low nibble; fragment count, high nibble), seqno u32,
// offset u32, total_size u32, data_size u16.
const multicastHeader = 24

type multicastFragment struct {
	ts     uint64
	seqno  uint32
	offset uint32
	total  uint32
	data   []byte
}

func parseMulticastFragment(p []byte) (multicastFragment, error) {
	if len(p) < multicastHeader {
		return multicastFragment{}, errors.New("short packet")
	}
	f := multicastFragment{
		ts:     binary.LittleEndian.Uint64(p[0:]),
		seqno:  binary.LittleEndian.Uint32(p[10:]),
		offset: binary.LittleEndian.Uint32(p[14:]),
		total:  binary.LittleEndian.Uint32(p[18:]),
	}
	size := int(binary.LittleEndian.Uint16(p[22:]))
	if multicastHeader+size > len(p) || uint64(f.offset)+uint64(size) > uint64(f.total) {
		return multicastFragment{}, fmt.Errorf("fragment out of bounds (seq %d)", f.seqno)
	}
	f.data = p[multicastHeader : multicastHeader+size]
	return f, nil
}

// frameAssembler puts a frame's fragments back together. A frame starts
// over when its seqno changes; a frame with a fragment missing is dropped
// (UDP on the loopback loses nothing but an overrun socket buffer).
type frameAssembler struct {
	seqno   uint32
	started bool
	buf     []byte
	have    uint32
	ts      uint64
}

// add takes a fragment and returns the whole frame once it is complete.
func (a *frameAssembler) add(f multicastFragment) ([]byte, bool) {
	if !a.started || f.seqno != a.seqno || uint32(len(a.buf)) != f.total {
		a.started, a.seqno, a.have = true, f.seqno, 0
		a.buf = make([]byte, f.total)
		a.ts = f.ts
	}
	copy(a.buf[f.offset:], f.data)
	a.have += uint32(len(f.data))
	if a.have < f.total {
		return nil, false
	}
	frame := a.buf
	a.started, a.buf = false, nil
	return frame, true
}

// MulticastVideo reads hlcamd's H.264 stream on port until ctx ends. Like
// MPEGTSParser, it sends one Sample per NAL unit, without start code (each
// frame is Annex-B, SPS and PPS before each IDR); the PTS is hlcamd's, in
// the 90 kHz clock.
func MulticastVideo(ctx context.Context, port int, logger *slog.Logger) (<-chan Sample, error) {
	ifi, err := net.InterfaceByName("lo")
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenMulticastUDP("udp4", ifi, &net.UDPAddr{IP: net.ParseIP(MulticastGroup), Port: port})
	if err != nil {
		return nil, fmt.Errorf("multicast %d: %w", port, err)
	}
	_ = conn.SetReadBuffer(1 << 20) // frames come in bursts of up to 24 KiB fragments
	return readMulticastVideo(ctx, conn, logger), nil
}

func readMulticastVideo(ctx context.Context, conn net.PacketConn, logger *slog.Logger) <-chan Sample {
	out := make(chan Sample, 32)
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		defer close(out)
		var asm frameAssembler
		buf := make([]byte, 64<<10)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("multicast video: read failed", "error", err)
				}
				return
			}
			f, err := parseMulticastFragment(buf[:n])
			if err != nil {
				logger.Debug("multicast video: bad fragment", "error", err)
				continue
			}
			frame, ok := asm.add(f)
			if !ok {
				continue
			}
			pts := int64(asm.ts * 90 / 1000)
			for _, nal := range splitNALUnits(frame) {
				if len(nal) == 0 {
					continue
				}
				select {
				case out <- Sample{IsVideo: true, PTS: pts, Data: nal}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
