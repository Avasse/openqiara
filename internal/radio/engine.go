// Package radio is openqiara's side of the Domus radio protocol, the job
// fbxhome did: keep every paired sensor provisioned (bytecode, time,
// config, keypad PIN) and turn its frames into events.
//
// The sensors drive the protocol. A sleepy sensor only listens right after
// it transmits, so every frame flagged Z gets exactly one answer, computed
// from that frame and the sensor's state: the reaction table checked
// against fbxhome in internal/fbxreplay. The MCU encrypts, retries and
// reports delivery failures, so the engine needs no timer.
//
// Engine does no I/O and never reads the clock. One goroutine feeds it
// what happened (Start, Receive, Command) and sends what it returns, which
// is what lets a day of fbxhome transcript be replayed frame by frame.
// It knows sensors by radio address only; mapping them to stable
// identities and deciding what an alarm is belong to its caller.
package radio

import (
	"encoding/binary"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/domus"
)

// Values as fbxhome puts them on the wire. The wflags byte is a class in
// its low six bits plus two markers (0x40, 0x80); the meaning of the high
// flag bits (0x40, 0x80, 0x100, 0x400) is unknown, they are copied as is.
const (
	wfApp          byte = 0x01 // application port: events, config, commands, PIN
	wfManageAnswer byte = 0x81 // sensor: read_status answer, bytecode ack
	wfStatus       byte = 0x82 // sensor: status heartbeat
	wfTime         byte = 0xc8 // gateway: current time
	wfReadStatus   byte = 0xcc // gateway: read_status request
	wfBytecode     byte = 0xcd // gateway: VM write frame

	flagsManage   uint16 = 0x0547 // answer that carries a payload
	flagsCommand  uint16 = 0x0d43 // gateway-initiated, routed through its destination
	flagsAck      uint16 = 0x0004 // bare ack
	flagsEventAck uint16 = 0x0544 // bare ack of an event, for every sensor but the DWS
	flagsDWSAck   uint16 = 0x0084 // bare ack of a DWS event

	readStatusAll      byte = 0x78 // ask for firmware hash, bytecode hash and battery
	statusNeedTime     byte = 0x10
	statusNeedBytecode byte = 0x40

	qiaraEpoch = 1514764800 // 2018-01-01 UTC, origin of the time frame
)

// Node is what the engine needs to know about a paired sensor.
type Node struct {
	Addr        uint32 // radio address the MCU assigned at pairing
	Model       Model
	SystemIndex byte   // per-sensor index 0..63 echoed in the config
	PIN         string // keypad only: code pushed each time it wakes up
}

// VMFrame is one bytecode frame and the number of VM ops it carries: the
// sensor acknowledges it with one bit per op.
type VMFrame struct {
	Payload []byte
	Ops     int
}

// Options configure an Engine.
type Options struct {
	Gateway uint32 // gateway radio address, from GetInfo (1 on production cameras)
	Nodes   []Node
	// Bytecode returns the VM frames for a sensor firmware hash, from the
	// update manifest. Called when a sensor has lost its bytecode.
	Bytecode func(fwHash []byte) ([]VMFrame, error)
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
	gateway  uint32
	counter  uint32 // one TX counter for all sensors, like fbxhome
	siren    uint32 // radio address of the siren, told to every other sensor
	nodes    map[uint32]*node
	bytecode func(fwHash []byte) ([]VMFrame, error)
}

// node is a Node plus where its provisioning dialogue stands.
type node struct {
	Node
	step       step
	vm         []VMFrame // bytecode frames not sent yet
	sentOps    int       // ops in the bytecode frame awaiting its ack
	needTime   bool
	configSent bool  // once per engine lifetime, again after a bytecode push
	lastSent   int64 // counter of the last frame sent to it, -1 before any
}

type step int

const (
	idle          step = iota
	readingStatus      // read_status sent, waiting for its answer
	writingVM          // bytecode frames going out, waiting for their acks
	sendingTime        // time sent, waiting for the sensor's empty answer
)

// New returns an Engine for the given paired sensors.
func New(o Options) *Engine {
	e := &Engine{gateway: o.Gateway, nodes: make(map[uint32]*node), bytecode: o.Bytecode}
	for _, n := range o.Nodes {
		e.nodes[n.Addr] = &node{Node: n, lastSent: -1}
		if n.Model == SRN {
			e.siren = n.Addr
		}
	}
	return e
}

// Start is called once, when the engine takes over the radio. The siren
// listens all the time: ask for its state, which is also its config.
func (e *Engine) Start() Result {
	var res Result
	for _, n := range e.nodes {
		if n.Model == SRN {
			n.configSent = true
			res.Send = append(res.Send, e.command(n, n.config(e.siren)))
		}
	}
	return res
}

// Command sends an application payload to a sensor that listens all the
// time, the siren: a routed frame, not an answer.
func (e *Engine) Command(addr uint32, payload []byte) Result {
	n := e.nodes[addr]
	if n == nil {
		return Result{}
	}
	return Result{Send: []charmux.ManagedFrame{e.command(n, payload)}}
}

// Receive handles one frame from the MCU. now is when it arrived; it only
// ends up in the time frame.
func (e *Engine) Receive(now time.Time, rx charmux.ManagedFrame) Result {
	if rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) == 0 {
		return e.deliveryFailed(rx)
	}
	n := e.nodes[rx.Src]
	if n == nil {
		return Result{} // not a paired sensor: nothing to answer for it
	}
	res := Result{Events: n.events(rx)}
	if rx.Flags&charmux.FlagZ != 0 {
		res.Send = []charmux.ManagedFrame{e.answer(n, rx, now)}
	}
	return res
}

// answer is the reaction table: the one frame that goes back to a frame
// flagged Z.
func (e *Engine) answer(n *node, rx charmux.ManagedFrame, now time.Time) charmux.ManagedFrame {
	switch {
	case rx.WFlags == wfStatus:
		// A heartbeat: read the sensor's status, whose answer carries its
		// firmware hash and what it needs.
		n.step, n.vm = readingStatus, nil
		return e.reply(n, rx, flagsManage, wfReadStatus, []byte{readStatusAll})

	case n.step == readingStatus && rx.WFlags == wfManageAnswer &&
		len(rx.Payload) >= 10 && rx.Payload[1] == readStatusAll:
		status := rx.Payload[0]
		n.needTime = status&statusNeedTime != 0
		if status&statusNeedBytecode != 0 && e.bytecode != nil {
			// The sensor rebooted and lost its VM, and with it its config.
			if vm, err := e.bytecode(rx.Payload[2:10]); err == nil && len(vm) > 0 {
				n.vm, n.configSent = vm, false
				return e.nextVM(n, rx)
			}
		}
		return e.finish(n, rx, now)

	case n.step == writingVM && rx.WFlags == wfManageAnswer && len(rx.Payload) == 5:
		if binary.LittleEndian.Uint32(rx.Payload) != 1<<n.sentOps-1 {
			// An op failed: stop there, the sensor will ask again.
			n.step, n.vm = idle, nil
			return e.reply(n, rx, flagsAck, 0, nil)
		}
		if len(n.vm) > 0 {
			return e.nextVM(n, rx)
		}
		return e.finish(n, rx, now)

	case n.step == sendingTime && rx.Flags&charmux.FlagW == 0:
		return e.finish(n, rx, now)

	case rx.WFlags == wfApp:
		if n.Model == KPD && isKeypadWake(rx.Payload) && n.PIN != "" {
			return e.reply(n, rx, flagsManage, wfApp, keypadPIN(n.PIN))
		}
		return e.reply(n, rx, n.eventAck(), 0, nil)
	}
	return e.reply(n, rx, flagsAck, 0, nil)
}

// finish ends the dialogue once the bytecode is in place: the time if the
// sensor asked for it, then the config if it is not sent yet, else an ack.
func (e *Engine) finish(n *node, rx charmux.ManagedFrame, now time.Time) charmux.ManagedFrame {
	if n.needTime {
		n.step, n.needTime = sendingTime, false
		ts := make([]byte, 4)
		binary.LittleEndian.PutUint32(ts, uint32(now.Unix()-qiaraEpoch))
		return e.reply(n, rx, flagsManage, wfTime, ts)
	}
	n.step = idle
	if n.configSent {
		return e.reply(n, rx, flagsAck, 0, nil)
	}
	n.configSent = true
	return e.reply(n, rx, flagsManage, wfApp, n.config(e.siren))
}

func (e *Engine) nextVM(n *node, rx charmux.ManagedFrame) charmux.ManagedFrame {
	f := n.vm[0]
	n.vm, n.step, n.sentOps = n.vm[1:], writingVM, f.Ops
	return e.reply(n, rx, flagsManage, wfBytecode, f.Payload)
}

// deliveryFailed handles an MCU report: the frame it names never reached
// its sensor. The dialogue it belonged to is dropped; the sensor retries.
func (e *Engine) deliveryFailed(rx charmux.ManagedFrame) Result {
	for _, n := range e.nodes {
		if n.lastSent == int64(rx.AckCnt) {
			n.step, n.vm = idle, nil
			reason := int(rx.Flags>>6) & 0xf // 1 is UNREACHABLE, the only one seen
			return Result{Events: []Event{{Addr: n.Addr, Kind: DeliveryFailed, Value: reason}}}
		}
	}
	return Result{}
}

// reply answers rx: it acknowledges it and may carry a payload.
func (e *Engine) reply(n *node, rx charmux.ManagedFrame, flags uint16, wflags byte, payload []byte) charmux.ManagedFrame {
	f := e.frame(n, flags, wflags, payload)
	f.AckDst, f.AckCnt = rx.Src, rx.Counter
	return f
}

func (e *Engine) command(n *node, payload []byte) charmux.ManagedFrame {
	f := e.frame(n, flagsCommand, wfApp, payload)
	f.Route = n.Addr
	return f
}

func (e *Engine) frame(n *node, flags uint16, wflags byte, payload []byte) charmux.ManagedFrame {
	f := charmux.ManagedFrame{GWDst: n.Addr, GWSrc: e.gateway, Counter: e.counter, Src: e.gateway,
		Flags: flags, WFlags: wflags, Payload: payload}
	e.counter++
	n.lastSent = int64(f.Counter)
	return f
}

// keypadPIN is the frame body that programs the keypad's code when it
// wakes up: 03 00 <digits> <BCD>.
func keypadPIN(pin string) []byte {
	return append([]byte{0x03, 0x00, byte(len(pin))}, domus.KpdBCDPublic(pin)...)
}
