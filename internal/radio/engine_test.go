package radio

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Paths the fbxhome transcripts do not exercise.

const siren = 6

var now = time.Date(2026, 9, 30, 10, 31, 13, 0, time.UTC)

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

func heartbeat(addr, counter uint32, status byte) charmux.ManagedFrame {
	return fromSensor(addr, counter, true, wfStatus, status, 0xff)
}

func readStatusAnswer(addr, counter uint32, status byte) charmux.ManagedFrame {
	p := []byte{status, readStatusAll, 0xe7, 0xaa, 0xb7, 0xec, 0xed, 0x2e, 0x45, 0x63}
	return fromSensor(addr, counter, true, wfManageAnswer, append(p, make([]byte, 9)...)...)
}

func vmAck(addr, counter uint32, mask byte) charmux.ManagedFrame {
	return fromSensor(addr, counter, true, wfManageAnswer, mask, 0, 0, 0, 0)
}

func report(counter uint32) charmux.ManagedFrame {
	return charmux.ManagedFrame{GWDst: 1, GWSrc: 1, Src: 1, Flags: 0x0040, AckDst: 1, AckCnt: counter}
}

func newEngine(t *testing.T, o Options) *Engine {
	t.Helper()
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// oneSend returns the single frame of r, failing otherwise.
func oneSend(t *testing.T, r Result) charmux.ManagedFrame {
	t.Helper()
	if len(r.Send) != 1 {
		t.Fatalf("sent %+v, want one frame", r.Send)
	}
	return r.Send[0]
}

// rebootedSiren walks a siren through status and answer up to its first
// bytecode frame: two frames of 3 ops.
func rebootedSiren(t *testing.T) (*Engine, charmux.ManagedFrame) {
	t.Helper()
	vm := [][]byte{{0x01, 0x80, 0x88, 0x82, 0, 0, 8, 0, 0, 0}, {0x01, 0x80, 0x88, 0x88}}
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: siren, Model: SRN}},
		Bytecode: func([]byte) ([][]byte, error) { return vm, nil }})
	oneSend(t, e.Receive(now, heartbeat(siren, 10, 0xf1)))
	push := oneSend(t, e.Receive(now, readStatusAnswer(siren, 11, 0xd1)))
	if push.WFlags != wfBytecode {
		t.Fatalf("answer to a sensor needing bytecode: %+v", push)
	}
	return e, push
}

func TestTimeFrameCarriesTheClock(t *testing.T) {
	e, _ := rebootedSiren(t)
	oneSend(t, e.Receive(now, vmAck(siren, 12, 0x07)))
	tf := oneSend(t, e.Receive(now, vmAck(siren, 13, 0x07)))
	if tf.WFlags != wfTime || binary.LittleEndian.Uint32(tf.Payload) != uint32(now.Unix()-qiaraEpoch) {
		t.Fatalf("time frame %+v", tf)
	}
	cfg := oneSend(t, e.Receive(now, fromSensor(siren, 14, true, 0)))
	if cfg.WFlags != wfApp || !bytes.Equal(cfg.Payload, []byte{0x55, 0x06}) {
		t.Fatalf("after the time, the siren gets its config: %+v", cfg)
	}
}

// Assumption, not observed: fbxhome's reaction to a failed op is unknown.
func TestFailedVMOpStopsThePush(t *testing.T) {
	e, _ := rebootedSiren(t)
	got := oneSend(t, e.Receive(now, vmAck(siren, 12, 0x03)))
	if got.Flags != flagsAck || got.WFlags != 0 {
		t.Fatalf("op 3 failed, the push must stop with a bare ack: %+v", got)
	}
}

func TestLostConfigIsSentAgain(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, AlarmSiren: siren, Nodes: []Node{{Addr: 5, Model: DWS, SystemIndex: 2}}})
	oneSend(t, e.Receive(now, heartbeat(5, 1, 0x81)))
	cfg := oneSend(t, e.Receive(now, readStatusAnswer(5, 2, 0x81)))
	if !bytes.Equal(cfg.Payload, []byte{0x55, 0x00, 0x02, siren, 0, 0, 0}) {
		t.Fatalf("config %x", cfg.Payload)
	}
	res := e.Receive(now, report(cfg.Counter))
	if want := (Event{Addr: 5, Kind: DeliveryFailed, Value: int(cfg.Counter)}); !slices.Equal(res.Events, []Event{want}) {
		t.Fatalf("report: %+v, want %+v", res, want)
	}
	oneSend(t, e.Receive(now, heartbeat(5, 3, 0x81)))
	if again := oneSend(t, e.Receive(now, readStatusAnswer(5, 4, 0x81))); !bytes.Equal(again.Payload, cfg.Payload) {
		t.Fatalf("lost config not sent again: %+v", again)
	}
}

func TestLostFramesAreAttributed(t *testing.T) {
	e, push := rebootedSiren(t)
	first := oneSend(t, e.Command(siren, []byte{0x55, 0x05, 0x00, 0x84}))
	oneSend(t, e.Command(siren, []byte{0x55, 0x05, 0x00, 0x84})) // first is no longer the last sent

	// A lost command is reported with its counter and leaves the push alone.
	if res := e.Receive(now, report(first.Counter)); res.Events[0].Value != int(first.Counter) {
		t.Fatalf("report on the first command: %+v", res)
	}
	if next := oneSend(t, e.Receive(now, vmAck(siren, 12, 0x07))); next.WFlags != wfBytecode {
		t.Fatalf("the push must go on after a lost command: %+v", next)
	}

	// A report with reason 0 (PROCESSING) is not a failure.
	processing := report(push.Counter)
	processing.Flags = 0
	if res := e.Receive(now, processing); len(res.Events) != 0 {
		t.Fatalf("PROCESSING report: %+v", res)
	}
}

func TestLostVMFrameDropsThePush(t *testing.T) {
	e, _ := rebootedSiren(t)
	vm2 := oneSend(t, e.Receive(now, vmAck(siren, 12, 0x07)))
	oneSend(t, e.Command(siren, []byte{0x55, 0x06}))
	e.Receive(now, report(vm2.Counter))
	if got := oneSend(t, e.Receive(now, vmAck(siren, 13, 0x07))); got.Flags != flagsAck || got.WFlags != 0 {
		t.Fatalf("an ack after the push was dropped gets a bare ack: %+v", got)
	}
}

func TestLostUnsentFrame(t *testing.T) {
	e, push := rebootedSiren(t)
	if res := e.Lost(push.Counter); len(res.Events) != 1 || res.Events[0].Kind != DeliveryFailed {
		t.Fatalf("Lost: %+v", res)
	}
}

func TestCounterIsGlobal(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: 3, Model: PIR}, {Addr: 5, Model: DWS}}})
	event := []byte{0x55, 0x01, 0, 0, 0, 0, 0x41, 0x00}
	for i, addr := range []uint32{3, 5, 3, 5} {
		got := oneSend(t, e.Receive(now, fromSensor(addr, 100, true, wfApp, event...)))
		if got.Counter != uint32(i) {
			t.Fatalf("frame %d to %d has counter %d", i, addr, got.Counter)
		}
	}
}

func TestKeypadCodes(t *testing.T) {
	for _, c := range []struct {
		pins []string
		want []byte
	}{
		{nil, []byte{0x00, 0x00}},
		{[]string{"1234"}, []byte{0x03, 0x00, 0x04, 0x10, 0x32}},
		{[]string{"123456"}, []byte{0x04, 0x00, 0x06, 0x10, 0x32, 0x54}},
		{[]string{"1234", "0000"}, []byte{0x06, 0x00, 0x04, 0x10, 0x32, 0x04, 0xaa, 0xaa}},
	} {
		if got := keypadCodes(c.pins); !bytes.Equal(got, c.want) {
			t.Errorf("%q: %x, want %x", c.pins, got, c.want)
		}
	}
	if _, err := New(Options{Nodes: []Node{{Addr: 2, Model: KPD, PINs: []string{"12a4"}}}}); err == nil {
		t.Error("a code with a letter must be refused")
	}
}

func TestKeypadWakeAlwaysGetsItsCodes(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: 2, Model: KPD}}})
	got := oneSend(t, e.Receive(now, fromSensor(2, 7, true, wfApp, 0x55, 0x09)))
	if got.Flags != flagsManage || got.WFlags != wfApp || !bytes.Equal(got.Payload, []byte{0x00, 0x00}) {
		t.Fatalf("keypad wake without codes: %+v", got)
	}
	if err := e.SetNode(Node{Addr: 2, Model: KPD, PINs: []string{"1234"}}); err != nil {
		t.Fatal(err)
	}
	got = oneSend(t, e.Receive(now, fromSensor(2, 8, true, wfApp, 0x55, 0x09)))
	if !bytes.Equal(got.Payload, []byte{0x03, 0x00, 0x04, 0x10, 0x32}) {
		t.Fatalf("new code not pushed at the next wake: %x", got.Payload)
	}
}

// TestStateValues: the value byte of a state report, per sensor type, as
// fbxhome's jump tables read it.
func TestStateValues(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{
		{Addr: 2, Model: KPD}, {Addr: 3, Model: PIR}, {Addr: 5, Model: DWS}, {Addr: siren, Model: SRN}}})
	report := func(addr uint32, kind, value byte, rest ...byte) []Event {
		p := append([]byte{0x55, 0x01, 1, 2, 3, 4, kind, value}, rest...)
		return e.Receive(now, fromSensor(addr, 1, false, wfApp, p...)).Events
	}
	for _, c := range []struct {
		name string
		got  []Event
		want Event
	}{
		{"DWS open", report(5, 0x42, 0), Event{Addr: 5, Kind: Opened}},
		{"DWS closed", report(5, 0x02, 1), Event{Addr: 5, Kind: Closed}},
		{"DWS tamper", report(5, 0x02, 2), Event{Addr: 5, Kind: Tamper}},
		{"PIR start", report(3, 0x41, 0), Event{Addr: 3, Kind: MotionStart}},
		{"PIR end", report(3, 0x41, 1), Event{Addr: 3, Kind: MotionEnd}},
		{"PIR tamper", report(3, 0x41, 2), Event{Addr: 3, Kind: Tamper}},
		{"keypad off button", report(2, 0x04, 0), Event{Addr: 2, Kind: Disarmed}},
		{"keypad code", report(2, 0x84, 0, 0x10, 0x32, 0, 0), Event{Addr: 2, Kind: Disarmed, Code: "1234"}},
		{"keypad day", report(2, 0x04, 1), Event{Addr: 2, Kind: ArmedAway}},
		{"keypad night", report(2, 0x04, 2), Event{Addr: 2, Kind: ArmedNight}},
		{"keypad panic", report(2, 0x04, 3), Event{Addr: 2, Kind: Emergency}},
		{"keypad tamper", report(2, 0x04, 4), Event{Addr: 2, Kind: Tamper}},
		{"siren armed", report(siren, 0x00, 3), Event{Addr: siren, Kind: SirenState, Value: 3}},
		{"DWS unknown value", report(5, 0x02, 7), Event{Addr: 5, Kind: Unhandled, Value: 7}},
	} {
		if !slices.Equal(c.got, []Event{c.want}) {
			t.Errorf("%s: %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

func TestHeartbeatEvents(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: siren, Model: SRN}}})
	if got := e.Receive(now, fromSensor(siren, 1, false, wfStatus, 0x81, 0x49)).Events; !slices.Equal(got, []Event{{Addr: siren, Kind: Battery, Value: 73}}) {
		t.Errorf("heartbeat 81 49: %+v", got)
	}
	if got := e.Receive(now, fromSensor(siren, 2, false, wfStatus, 0xf1, 0x00, 0x01)).Events; !slices.Equal(got, []Event{{Addr: siren, Kind: Rebooted}}) {
		t.Errorf("heartbeat of a rebooted siren: %+v", got)
	}
}

func TestStartConfiguresSirensInOrder(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: 9, Model: SRN}, {Addr: siren, Model: SRN}}})
	start := e.Start().Send
	if len(start) != 2 || start[0].Route != siren || start[1].Route != 9 {
		t.Fatalf("Start: %+v", start)
	}
	oneSend(t, e.Receive(now, heartbeat(siren, 1, 0x81)))
	if got := oneSend(t, e.Receive(now, readStatusAnswer(siren, 2, 0x81))); got.Flags != flagsAck {
		t.Fatalf("a siren configured by Start gets a bare ack: %+v", got)
	}
}

func TestReconfiguration(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, AlarmSiren: siren, Nodes: []Node{{Addr: 5, Model: DWS, SystemIndex: 2}}})
	config := func(counter uint32) []byte {
		oneSend(t, e.Receive(now, heartbeat(5, counter, 0x81)))
		return oneSend(t, e.Receive(now, readStatusAnswer(5, counter+1, 0x81))).Payload
	}
	config(1)
	if got := config(3); len(got) != 0 {
		t.Fatalf("config sent twice: %x", got)
	}
	e.SetAlarmSiren(9)
	if got := config(5); !bytes.Equal(got, []byte{0x55, 0x00, 0x02, 9, 0, 0, 0}) {
		t.Fatalf("after a new siren: %x", got)
	}
	if err := e.SetNode(Node{Addr: 5, Model: DWS, SystemIndex: 4}); err != nil {
		t.Fatal(err)
	}
	if got := config(7); !bytes.Equal(got, []byte{0x55, 0x00, 0x04, 9, 0, 0, 0}) {
		t.Fatalf("after a new system index: %x", got)
	}
	e.RemoveNode(5)
	if got := e.Receive(now, heartbeat(5, 9, 0x81)); len(got.Send) != 0 || got.Events[0].Kind != UnknownSensor {
		t.Fatalf("removed sensor: %+v", got)
	}
}

func TestObservability(t *testing.T) {
	e := newEngine(t, Options{Gateway: 1, Nodes: []Node{{Addr: 5, Model: DWS}}})
	if got := e.Receive(now, heartbeat(9, 1, 0x81)); len(got.Send) != 0 || got.Events[0] != (Event{Addr: 9, Kind: UnknownSensor}) {
		t.Errorf("unknown sensor: %+v", got)
	}
	res := e.Receive(now, fromSensor(5, 1, true, 0x3f, 0x01))
	if oneSend(t, res).Flags != flagsAck || !slices.Contains(res.Events, Event{Addr: 5, Kind: Unhandled, Value: 0x3f}) {
		t.Errorf("Z frame outside the reaction table: %+v", res)
	}
	oneSend(t, e.Receive(now, heartbeat(5, 2, 0xd1)))
	res = e.Receive(now, readStatusAnswer(5, 3, 0xd1))
	if oneSend(t, res).WFlags != wfTime || !slices.Contains(res.Events, Event{Addr: 5, Kind: NoBytecode}) {
		t.Errorf("sensor needing bytecode with none available: %+v", res)
	}
}
