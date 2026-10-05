package domus

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// fakeMCU records the CTRL frames Pair sends.
type fakeMCU struct {
	mu   sync.Mutex
	sent [][]byte
}

func (m *fakeMCU) GetInfo(context.Context) (*charmux.MCUInfo, error) { return &charmux.MCUInfo{}, nil }
func (m *fakeMCU) GetNet(context.Context) (byte, error)              { return 5, nil }
func (m *fakeMCU) SendRawCTRL(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, append([]byte(nil), b...))
	return nil
}

func (m *fakeMCU) ops() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ops []byte
	for _, f := range m.sent {
		ops = append(ops, f[0])
	}
	return ops
}

// A made-up vendor key: the real ones stay on the camera.
var testKey = VendorKey{Name: "test", Key: [32]byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 31: 0xff}}

var testUID = []byte{1, 2, 3, 4, 5, 6, 7, 8}

func beacon() []byte {
	f := append([]byte{opBeacon}, testKey.Key[:6]...)
	f = append(f, testUID...)
	return append(f, []byte("HOMELABDWS00ACFD")...)
}

// TestPairHandshake: the MCU's frames get fbxhome's answers, in order,
// the address comes from the result frame, and a sensor of another type
// in pairing mode is left alone.
func TestPairHandshake(t *testing.T) {
	mcu := &fakeMCU{}
	frames := make(chan []byte, 8)
	frames <- []byte{0x15}                                         // the MCU's reply to START_PAIRING, ignored
	frames <- append(beacon()[:15], []byte("HOMELABPIR00ACFD")...) // not the type asked for
	frames <- beacon()
	frames <- append([]byte{opPairChallenge}, testUID...)
	frames <- append(append([]byte{opPairResult}, testUID...), 7, 0, 0, 0)
	frames <- []byte{opStopPairing}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	res, err := Pair(context.Background(), mcu, frames, []VendorKey{testKey}, 7, "HOMELABDWS", log)
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != 7 || res.Model != "HOMELABDWS00ACFD" || !bytes.Equal(res.DeviceUID[:], testUID) {
		t.Errorf("result = %+v", res)
	}
	want := []byte{opStartPairing, opStartPairing, opPairRequest, opPairConfirm, opStopPairing}
	if got := mcu.ops(); !bytes.Equal(got, want) {
		t.Errorf("CTRL sent %x, want %x", got, want)
	}
	start, request := mcu.sent[0], mcu.sent[2]
	if len(start) != 18 || start[1] != 7 || start[5] != 7 {
		t.Errorf("start = %x, want the address at 1 and 5", start)
	}
	if !bytes.Equal(request[1:9], testUID) || !bytes.Equal(request[9:41], testKey.Key[:]) {
		t.Errorf("pair request = %x", request)
	}
}

// TestPairCancelStopsTheMCU: giving up leaves the MCU out of pairing mode.
func TestPairCancelStopsTheMCU(t *testing.T) {
	mcu := &fakeMCU{}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := Pair(ctx, mcu, make(chan []byte), []VendorKey{testKey}, 7, "HOMELABDWS", log); err == nil {
		t.Fatal("pairing without a sensor succeeded")
	}
	ops := mcu.ops()
	if ops[len(ops)-1] != opStopPairing {
		t.Errorf("CTRL sent %x, want a final stop", ops)
	}
}
