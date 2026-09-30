package radio

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Paths the fbxhome transcripts do not exercise.

const siren = 6

// fromSensor builds a frame as the MCU delivers it from a sensor: flag Z
// asks for an answer, 0x80 is set on every sensor frame.
func fromSensor(addr, counter uint32, z bool, wflags byte, payload ...byte) charmux.ManagedFrame {
	f := charmux.ManagedFrame{GWDst: 1, GWSrc: addr, Counter: counter, Src: addr, Flags: 0x80,
		WFlags: wflags, Payload: payload}
	if z {
		f.Flags |= charmux.FlagZ
	}
	if wflags != 0 {
		f.Flags |= charmux.FlagW
	}
	return f
}

func readStatusAnswer(addr, counter uint32, status byte) charmux.ManagedFrame {
	p := []byte{status, readStatusAll, 0xe7, 0xaa, 0xb7, 0xec, 0xed, 0x2e, 0x45, 0x63}
	return fromSensor(addr, counter, true, wfManageAnswer, append(p, make([]byte, 9)...)...)
}

// oneSend returns the single frame of r, failing otherwise.
func oneSend(t *testing.T, r Result) charmux.ManagedFrame {
	t.Helper()
	if len(r.Send) != 1 {
		t.Fatalf("sent %d frames, want 1", len(r.Send))
	}
	return r.Send[0]
}

// rebootedSiren walks a siren through status, answer and bytecode push:
// one VM frame of 3 ops.
func rebootedSiren(t *testing.T, now time.Time) (*Engine, charmux.ManagedFrame) {
	t.Helper()
	vm := []VMFrame{{Payload: []byte{0x01, 0x80, 0x88, 0x82, 0, 0, 8, 0, 0, 0}, Ops: 3}}
	e := New(Options{Gateway: 1, Nodes: []Node{{Addr: siren, Model: SRN}},
		Bytecode: func([]byte) ([]VMFrame, error) { return vm, nil }})
	oneSend(t, e.Receive(now, fromSensor(siren, 10, true, wfStatus, 0xf1, 0x49, 0x01)))
	push := oneSend(t, e.Receive(now, readStatusAnswer(siren, 11, 0xd1)))
	if push.WFlags != wfBytecode {
		t.Fatalf("answer to a sensor needing bytecode: %+v", push)
	}
	return e, push
}

func TestTimeFrameCarriesTheClock(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 31, 13, 0, time.UTC)
	e, _ := rebootedSiren(t, now)
	tf := oneSend(t, e.Receive(now, fromSensor(siren, 12, true, wfManageAnswer, 0x07, 0, 0, 0, 0)))
	if tf.WFlags != wfTime || binary.LittleEndian.Uint32(tf.Payload) != uint32(now.Unix()-qiaraEpoch) {
		t.Fatalf("time frame %+v", tf)
	}
	cfg := oneSend(t, e.Receive(now, fromSensor(siren, 13, true, 0)))
	if cfg.WFlags != wfApp || !bytes.Equal(cfg.Payload, []byte{0x55, 0x06}) {
		t.Fatalf("after the time, the siren gets its config: %+v", cfg)
	}
}

func TestFailedVMOpStopsThePush(t *testing.T) {
	e, _ := rebootedSiren(t, time.Now())
	got := oneSend(t, e.Receive(time.Now(), fromSensor(siren, 12, true, wfManageAnswer, 0x03, 0, 0, 0, 0)))
	if got.Flags != flagsAck || got.WFlags != 0 {
		t.Fatalf("op 3 failed, the push must stop with a bare ack: %+v", got)
	}
}

func TestDeliveryFailureDropsTheDialogue(t *testing.T) {
	e, push := rebootedSiren(t, time.Now())
	report := charmux.ManagedFrame{GWDst: 1, GWSrc: 1, Src: 1, Flags: 0x0040, AckDst: 1, AckCnt: push.Counter}
	res := e.Receive(time.Now(), report)
	want := Event{Addr: siren, Kind: DeliveryFailed, Value: 1}
	if len(res.Send) != 0 || len(res.Events) != 1 || res.Events[0] != want {
		t.Fatalf("report: %+v, want only %+v", res, want)
	}
	late := oneSend(t, e.Receive(time.Now(), fromSensor(siren, 12, true, wfManageAnswer, 0x07, 0, 0, 0, 0)))
	if late.Flags != flagsAck {
		t.Fatalf("an ack after the dialogue was dropped gets a bare ack: %+v", late)
	}
}

func TestCounterIsGlobal(t *testing.T) {
	e := New(Options{Gateway: 1, Nodes: []Node{{Addr: 3, Model: PIR}, {Addr: 5, Model: DWS}}})
	event := []byte{0x55, 0x01, 0, 0, 0, 0, 0x41, 0x00}
	for i, addr := range []uint32{3, 5, 3, 5} {
		got := oneSend(t, e.Receive(time.Now(), fromSensor(addr, 100, true, wfApp, event...)))
		if got.Counter != uint32(i) {
			t.Fatalf("frame %d to %d has counter %d", i, addr, got.Counter)
		}
	}
}

func TestKeypadWakeWithoutPIN(t *testing.T) {
	// Not seen in the transcripts: fbxhome always had a PIN to push.
	e := New(Options{Gateway: 1, Nodes: []Node{{Addr: 2, Model: KPD}}})
	got := oneSend(t, e.Receive(time.Now(), fromSensor(2, 7, true, wfApp, 0x55, 0x09)))
	if got.Flags != flagsEventAck || got.WFlags != 0 {
		t.Fatalf("keypad wake without PIN: %+v", got)
	}
}

func TestConfigWithoutSiren(t *testing.T) {
	e := New(Options{Gateway: 1, Nodes: []Node{{Addr: 5, Model: DWS, SystemIndex: 2}}})
	oneSend(t, e.Receive(time.Now(), fromSensor(5, 1, true, wfStatus, 0x81, 0xff)))
	cfg := oneSend(t, e.Receive(time.Now(), readStatusAnswer(5, 2, 0x81)))
	if !bytes.Equal(cfg.Payload, []byte{0x55, 0x00, 0x02, 0, 0, 0, 0}) {
		t.Fatalf("config without a siren: %x", cfg.Payload)
	}
}

func TestUnknownSensorIsIgnored(t *testing.T) {
	e := New(Options{Gateway: 1})
	if res := e.Receive(time.Now(), fromSensor(9, 1, true, wfStatus, 0x81, 0xff)); len(res.Send)+len(res.Events) != 0 {
		t.Fatalf("unknown sensor: %+v", res)
	}
}
