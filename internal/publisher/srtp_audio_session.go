// Package publisher — SRTP audio session for HomeKit camera streaming.
//
// HomeKit cameras require both a video AND an audio RTP stream in the
// session — iOS rejects sessions that only deliver video. This sender
// encodes the microphone's PCM to AAC-ELD (SendPCM) and fills the gaps
// with silence (RunSilence): no PCM with the HLS source, or hlcamd
// stalled. Without libfdk-aac, it sends the Opus silence that kept the
// session up before: no sound, but video.

package publisher

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/caligone/openqiara/internal/aaceld"
)

// The microphone's PCM (camera.MulticastPCM) is encoded as it comes:
// AAC-ELD 16 kHz mono, which the accessory advertises, one 30 ms access
// unit per packet, RFC 3640 (aac_rtp.go). The RTP clock is the sample rate.
const (
	aacSampleRate = 16000
	aacBitrate    = 24000
	// realAudioHold is how long after the last real packet silence stays
	// off: PCM comes in 64 ms bursts.
	realAudioHold = 200 * time.Millisecond
)

// makeAudioRTPHeader builds an RTP header for one Opus audio packet.
// Marker bit is always set on audio packets in HomeKit (each packet is
// a complete frame, no fragmentation needed).
func makeAudioRTPHeader(pt uint8, seq uint16, ts, ssrc uint32) *rtp.Header {
	return &rtp.Header{
		Version:        2,
		PayloadType:    pt,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           ssrc,
		Marker:         true,
	}
}

// audioPayloadType is the dynamic RTP payload type negotiated by HomeKit
// for the audio stream. iOS sends it via SelectedRTPStreamConfiguration;
// we cache it and reuse on Reconfigure (same trick as video PT).
const audioDefaultPayloadType = 110

// opusSilencePacket is a single Opus packet representing 20 ms of
// silence, sent when no AAC-ELD encoder is at hand. The TOC byte 0xF8 = config 31 (CELT fullband, 20 ms) + s=0
// (mono) + c=0 (1 frame). The frame data is empty for pure silence —
// Opus represents silence as the TOC byte alone.
var opusSilencePacket = []byte{0xF8}

// srtpAudioSender encrypts audio packets into SRTP and sends them to the
// iOS controller's audio RTP port.
type srtpAudioSender struct {
	logger *slog.Logger

	conn    net.Conn // UDP socket connected to iOS:audioRtpPort
	session *srtp.SessionSRTP
	writer  *srtp.WriteStreamSRTP

	ssrc        uint32
	payloadType uint8

	mu       sync.Mutex
	seq      uint16
	ts       uint32          // next packet's RTP timestamp (random start per RFC 3550)
	enc      *aaceld.Encoder // nil without libfdk-aac
	pcm      []int16         // PCM waiting for a whole frame
	silence  []int16
	lastReal time.Time // last packet of real audio
	closed   bool
}

// newSRTPAudioSender opens a UDP socket to the iOS controller and
// initialises the SRTP encryption context for the audio stream. Same
// shape as newSRTPVideoSender but on the audio port and with audio
// crypto keys (negotiated separately by SetupEndpoints).
func newSRTPAudioSender(
	controllerIP net.IP,
	controllerPort uint16,
	localKey, localSalt []byte,
	remoteKey, remoteSalt []byte,
	ssrc uint32,
	payloadType uint8,
	logger *slog.Logger,
) (*srtpAudioSender, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(localKey) != 16 || len(localSalt) != 14 {
		return nil, fmt.Errorf("srtp audio: bad local key/salt size (%d/%d)", len(localKey), len(localSalt))
	}
	if len(remoteKey) != 16 || len(remoteSalt) != 14 {
		return nil, fmt.Errorf("srtp audio: bad remote key/salt size (%d/%d)", len(remoteKey), len(remoteSalt))
	}
	if payloadType == 0 {
		payloadType = audioDefaultPayloadType
	}

	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: controllerIP, Port: int(controllerPort)})
	if err != nil {
		return nil, fmt.Errorf("srtp audio: dial udp: %w", err)
	}

	cfg := &srtp.Config{
		Keys: srtp.SessionKeys{
			LocalMasterKey:   localKey,
			LocalMasterSalt:  localSalt,
			RemoteMasterKey:  remoteKey,
			RemoteMasterSalt: remoteSalt,
		},
		Profile: srtp.ProtectionProfileAes128CmHmacSha1_80,
	}

	session, err := srtp.NewSessionSRTP(udpConn, cfg)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("srtp audio: new session: %w", err)
	}

	enc, err := aaceld.New(aacSampleRate, aacBitrate)
	if err != nil {
		logger.Warn("srtp audio: no AAC-ELD encoder, no sound", "error", err)
		enc = nil
	}

	writer, err := session.OpenWriteStream()
	if err != nil {
		_ = session.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("srtp audio: open write stream: %w", err)
	}

	logger.Info("srtp audio: sender ready",
		"controller", fmt.Sprintf("%s:%d", controllerIP, controllerPort),
		"ssrc", ssrc,
		"pt", payloadType)

	return &srtpAudioSender{
		logger:      logger,
		conn:        udpConn,
		session:     session,
		writer:      writer,
		ssrc:        ssrc,
		payloadType: payloadType,
		seq:         uint16(time.Now().UnixNano() & 0xFFFF),
		ts:          uint32(time.Now().UnixNano() & 0xFFFFFFFF),
		enc:         enc,
		silence:     make([]int16, aaceld.FrameSamples),
	}, nil
}

// sendFrameLocked encodes one frame and sends it as the next packet. The
// clock moves on even when the encoder, filling up, returns nothing.
func (s *srtpAudioSender) sendFrameLocked(pcm []int16) error {
	au, err := s.enc.Encode(pcm)
	ts := s.ts
	s.ts += aaceld.FrameSamples
	if err != nil || len(au) == 0 {
		return err
	}
	pkt := packetizeAAC(au, s.ssrc, s.seq, ts, s.payloadType)
	s.seq++
	_, err = s.writer.WriteRTP(&pkt.Header, pkt.Payload)
	return err
}

// SendPCM encodes the microphone's PCM (s16le, 16 kHz, mono) and sends it
// a frame at a time; what is left waits for the next call.
func (s *srtpAudioSender) SendPCM(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.enc == nil {
		return nil
	}
	for i := 0; i+1 < len(data); i += 2 {
		s.pcm = append(s.pcm, int16(binary.LittleEndian.Uint16(data[i:])))
	}
	for len(s.pcm) >= aaceld.FrameSamples {
		err := s.sendFrameLocked(s.pcm[:aaceld.FrameSamples])
		s.pcm = s.pcm[aaceld.FrameSamples:]
		if err != nil {
			return err
		}
		s.lastReal = time.Now()
	}
	s.pcm = append(s.pcm[:0:0], s.pcm...) // don't grow the backing array forever
	return nil
}

// RunSilence sends silence, a frame at a time, while no real audio comes,
// until ctx is cancelled: iOS drops a session without audio.
func (s *srtpAudioSender) RunSilence(ctx context.Context) {
	every := time.Duration(aaceld.FrameSamples) * time.Second / aacSampleRate
	if s.enc == nil {
		every = 20 * time.Millisecond
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			var err error
			switch {
			case time.Since(s.lastReal) <= realAudioHold:
			case s.enc != nil:
				err = s.sendFrameLocked(s.silence)
			default:
				header := makeAudioRTPHeader(s.payloadType, s.seq, s.ts, s.ssrc)
				s.seq++
				s.ts += 960 // 20 ms at Opus's 48 kHz RTP clock
				_, err = s.writer.WriteRTP(header, opusSilencePacket)
			}
			s.mu.Unlock()
			if err != nil {
				s.logger.Debug("srtp audio: write failed", "error", err)
				return
			}
		}
	}
}

// Close shuts down the SRTP session and closes the UDP socket.
func (s *srtpAudioSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.enc != nil {
		s.enc.Close()
	}
	if s.session != nil {
		_ = s.session.Close()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	return nil
}
