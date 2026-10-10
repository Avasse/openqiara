package publisher

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestSendPCMFramesAAC(t *testing.T) {
	ios, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ios.Close()
	key, salt := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 14)
	port := uint16(ios.LocalAddr().(*net.UDPAddr).Port)
	s, err := newSRTPAudioSender(net.IPv4(127, 0, 0, 1), port, key, salt, key, salt, 1, 110,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.enc == nil {
		t.Skip("libfdk-aac absent: the camera has it")
	}

	start := s.ts
	packet := make([]byte, 2048) // one hlcamd packet: 1024 samples, 64 ms
	for i := range 2 {
		if err := s.SendPCM(packet); err != nil {
			t.Fatal(err)
		}
		// 1024 then 1088 samples waiting: 2 frames of 480 each time.
		if got, want := s.ts-start, uint32(2*(i+1)*480); got != want {
			t.Fatalf("after packet %d: RTP clock moved %d, want %d", i, got, want)
		}
	}
	if len(s.pcm) != 2048-4*480 {
		t.Fatalf("%d samples left, want %d", len(s.pcm), 2048-4*480)
	}

	_ = ios.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1500)
	for i := range 4 {
		if _, err := ios.Read(buf); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
}
