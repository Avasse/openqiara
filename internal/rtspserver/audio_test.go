package rtspserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/caligone/openqiara/internal/aacenc"
	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mediahub"
)

// fakeCamera sends, every 64 ms, an IDR with its parameter sets and a
// packet of PCM, as the multicast source does.
func fakeCamera(ctx context.Context) (<-chan camera.Sample, error) {
	out := make(chan camera.Sample)
	go func() {
		defer close(out)
		pcm := make([]byte, 2048)
		for i := range pcm {
			pcm[i] = byte(i * 7)
		}
		tick := time.NewTicker(64 * time.Millisecond)
		defer tick.Stop()
		for pts := int64(0); ; pts += 5760 {
			for _, s := range []camera.Sample{
				{IsVideo: true, PTS: pts, Data: []byte{0x67, 0x42, 0xc0, 0x1f}},
				{IsVideo: true, PTS: pts, Data: []byte{0x68, 0xce, 0x3c, 0x80}},
				{IsVideo: true, PTS: pts, Data: []byte{0x65, 0x88, 0x84, 0x00}},
				{PTS: pts, Data: pcm},
			} {
				select {
				case out <- s:
				case <-ctx.Done():
					return
				}
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

// play starts a server on the fake camera, plays it with a real client
// for up to 3 s and counts the video and audio packets.
func play(t *testing.T, audio bool) (medias int, video, sound int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := mediahub.New("fake", fakeCamera, nil, logger)
	srv := New(Config{Listen: addr, Path: "openqiara", Audio: audio}, hub, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}

	c := gortsplib.Client{Scheme: "rtsp", Host: addr}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u, err := base.ParseURL("rtsp://" + addr + "/openqiara")
	if err != nil {
		t.Fatal(err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	var v, a atomic.Int32
	c.OnPacketRTPAny(func(m *description.Media, f format.Format, _ *rtp.Packet) {
		if _, ok := f.(*format.MPEG4Audio); ok {
			a.Add(1)
		} else {
			v.Add(1)
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	enough := func() bool { return v.Load() >= 5 && (!audio || a.Load() >= 5) }
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !enough(); {
		time.Sleep(50 * time.Millisecond)
	}
	return len(desc.Medias), v.Load(), a.Load()
}

func TestRTSPServesVideo(t *testing.T) {
	medias, video, _ := play(t, false)
	if medias != 1 || video < 5 {
		t.Fatalf("%d medias, %d video packets; want 1 and some", medias, video)
	}
}

func TestRTSPServesAudio(t *testing.T) {
	if e, err := aacenc.New(aacenc.LC, audioRate, audioBitrate); err != nil {
		t.Skip(err) // libfdk-aac absent: the camera has it
	} else {
		e.Close()
	}
	medias, video, audio := play(t, true)
	if medias != 2 || video < 5 || audio < 5 {
		t.Fatalf("%d medias, %d video and %d audio packets", medias, video, audio)
	}
}
