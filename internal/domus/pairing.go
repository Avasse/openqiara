package domus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Pairing opcodes on the CTRL channel.
const (
	opStartPairing  = 0x15
	opStopPairing   = 0x16
	opBeacon        = 0x17
	opPairRequest   = 0x1a
	opPairChallenge = 0x1f
	opPairConfirm   = 0x1c
	opPairResult    = 0x1e
)

// PairingResult is what the MCU and the sensor agreed on.
type PairingResult struct {
	DeviceUID  [8]byte
	Model      string // from the beacon, e.g. HOMELABDWS00ACFD
	VendorName string
	Address    byte // radio address the MCU assigned
}

// MCU is the part of charmux.Client that pairing drives.
type MCU interface {
	GetInfo(ctx context.Context) (*charmux.MCUInfo, error)
	GetNet(ctx context.Context) (byte, error)
	SendRawCTRL(data []byte) error
	SendWatchdog()
}

// Pair adds a sensor to the MCU's radio network with the CTRL handshake
// captured from fbxhome (2026-03-31), in the order that persists in the
// MCU (memory feedback_pairing_protocol):
//
//  1. GetInfo, GetNet, then START_PAIRING (0x15) twice with a watchdog
//     ping in between: the first one wakes the MCU up.
//  2. Beacon (0x17) of the sensor put in pairing mode → pair request
//     (0x1a) with the vendor key matching the beacon.
//  3. Challenge (0x1f) → confirm (0x1c).
//  4. Result (0x1e) with the address, then stop (0x16), echoed: without
//     the echo the pairing does not persist.
//
// frames carries the CTRL frames the MCU sends meanwhile. addr is the
// address to give the sensor: the MCU assigns it as is
// (dws_repair_2026-05-16.pcap), so it must be unused. Only a beacon whose
// model starts with model (e.g. HOMELABPIR) is taken. The sensor then
// sends a status heartbeat and gets provisioned like after any reboot,
// which is the radio engine's job. Cancelling ctx stops the pairing.
func Pair(ctx context.Context, mcu MCU, frames <-chan []byte, keys []VendorKey, addr byte, model string, log *slog.Logger) (*PairingResult, error) {
	warmUp(ctx, "GetInfo", func(ctx context.Context) error { _, err := mcu.GetInfo(ctx); return err }, log)
	warmUp(ctx, "GetNet", func(ctx context.Context) error { _, err := mcu.GetNet(ctx); return err }, log)

	start := make([]byte, 18)
	start[0], start[1], start[5] = opStartPairing, addr, addr
	if err := mcu.SendRawCTRL(start); err != nil {
		return nil, fmt.Errorf("pairing: start: %w", err)
	}
	mcu.SendWatchdog()
	if err := sleep(ctx, 500*time.Millisecond); err != nil {
		return nil, stop(mcu, err)
	}
	if err := mcu.SendRawCTRL(start); err != nil {
		return nil, fmt.Errorf("pairing: start: %w", err)
	}
	log.Info("pairing: waiting for a sensor in pairing mode", "addr", addr)

	var res PairingResult
	matched := false
	for {
		var f []byte
		select {
		case <-ctx.Done():
			return nil, stop(mcu, ctx.Err())
		case f = <-frames:
		}
		if len(f) == 0 {
			continue
		}
		var reply []byte
		switch op := f[0] & 0x7f; {
		case op == opBeacon && !matched && len(f) >= 31:
			key, ok := MatchBeacon(f, keys)
			if !ok {
				log.Warn("pairing: beacon of an unknown vendor")
				continue
			}
			if beaconModel := string(trimNull(f[15:31])); !strings.HasPrefix(beaconModel, model) {
				log.Warn("pairing: beacon of another sensor type ignored", "model", beaconModel, "want", model)
				continue
			}
			matched = true
			copy(res.DeviceUID[:], f[7:15])
			res.Model, res.VendorName = string(trimNull(f[15:31])), key.Name
			log.Info("pairing: beacon", "model", res.Model, "vendor", key.Name)
			reply = make([]byte, 57)
			reply[0] = opPairRequest
			copy(reply[1:9], res.DeviceUID[:])
			copy(reply[9:41], key.Key[:])
		case op == opPairChallenge && matched:
			reply = append([]byte{opPairConfirm}, res.DeviceUID[:]...)
		case op == opPairResult && matched && len(f) >= 10:
			res.Address = f[9]
		case op == opStopPairing && matched:
			if err := mcu.SendRawCTRL([]byte{opStopPairing}); err != nil {
				return nil, fmt.Errorf("pairing: stop echo: %w", err)
			}
			mcu.SendWatchdog()
			if res.Address == 0 {
				return nil, errors.New("pairing: the MCU stopped before assigning an address")
			}
			log.Info("pairing: done", "model", res.Model, "addr", res.Address)
			return &res, nil
		}
		if reply != nil {
			if err := mcu.SendRawCTRL(reply); err != nil {
				return nil, stop(mcu, fmt.Errorf("pairing: send 0x%02x: %w", reply[0], err))
			}
		}
	}
}

// warmUp runs a CTRL request the way fbxhome opens a pairing, bounded so
// that a silent MCU does not eat the pairing window.
func warmUp(ctx context.Context, name string, req func(context.Context) error, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := req(ctx); err != nil {
		log.Warn("pairing: "+name+" failed", "error", err)
	}
}

// stop takes the MCU out of pairing mode after a failure.
func stop(mcu MCU, cause error) error {
	_ = mcu.SendRawCTRL([]byte{opStopPairing})
	return cause
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func trimNull(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
