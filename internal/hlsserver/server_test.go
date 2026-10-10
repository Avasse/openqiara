package hlsserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mediahub"
)

// A 1920x1080 Constrained Baseline SPS and its PPS, as hlcamd sends them.
var (
	sps = []byte{0x67, 0x42, 0x40, 0x28, 0xa6, 0x80, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x0f, 0xa4, 0x00, 0x03, 0xaa, 0x70, 0x10}
	pps = []byte{0x68, 0xca, 0x8f, 0x20}
)

// fakeCamera sends, at 30 fps, an IDR with its parameter sets every 15
// frames, and PCM every 64 ms, as the multicast source does.
func fakeCamera(ctx context.Context) (<-chan camera.Sample, error) {
	out := make(chan camera.Sample)
	go func() {
		defer close(out)
		pcm := make([]byte, 2048)
		send := func(s camera.Sample) bool {
			select {
			case out <- s:
				return true
			case <-ctx.Done():
				return false
			}
		}
		tick := time.NewTicker(time.Second / 30)
		defer tick.Stop()
		for f := int64(0); ; f++ {
			pts := f * 3000
			nals := [][]byte{{0x41, 0x9a, 0x00}}
			if f%15 == 0 {
				nals = [][]byte{sps, pps, {0x65, 0x88, 0x84, 0x00}}
			}
			for _, n := range nals {
				if !send(camera.Sample{IsVideo: true, PTS: pts, Data: n}) {
					return
				}
			}
			if f%2 == 0 && !send(camera.Sample{PTS: pts, Data: pcm}) {
				return
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func get(t *testing.T, h http.Handler, url string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestServesHLSAtTheOldURLs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := mediahub.New("fake", fakeCamera, nil, logger)
	srv := New(hub, true, logger)
	srv.idleStop = 300 * time.Millisecond

	for _, base := range []string{"/stream/", "/stream/720p/"} {
		main := get(t, srv, base+"HLS_TEST.m3u8")
		if !strings.Contains(main, "RESOLUTION=1920x1080") {
			t.Fatalf("multivariant playlist:\n%s", main)
		}
		media := regexp.MustCompile(`(?m)^(\S+_stream\.m3u8)`).FindString(main)
		if media == "" {
			t.Fatalf("no media playlist in:\n%s", main)
		}
		pl := get(t, srv, base+media)
		seg := regexp.MustCompile(`(?m)^(\S+\.ts)`).FindString(pl)
		if seg == "" {
			t.Fatalf("no segment in:\n%s", pl)
		}
		if ts := get(t, srv, base+seg); len(ts) == 0 || ts[0] != 0x47 {
			t.Fatalf("segment is not MPEG-TS (%d bytes)", len(ts))
		}
	}

	// Nobody watches: the muxer stops, and starts again on demand.
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		stopped := srv.run == nil
		srv.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("muxer still running with nobody watching")
		}
		time.Sleep(50 * time.Millisecond)
	}
	get(t, srv, "/stream/720p/HLS_TEST.m3u8")
}
