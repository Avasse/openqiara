// Package hlsserver serves the camera as HLS, from the media hub, in place
// of the vendor's hls: the 1080p stream and, with the multicast source,
// the microphone as AAC-LC, in MPEG-TS segments every player reads
// (Safari, VLC, Home Assistant). The URLs stay those hls served:
// /stream/HLS_TEST.m3u8 and /stream/720p/HLS_TEST.m3u8.
//
// The muxer runs only while someone watches: it starts with the first
// request and stops once requests stop.
package hlsserver

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/caligone/openqiara/internal/aacenc"
	"github.com/caligone/openqiara/internal/mediahub"
)

const (
	audioRate    = 16000
	audioBitrate = 32000
	// readyWait bounds how long a request waits for the first segment
	// (an IDR comes every 0.5 s; the shutter closed, none comes).
	readyWait = 10 * time.Second
)

// Server serves the hub as HLS.
type Server struct {
	hub   *mediahub.Hub
	audio bool
	log   *slog.Logger
	// idleStop is how long the muxer outlives the last request: players
	// reload the playlist every segment or so.
	idleStop time.Duration

	mu       sync.Mutex
	run      *run // nil while nobody watches
	lastReq  time.Time
	inflight int // requests being served: a playlist waits for content
}

// run is one muxing session, from the first request to idleStop.
type run struct {
	cancel context.CancelFunc
	ready  chan struct{} // closed once muxer is set
	muxer  *gohlslib.Muxer
}

// New returns a server for hub; audio adds the microphone, which the hub
// carries with the multicast source.
func New(hub *mediahub.Hub, audio bool, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{hub: hub, audio: audio, log: logger, idleStop: 30 * time.Second}
}

// ServeHTTP serves the playlists and segments under /stream/.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rn := s.touch()
	defer s.done()
	ctx, cancel := context.WithTimeout(r.Context(), readyWait)
	defer cancel()
	select {
	case <-rn.ready:
	case <-ctx.Done():
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"aucune image (clapet fermé ?)"}`))
		return
	}
	// hls's playlist names, in both folders, are the muxer's main one.
	if path.Base(r.URL.Path) == "HLS_TEST.m3u8" {
		r = r.Clone(r.Context())
		r.URL.Path = "/index.m3u8"
	}
	rn.muxer.Handle(w, r)
}

// touch notes a request and starts a run if none is going.
func (s *Server) touch() *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReq = time.Now()
	s.inflight++
	if s.run == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.run = &run{cancel: cancel, ready: make(chan struct{})}
		go s.mux(ctx, s.run)
		go s.stopWhenIdle(ctx, s.run)
	}
	return s.run
}

// done notes the end of a request.
func (s *Server) done() {
	s.mu.Lock()
	s.lastReq = time.Now()
	s.inflight--
	s.mu.Unlock()
}

// stopWhenIdle ends rn once requests stop.
func (s *Server) stopWhenIdle(ctx context.Context, rn *run) {
	tick := time.NewTicker(s.idleStop / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.mu.Lock()
		idle := s.inflight == 0 && time.Since(s.lastReq) > s.idleStop
		if idle && s.run == rn {
			s.run = nil
		}
		s.mu.Unlock()
		if idle {
			rn.cancel()
			return
		}
	}
}

// mux reads the hub until ctx ends: the muxer starts at the first SPS and
// PPS, which it needs for the playlist.
func (s *Server) mux(ctx context.Context, rn *run) {
	sub := s.hub.Subscribe()
	defer sub.Close()
	defer s.drop(rn)

	var sps, pps []byte
	var au [][]byte
	var auPTS int64
	var video, audio *gohlslib.Track
	var aac *aacenc.Stream

	flush := func() {
		if len(au) == 0 {
			return
		}
		if rn.muxer != nil {
			if err := rn.muxer.WriteH264(video, time.Now(), auPTS, au); err != nil {
				s.log.Debug("hls: write video", "error", err)
			}
		}
		au = nil
	}
	start := func() error {
		video = &gohlslib.Track{Codec: &codecs.H264{SPS: sps, PPS: pps}, ClockRate: 90000}
		m := &gohlslib.Muxer{
			Variant:            gohlslib.MuxerVariantMPEGTS,
			Tracks:             []*gohlslib.Track{video},
			SegmentMinDuration: time.Second,
		}
		if s.audio {
			if a, err := aacenc.NewStream(aacenc.LC, audioRate, audioBitrate); err != nil {
				s.log.Warn("hls: no AAC encoder, video only", "error", err)
			} else {
				aac = a
				audio = &gohlslib.Track{Codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
					Type: mpeg4audio.ObjectTypeAACLC, SampleRate: audioRate, ChannelConfig: 1,
				}}, ClockRate: audioRate}
				m.Tracks = append(m.Tracks, audio)
			}
		}
		if err := m.Start(); err != nil {
			return err
		}
		rn.muxer = m
		close(rn.ready)
		s.log.Info("hls: muxer started", "audio", audio != nil)
		return nil
	}
	defer func() {
		if aac != nil {
			aac.Close()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case sample, ok := <-sub.Samples():
			if !ok {
				return
			}
			if !sample.IsVideo {
				if aac == nil {
					continue
				}
				err := aac.Write(sample.Data, sample.PTS, func(b []byte, pts int64) {
					if err := rn.muxer.WriteMPEG4Audio(audio, time.Now(), pts*audioRate/90000, [][]byte{b}); err != nil {
						s.log.Debug("hls: write audio", "error", err)
					}
				})
				if err != nil {
					s.log.Warn("hls: aac encode", "error", err)
				}
				continue
			}
			if len(au) > 0 && sample.PTS != auPTS {
				flush()
			}
			nal := append([]byte(nil), sample.Data...)
			switch nal[0] & 0x1f {
			case 7:
				sps = nal
			case 8:
				pps = nal
			}
			if rn.muxer == nil && sps != nil && pps != nil {
				if err := start(); err != nil {
					s.log.Warn("hls: muxer start failed", "error", err)
					return
				}
			}
			auPTS = sample.PTS
			au = append(au, nal)
		}
	}
}

// drop closes rn's muxer and forgets rn: the next request starts afresh.
func (s *Server) drop(rn *run) {
	s.mu.Lock()
	if s.run == rn {
		s.run = nil
	}
	s.mu.Unlock()
	rn.cancel()
	if rn.muxer != nil {
		rn.muxer.Close()
		s.log.Info("hls: muxer stopped")
	}
}
