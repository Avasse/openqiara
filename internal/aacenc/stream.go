package aacenc

import "encoding/binary"

// Stream encodes a PCM stream (s16le, mono) as it comes, a frame at a
// time, each access unit stamped with the 90 kHz PTS of its first sample.
type Stream struct {
	enc  *Encoder
	rate int
	pcm  []int16
	pts  int64 // 90 kHz PTS of pcm[0]
}

// NewStream opens an encoder of profile p for PCM at rate.
func NewStream(p Profile, rate, bitrate int) (*Stream, error) {
	enc, err := New(p, rate, bitrate)
	if err != nil {
		return nil, err
	}
	return &Stream{enc: enc, rate: rate}, nil
}

// Write takes PCM whose first sample has PTS pts and hands emit every
// access unit it completes; what is left waits for the next call.
func (s *Stream) Write(pcm []byte, pts int64, emit func(au []byte, pts int64)) error {
	if len(s.pcm) == 0 {
		s.pts = pts
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		s.pcm = append(s.pcm, int16(binary.LittleEndian.Uint16(pcm[i:])))
	}
	for len(s.pcm) >= s.enc.frame {
		au, err := s.enc.Encode(s.pcm[:s.enc.frame])
		s.pcm = s.pcm[s.enc.frame:]
		pts := s.pts
		s.pts += int64(s.enc.frame) * 90000 / int64(s.rate)
		if err != nil {
			return err
		}
		if len(au) > 0 {
			emit(au, pts)
		}
	}
	s.pcm = append(s.pcm[:0:0], s.pcm...) // don't grow the backing array forever
	return nil
}

// Close frees the encoder.
func (s *Stream) Close() { s.enc.Close() }
