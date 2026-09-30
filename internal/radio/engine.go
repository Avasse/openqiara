// Package radio is openqiara's side of the Domus radio protocol, the job
// fbxhome did: keep every paired sensor provisioned (bytecode, time,
// config, keypad codes) and turn its frames into events.
//
// The sensors drive the protocol. A sleepy sensor only listens right after
// it transmits, so every frame flagged Z gets exactly one answer, computed
// from that frame and the sensor's state. The MCU encrypts, retries and
// reports delivery failures, so the engine needs no timer.
//
// Engine does no I/O and never reads the clock. One goroutine feeds it
// what happened (Start, Receive, Command, Lost) and sends what it returns,
// which is what lets a day of fbxhome transcript be replayed frame by frame
// (replay_test.go). It knows sensors by radio address only; mapping them to
// stable identities and deciding what an alarm is belong to its caller.
//
// Not supported: sensors behind a repeater (fbxhome routes along a parent
// chain, see charmux routeShift) and the siren reboot fbxhome uses to wake
// a mute siren (read_status(0x08), then a class 5 frame whose payload is
// not understood).
package radio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Values as fbxhome puts them on the wire. The wflags byte is a class in
// its low six bits plus two markers (0x40, 0x80); the meaning of the high
// flag bits (0x40, 0x80, 0x100, 0x400) is unknown, they are copied as is.
const (
	wfApp          byte = 0x01 // application port: events, config, commands, codes
	wfManageAnswer byte = 0x81 // sensor: read_status answer, bytecode ack
	wfStatus       byte = 0x82 // sensor: status heartbeat
	wfTime         byte = 0xc8 // gateway: current time
	wfReadStatus   byte = 0xcc // gateway: read_status request
	wfBytecode     byte = 0xcd // gateway: VM write frame

	flagsManage   uint16 = 0x0547 // answer that carries a payload
	flagsCommand  uint16 = 0x0d43 // gateway-initiated, routed through its destination
	flagsAck      uint16 = 0x0004 // bare ack
	flagsEventAck uint16 = 0x0544 // bare ack of an application frame
	flagsDWSAck   uint16 = 0x0084 // bare ack of a DWS application frame

	readStatusAll      byte = 0x78 // ask for firmware hash, bytecode hash and battery
	statusNeedTime     byte = 0x10
	statusNeedBytecode byte = 0x40

	qiaraEpoch = 1514764800 // 2018-01-01 UTC, origin of the time frame
	sentWindow = 256        // frames remembered for MCU reports
)

// Node is what the engine needs to know about a paired sensor.
type Node struct {
	Addr        uint32 // radio address the MCU assigned at pairing
	Model       Model
	SystemIndex byte     // per-sensor index 0..63 echoed in the config
	PINs        []string // keypad only: codes it accepts, pushed each time it wakes up
}

// Options configure an Engine.
type Options struct {
	Gateway uint32 // gateway radio address, from GetInfo (1 on production cameras)
	Nodes   []Node
	// AlarmSiren is the radio address every sensor config carries, 0 for
	// none. fbxhome sends the siren of its alarm (HlDws config FUN_000a8a88:
	// the HlSrn node's domus_addr, 0 without an HlAlarm); what the sensors
	// do with it is not known.
	AlarmSiren uint32
	// Bytecode returns the VM write frames for a sensor firmware hash, from
	// the update manifest. Called when a sensor has lost its bytecode.
	Bytecode func(fwHash []byte) ([][]byte, error)
}

// Result is what the caller must do next: send these frames in order and
// publish these events.
type Result struct {
	Send   []charmux.ManagedFrame
	Events []Event
}

// Engine is the gateway side of the protocol. It is not safe for
// concurrent use: a single goroutine owns it.
type Engine struct {
	gateway    uint32
	counter    uint32 // one TX counter for all sensors, like fbxhome
	alarmSiren uint32
	nodes      map[uint32]*node
	sent       map[uint32]sentFrame // recent frames by counter, for MCU reports
	bytecode   func(fwHash []byte) ([][]byte, error)
}

// sentFrame is what a report about a lost frame needs to know.
type sentFrame struct {
	addr   uint32
	config bool
}

// node is a Node plus where its provisioning dialogue stands.
type node struct {
	Node
	step       step
	vm         [][]byte // bytecode frames not sent yet
	sentOps    int      // ops in the bytecode frame awaiting its ack
	needTime   bool
	configSent bool  // once per engine lifetime, again after a bytecode push or a lost config
	lastReply  int64 // counter of the dialogue's last frame, -1 before any
}

type step int

const (
	idle          step = iota
	readingStatus      // read_status sent, waiting for its answer
	writingVM          // bytecode frames going out, waiting for their acks
	sendingTime        // time sent, waiting for the sensor's empty answer
)

// New returns an Engine for the given paired sensors.
func New(o Options) (*Engine, error) {
	e := &Engine{gateway: o.Gateway, alarmSiren: o.AlarmSiren, bytecode: o.Bytecode,
		nodes: make(map[uint32]*node), sent: make(map[uint32]sentFrame)}
	for _, n := range o.Nodes {
		if err := e.SetNode(n); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// SetNode adds a paired sensor or updates one. New keypad codes go out at
// its next wake; a new model or system index means a new config at the
// sensor's next status heartbeat.
func (e *Engine) SetNode(n Node) error {
	for _, pin := range n.PINs {
		if err := validPIN(pin); err != nil {
			return fmt.Errorf("radio: sensor %d: %w", n.Addr, err)
		}
	}
	if old := e.nodes[n.Addr]; old != nil {
		if old.Model != n.Model || old.SystemIndex != n.SystemIndex {
			old.configSent = false
		}
		old.Node = n
		return nil
	}
	e.nodes[n.Addr] = &node{Node: n, lastReply: -1}
	return nil
}

// RemoveNode forgets a sensor: its frames are reported as UnknownSensor.
func (e *Engine) RemoveNode(addr uint32) {
	delete(e.nodes, addr)
}

// SetAlarmSiren changes the siren address in the sensors' config; each
// sensor gets its config again at its next status heartbeat.
func (e *Engine) SetAlarmSiren(addr uint32) {
	e.alarmSiren = addr
	for _, n := range e.nodes {
		if n.Model != SRN {
			n.configSent = false
		}
	}
}

// Start is called once, when the engine takes over the radio. A siren
// listens all the time: ask for its state, which is also its config.
func (e *Engine) Start() Result {
	var res Result
	for _, addr := range slices.Sorted(maps.Keys(e.nodes)) {
		if n := e.nodes[addr]; n.Model == SRN {
			n.configSent = true
			res.Send = append(res.Send, e.command(n, n.config(e.alarmSiren), true))
		}
	}
	return res
}

// Command sends an application payload to a sensor that listens all the
// time, the siren: a routed frame, not an answer. A report about it comes
// back as DeliveryFailed with its counter.
func (e *Engine) Command(addr uint32, payload []byte) Result {
	n := e.nodes[addr]
	if n == nil {
		return Result{Events: []Event{{Addr: addr, Kind: UnknownSensor}}}
	}
	return Result{Send: []charmux.ManagedFrame{e.command(n, payload, false)}}
}

// Receive handles one frame from the MCU. now is when it arrived; it only
// ends up in the time frame.
func (e *Engine) Receive(now time.Time, rx charmux.ManagedFrame) Result {
	if rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) == 0 {
		// MCU delivery report. The reason sits in flag bits 6-9: 1 is
		// UNREACHABLE, the only one seen; 0 is PROCESSING in fbxhome's
		// reason table (strings at 0xd977d), not a failure.
		if rx.Flags>>6&0xf == 0 {
			return Result{}
		}
		return e.Lost(rx.AckCnt)
	}
	n := e.nodes[rx.Src]
	if n == nil {
		return Result{Events: []Event{{Addr: rx.Src, Kind: UnknownSensor}}}
	}
	res := Result{Events: n.events(rx)}
	if rx.Flags&charmux.FlagZ != 0 {
		e.answer(n, rx, now, &res)
	}
	return res
}

// Lost handles a frame that never reached its sensor: the MCU reported it,
// or the caller could not send it. Its dialogue is dropped (the sensor
// starts over at its next heartbeat) and a lost config goes out again.
func (e *Engine) Lost(counter uint32) Result {
	s, ok := e.sent[counter]
	if !ok {
		return Result{}
	}
	delete(e.sent, counter)
	n := e.nodes[s.addr]
	if n == nil {
		return Result{}
	}
	if s.config {
		n.configSent = false
	}
	if n.lastReply == int64(counter) {
		n.step, n.vm = idle, nil
	}
	return Result{Events: []Event{{Addr: n.Addr, Kind: DeliveryFailed, Value: int(counter)}}}
}

// answer is the reaction table: the one frame that goes back to a frame
// flagged Z.
func (e *Engine) answer(n *node, rx charmux.ManagedFrame, now time.Time, res *Result) {
	var f charmux.ManagedFrame
	switch {
	case rx.WFlags == wfStatus:
		// A heartbeat: read the sensor's status, whose answer carries its
		// firmware hash and what it needs.
		n.step, n.vm = readingStatus, nil
		f = e.reply(n, rx, flagsManage, wfReadStatus, []byte{readStatusAll})

	case n.step == readingStatus && rx.WFlags == wfManageAnswer &&
		len(rx.Payload) >= 10 && rx.Payload[1] == readStatusAll:
		status := rx.Payload[0]
		n.needTime = status&statusNeedTime != 0
		if status&statusNeedBytecode != 0 {
			vm, err := e.fetchBytecode(rx.Payload[2:10])
			if err == nil {
				// The sensor rebooted and lost its VM, and with it its config.
				n.vm, n.configSent = vm, false
				f = e.nextVM(n, rx)
				break
			}
			res.Events = append(res.Events, Event{Addr: n.Addr, Kind: NoBytecode})
		}
		f = e.finish(n, rx, now)

	case n.step == writingVM && rx.WFlags == wfManageAnswer && len(rx.Payload) == 5:
		if binary.LittleEndian.Uint32(rx.Payload) != 1<<n.sentOps-1 {
			// An op failed: stop there, the sensor will ask again.
			n.step, n.vm = idle, nil
			f = e.reply(n, rx, flagsAck, 0, nil)
		} else if len(n.vm) > 0 {
			f = e.nextVM(n, rx)
		} else {
			f = e.finish(n, rx, now)
		}

	case n.step == sendingTime && rx.Flags&charmux.FlagW == 0:
		f = e.finish(n, rx, now)

	case rx.WFlags == wfApp:
		flags, wflags, payload := n.appAnswer(rx.Payload)
		f = e.reply(n, rx, flags, wflags, payload)

	default:
		res.Events = append(res.Events, Event{Addr: n.Addr, Kind: Unhandled, Value: int(rx.WFlags)})
		f = e.reply(n, rx, flagsAck, 0, nil)
	}
	res.Send = append(res.Send, f)
}

// finish ends the dialogue once the bytecode is in place: the time if the
// sensor asked for it, then the config if it is not sent yet, else an ack.
func (e *Engine) finish(n *node, rx charmux.ManagedFrame, now time.Time) charmux.ManagedFrame {
	if n.needTime {
		n.step, n.needTime = sendingTime, false
		ts := binary.LittleEndian.AppendUint32(nil, uint32(now.Unix()-qiaraEpoch))
		return e.reply(n, rx, flagsManage, wfTime, ts)
	}
	n.step = idle
	if n.configSent {
		return e.reply(n, rx, flagsAck, 0, nil)
	}
	n.configSent = true
	f := e.reply(n, rx, flagsManage, wfApp, n.config(e.alarmSiren))
	e.sent[f.Counter] = sentFrame{addr: n.Addr, config: true}
	return f
}

func (e *Engine) fetchBytecode(fwHash []byte) ([][]byte, error) {
	if e.bytecode == nil {
		return nil, errors.New("no bytecode source")
	}
	vm, err := e.bytecode(fwHash)
	if err == nil && len(vm) == 0 {
		err = fmt.Errorf("no bytecode for firmware %x", fwHash)
	}
	return vm, err
}

func (e *Engine) nextVM(n *node, rx charmux.ManagedFrame) charmux.ManagedFrame {
	payload := n.vm[0]
	n.vm, n.step, n.sentOps = n.vm[1:], writingVM, countOps(payload)
	return e.reply(n, rx, flagsManage, wfBytecode, payload)
}

// countOps walks a VM write frame (fbxhome serializer FUN_000938a0): a 0x01
// marker, then ops that the sensor acknowledges one bit each. 0x87 write:
// addr:4 len:1 data; 0x82 boot: mode:2 entry:4; 0x80 start, 0x88 erase.
func countOps(p []byte) int {
	ops := 0
	for i := 1; i < len(p); ops++ {
		op := p[i]
		i++
		switch op {
		case 0x87:
			if i+4 >= len(p) {
				return ops + 1
			}
			i += 5 + int(p[i+4])
		case 0x82:
			i += 6
		}
	}
	return ops
}

// reply answers rx: it acknowledges it and belongs to the sensor's dialogue.
func (e *Engine) reply(n *node, rx charmux.ManagedFrame, flags uint16, wflags byte, payload []byte) charmux.ManagedFrame {
	f := e.frame(n, flags, wflags, payload)
	f.AckDst, f.AckCnt = rx.Src, rx.Counter
	n.lastReply = int64(f.Counter)
	return f
}

func (e *Engine) command(n *node, payload []byte, config bool) charmux.ManagedFrame {
	f := e.frame(n, flagsCommand, wfApp, payload)
	f.Route = n.Addr
	e.sent[f.Counter] = sentFrame{addr: n.Addr, config: config}
	return f
}

func (e *Engine) frame(n *node, flags uint16, wflags byte, payload []byte) charmux.ManagedFrame {
	f := charmux.ManagedFrame{GWDst: n.Addr, GWSrc: e.gateway, Counter: e.counter, Src: e.gateway,
		Flags: flags, WFlags: wflags, Payload: payload}
	delete(e.sent, e.counter-sentWindow)
	e.sent[e.counter] = sentFrame{addr: n.Addr}
	e.counter++
	return f
}
