package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The MCU's hardware watchdog (RE 2026-10-10, re_reports/20261010/
// watchdog.md): charmux links its channel 3 to UDP 127.0.0.1:8005→8004,
// and only datagrams from 8005 get through. One byte per command, no
// answer: 0x05 arms it for 5 min, again and again. 0x03 and 0x04 reset the
// camera at once when they come late: never sent here. Armed for 2 min when
// the MCU starts, it cannot be disarmed: whoever holds 8005 must feed it.
const (
	mcuWatchdogFeed  = 0x05
	mcuWatchdogEvery = time.Minute
)

// mcuWatchdog feeds the MCU's watchdog in place of the vendor's
// watchdog_mcu, which fed it only while the dead Free cloud's VPN kept
// exchanging keys. Fed while openqiarad is healthy, the camera restarts
// within 5 to 6 min of openqiarad stopping, or of health failing: a stuck
// gateway or alarm, hlcamd gone, a radio silent with a siren paired.
// Nothing outside the camera counts (Wi-Fi, MQTT, Home Assistant): a
// restart would not mend it, and the alarm works without.
type mcuWatchdog struct {
	local, mcu string // UDP addresses: 127.0.0.1:8005 and :8004 on the camera
	every      time.Duration
	health     func() error
	takeOver   func() error // stops watchdog_mcu
	// tookOver runs once the watchdog is held: what only watchdog_mcu
	// needed can go.
	tookOver func()
	logger   *slog.Logger
}

func newMCUWatchdog(health func() error, logger *slog.Logger) *mcuWatchdog {
	return &mcuWatchdog{
		local: "127.0.0.1:8005", mcu: "127.0.0.1:8004", every: mcuWatchdogEvery,
		health: health, logger: logger,
		takeOver: func() error {
			out, err := exec.Command("fbxupstartctl", "stop", "watchdog").CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		tookOver: func() { stopDeadCloud(logger) },
	}
}

// deadCloud are the vendor's services for the dead Free/Qiara cloud.
// myriadvpn, its VPN, kept watchdog_mcu feeding (and its death reboots
// the camera while watchdog_mcu runs: stopped only once the watchdog is
// ours). downloader polls an update URL our dnsmasq points at the camera
// itself and writes what it gets to the SD card. srt-daemon serves an SRT
// stream nothing uses since fbxhome is gone.
var deadCloud = []string{"myriadvpn", "downloader", "srt-daemon"}

func stopDeadCloud(logger *slog.Logger) {
	for _, svc := range deadCloud {
		if out, err := exec.Command("fbxupstartctl", "stop", svc).CombinedOutput(); err != nil {
			logger.Warn("watchdog: stopping a dead cloud service failed", "service", svc, "error", err,
				"output", strings.TrimSpace(string(out)))
		}
	}
	logger.Info("watchdog: dead cloud services stopped", "services", deadCloud)
}

// run takes the watchdog over and feeds it until ctx ends. The vendor's
// watchdog_mcu is never started again: its first command, 0x03, would reset
// the camera. If 8005 cannot be had, nothing feeds the watchdog and the
// camera restarts within 5 min, which is the way back.
func (w *mcuWatchdog) run(ctx context.Context) {
	if err := w.takeOver(); err != nil {
		w.logger.Warn("watchdog: stopping watchdog_mcu failed", "error", err)
	}
	conn, err := w.bind(ctx)
	if err != nil {
		w.logger.Error("watchdog: cannot hold the MCU's watchdog port, the camera will restart", "error", err)
		return
	}
	defer conn.Close()
	w.logger.Info("watchdog: feeding the MCU's watchdog", "every", w.every)
	if w.tookOver != nil {
		w.tookOver()
	}

	tick := time.NewTicker(w.every)
	defer tick.Stop()
	var failing error
	for {
		err := w.health()
		switch {
		case err == nil:
			if _, err := conn.Write([]byte{mcuWatchdogFeed}); err != nil {
				w.logger.Warn("watchdog: feed failed", "error", err)
			}
			if failing != nil {
				w.logger.Info("watchdog: healthy again, feeding")
			}
		case failing == nil:
			w.logger.Error("watchdog: unhealthy, no longer feeding: the camera restarts within 6 min unless it mends", "error", err)
		}
		failing = err
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// bind takes the local port, which watchdog_mcu frees as it stops.
func (w *mcuWatchdog) bind(ctx context.Context) (*net.UDPConn, error) {
	laddr, err := net.ResolveUDPAddr("udp4", w.local)
	if err != nil {
		return nil, err
	}
	raddr, err := net.ResolveUDPAddr("udp4", w.mcu)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Minute)
	for {
		conn, err := net.DialUDP("udp4", laddr, raddr)
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// hlcamdRunning tells whether hlcamd runs, as watchdog_mcu checked it (a
// ping on fbxbus there): the video pipeline, which openqiarad does not
// start again.
func hlcamdRunning() error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if comm, err := os.ReadFile("/proc/" + e.Name() + "/comm"); err == nil && strings.TrimSpace(string(comm)) == "hlcamd" {
			return nil
		}
	}
	return errors.New("hlcamd is not running")
}
