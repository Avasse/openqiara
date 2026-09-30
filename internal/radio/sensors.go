package radio

import (
	"encoding/binary"
	"errors"

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

// Event is something a sensor reported, or went wrong with it.
type Event struct {
	Addr  uint32
	Kind  EventKind
	Value int    // Battery: raw level; SirenState: state; DeliveryFailed: counter of the lost frame; Unhandled: wflags or state value
	Code  string // Disarmed: code typed on the keypad, "" for its off button
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
	Disarmed   // keypad off button, or a code the keypad accepted
	Emergency  // keypad panic
	Tamper
	SirenState // 0 off, 2 arming delay, 3 armed, 4 entry delay, 5 alert
	Battery    // raw level: 255 on every PIR, DWS and keypad seen, 73 on a siren
	Rebooted   // the sensor lost its bytecode and asked for it
	NoBytecode // no bytecode for the firmware of a sensor that needs one

	DeliveryFailed // a frame for this sensor never arrived
	Unhandled      // a frame or state value outside what is understood
	UnknownSensor  // a frame from a radio address that is not paired
)

// stateKinds is what each sensor type means by the value byte of a state
// report: the jump tables of fbxhome's handlers (r2 0x98d38 DWS, 0x9ef14
// PIR, 0xa49b0 keypad, the keypad's also in docs/protocol.md §11). A siren
// reports its state as is.
var stateKinds = map[Model][]EventKind{
	DWS: {Opened, Closed, Tamper},
	PIR: {MotionStart, MotionEnd, Tamper},
	KPD: {Disarmed, ArmedAway, ArmedNight, Emergency, Tamper},
}

// events decodes what a sensor frame reports, whether or not it needs an
// answer.
func (n *node) events(rx charmux.ManagedFrame) []Event {
	p := rx.Payload
	switch {
	case rx.WFlags == wfStatus && len(p) >= 2:
		// Heartbeat: status bits, battery, then the parent address (0x20).
		if p[0]&statusNeedBytecode != 0 {
			// Just rebooted: the battery byte is no measure yet (0).
			return []Event{{Addr: n.Addr, Kind: Rebooted}}
		}
		return []Event{{Addr: n.Addr, Kind: Battery, Value: int(p[1])}}

	case rx.WFlags == wfApp && len(p) >= 8 && p[0] == 0x55 && p[1] == 0x01:
		// 55 01 <timestamp:4> <kind:2|signal:6> <value> [<code:2> 00 00]
		// (fbxhome FUN_000b8bf8). Only the value carries meaning.
		return []Event{n.state(p[7], p[8:])}
	}
	return nil
}

func (n *node) state(value byte, rest []byte) Event {
	ev := Event{Addr: n.Addr}
	kinds := stateKinds[n.Model]
	switch {
	case n.Model == SRN:
		ev.Kind, ev.Value = SirenState, int(value)
	case int(value) < len(kinds):
		ev.Kind = kinds[value]
		if ev.Kind == Disarmed && len(rest) == 4 { // fbxhome reads a code only then
			ev.Code = keypadCode(rest[0], rest[1])
		}
	default:
		ev.Kind, ev.Value = Unhandled, int(value)
	}
	return ev
}

// config is the frame body that configures a sensor. DWS, PIR and keypad
// get their system index and the alarm's siren address; the siren is asked
// for its state.
func (n *node) config(alarmSiren uint32) []byte {
	if n.Model == SRN {
		return []byte{0x55, 0x06}
	}
	return binary.LittleEndian.AppendUint32([]byte{0x55, 0x00, n.SystemIndex}, alarmSiren)
}

// appAnswer is the answer to an application frame: the keypad gets its
// code list each time it wakes up, empty list included (fbxhome encoder
// FUN_000b3eb4, called on every wake); anything else gets a bare ack.
func (n *node) appAnswer(p []byte) (flags uint16, wflags byte, payload []byte) {
	if n.Model == KPD && len(p) >= 2 && p[0] == 0x55 && p[1] == 0x09 {
		return flagsManage, wfApp, keypadCodes(n.PINs)
	}
	// fbxhome acks a DWS with 0x0084 (its own flags, A instead of Z and W)
	// and every other sensor with 0x0544: two code paths in fbxhome, not a
	// known need of the DWS. Kept as is until a DWS is tried with 0x0544.
	if n.Model == DWS {
		return flagsDWSAck, 0, nil
	}
	return flagsEventAck, 0, nil
}

// keypadCodes is the code list pushed to the keypad: a u16 LE byte count,
// then for each code its length and its digits, two per byte, low nibble
// first.
func keypadCodes(pins []string) []byte {
	var body []byte
	for _, pin := range pins {
		body = append(body, byte(len(pin)))
		for i := 0; i < len(pin); i++ {
			d := keypadDigit(pin[i])
			if i%2 == 0 {
				body = append(body, d)
			} else {
				body[len(body)-1] |= d << 4
			}
		}
	}
	return append(binary.LittleEndian.AppendUint16(nil, uint16(len(body))), body...)
}

// keypadDigit is the keypad's digit encoding: '1'..'9' are 0..8, '0' is 10.
func keypadDigit(c byte) byte {
	if c == '0' {
		return 0x0a
	}
	return c - '1'
}

// keypadCode decodes the 4 digits a keypad sends with a code disarm.
func keypadCode(lo, hi byte) string {
	code := make([]byte, 0, 4)
	for _, nibble := range []byte{lo & 0xf, lo >> 4, hi & 0xf, hi >> 4} {
		if nibble == 0x0a {
			code = append(code, '0')
		} else {
			code = append(code, '1'+nibble)
		}
	}
	return string(code)
}

func validPIN(pin string) error {
	if pin == "" {
		return errors.New("empty keypad code")
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return errors.New("keypad code must be digits only")
		}
	}
	return nil
}
