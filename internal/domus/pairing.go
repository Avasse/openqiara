package domus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	SendRawCTRL(data []byte) error
}

// Pair adds a sensor to the MCU's radio network with fbxhome's CTRL
// handshake, as captured in dws_repair_2026-05-16.pcap:
//
//  1. START_PAIRING (0x15), once.
//  2. Beacon (0x17) of the sensor put in pairing mode → pair request
//     (0x1a) with the vendor key matching the beacon.
//  3. Challenge (0x1f) → confirm (0x1c).
//  4. Result (0x1e) with the address, then the MCU's stop (0x16).
//
// Nothing more: the GetInfo/GetNet warm-up, the second 0x15 and the echo
// of the MCU's 0x16 came from trials in April 2026, not from fbxhome, and
// after that echo the MCU no longer answered CTRL until a reboot (seen
// live 2026-10-05). The pairing persists without them.
//
// frames carries the CTRL frames the MCU sends meanwhile. addr is the
// address to give the sensor: the MCU assigns it as is
// (dws_repair_2026-05-16.pcap), so it must be unused. Only a beacon whose
// model starts with model (e.g. HOMELABPIR) is taken. The sensor then
// sends a status heartbeat and gets provisioned like after any reboot,
// which is the radio engine's job. Cancelling ctx stops the pairing.
func Pair(ctx context.Context, mcu MCU, frames <-chan []byte, keys []VendorKey, addr byte, model string, log *slog.Logger) (*PairingResult, error) {
	start := make([]byte, 18)
	start[0], start[1], start[5] = opStartPairing, addr, addr
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

// stop takes the MCU out of pairing mode when the pairing is given up.
func stop(mcu MCU, cause error) error {
	_ = mcu.SendRawCTRL([]byte{opStopPairing})
	return cause
}

func trimNull(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
