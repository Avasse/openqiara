package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestMCUWatchdogFeeds: once watchdog_mcu is stopped and the port taken,
// only 0x05 goes to the MCU, every tick, and nothing while unhealthy.
func TestMCUWatchdogFeeds(t *testing.T) {
	mcu, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mcu.Close() }()

	var healthy atomic.Bool
	healthy.Store(true)
	var stopped atomic.Bool
	w := &mcuWatchdog{
		local: "127.0.0.1:0", mcu: mcu.LocalAddr().String(), every: 20 * time.Millisecond,
		health: func() error {
			if healthy.Load() {
				return nil
			}
			return errors.New("stuck")
		},
		takeOver: func() error { stopped.Store(true); return nil },
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)

	read := func(within time.Duration) (byte, bool) {
		_ = mcu.SetReadDeadline(time.Now().Add(within))
		buf := make([]byte, 8)
		n, _, err := mcu.ReadFromUDP(buf)
		if err != nil || n != 1 {
			return 0, false
		}
		return buf[0], true
	}
	for range 3 {
		if b, ok := read(time.Second); !ok || b != 0x05 {
			t.Fatalf("fed %#x (%v), want 0x05", b, ok)
		}
	}
	if !stopped.Load() {
		t.Error("watchdog_mcu not stopped first")
	}

	healthy.Store(false)
	time.Sleep(30 * time.Millisecond)
	for {
		if _, ok := read(10 * time.Millisecond); !ok {
			break // drained what was sent before
		}
	}
	if _, ok := read(100 * time.Millisecond); ok {
		t.Error("fed while unhealthy")
	}
	healthy.Store(true)
	if b, ok := read(time.Second); !ok || b != 0x05 {
		t.Error("not fed again once healthy")
	}
}
