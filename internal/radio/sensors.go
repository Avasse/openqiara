package radio

import (
	"encoding/binary"

	"github.com/caligone/openqiara/internal/charmux"
)

// Model is a sensor type, from the model string of its pairing beacon.
type Model int

const (
	DWS Model = iota + 1 // door/window
	PIR                  // motion
	KPD                  // keypad
	SRN                  // siren
)

// Event is something a sensor reported.
type Event struct {
	Addr  uint32
	Kind  EventKind
	Value int // Battery: raw level; SirenState: state; DeliveryFailed: MCU reason
}

// EventKind says what an Event is about.
type EventKind int

const (
	Opened EventKind = iota + 1
	Closed
	MotionStart
	MotionEnd
	ArmedAway  // keypad day button
	ArmedNight // keypad night button
	Disarmed   // keypad code typed, the keypad checks it against its PIN
	SirenState // 0 off, 2 arming delay, 3 armed, 4 entry delay, 5 alert
	Battery
	Rebooted       // the sensor lost its bytecode and asked for it
	DeliveryFailed // the MCU gave up on a frame for this sensor
)

// config is the frame body that configures a sensor. DWS, PIR and keypad
// get their system index and the siren's address (fbxhome RE: HlDws config
// FUN_000a8a88 writes the HlSrn node's domus_addr); the siren is asked for
// its state.
func (n *node) config(siren uint32) []byte {
	if n.Model == SRN {
		return []byte{0x55, 0x06}
	}
	return binary.LittleEndian.AppendUint32([]byte{0x55, 0x00, n.SystemIndex}, siren)
}

// eventAck is the bare ack of an application frame: fbxhome acks a DWS
// with different flags than every other sensor.
func (n *node) eventAck() uint16 {
	if n.Model == DWS {
		return flagsDWSAck
	}
	return flagsEventAck
}

// events decodes what a sensor frame reports, whether or not it needs an
// answer. The meaning of each state byte is fbxhome's own reading of it,
// checked against its log in replay_test.go.
func (n *node) events(rx charmux.ManagedFrame) []Event {
	p := rx.Payload
	switch {
	case rx.WFlags == wfStatus && len(p) >= 2:
		// Heartbeat: status bits, battery, then the gateway address.
		ev := []Event{{Addr: n.Addr, Kind: Battery, Value: int(p[1])}}
		if p[0]&statusNeedBytecode != 0 {
			ev = append(ev, Event{Addr: n.Addr, Kind: Rebooted})
		}
		return ev

	case rx.WFlags == wfApp && len(p) >= 8 && p[0] == 0x55 && p[1] == 0x01:
		// State report: 55 01 <timestamp:4> <kind> <state> ...
		if kind, value, ok := n.state(p[6], p[7]); ok {
			return []Event{{Addr: n.Addr, Kind: kind, Value: value}}
		}
	}
	return nil
}

func (n *node) state(kind, state byte) (EventKind, int, bool) {
	switch n.Model {
	case DWS:
		return pick(state, Opened, Closed)
	case PIR:
		return pick(state, MotionStart, MotionEnd)
	case KPD:
		switch {
		case kind&0x80 != 0: // a code follows
			return Disarmed, 0, true
		case state == 1:
			return ArmedAway, 0, true
		case state == 2:
			return ArmedNight, 0, true
		}
	case SRN:
		return SirenState, int(state), true
	}
	return 0, 0, false
}

// pick maps state 0 and 1 to two kinds: DWS and PIR both report 0 while
// active (open, motion) and 1 once back to rest.
func pick(state byte, active, rest EventKind) (EventKind, int, bool) {
	switch state {
	case 0:
		return active, 0, true
	case 1:
		return rest, 0, true
	}
	return 0, 0, false
}

func isKeypadWake(p []byte) bool {
	return len(p) >= 2 && p[0] == 0x55 && p[1] == 0x09
}
