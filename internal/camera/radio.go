package camera

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/domus"
	"github.com/caligone/openqiara/internal/domusvm"
	"github.com/caligone/openqiara/internal/radio"
)

var _ Client = (*RadioClient)(nil)

// RadioClient is the Client of charmux mode: openqiarad is the radio
// gateway itself, fbxhome does not run. One goroutine owns the radio
// engine (internal/radio): it feeds it the frames the MCU delivers and
// sends what the engine answers. Pairing, siren commands and config
// changes hand it work through do.
//
// The config is the registry of paired sensors (config.SensorEntry.Radio).
// A sensor keeps the id it had under fbxhome, its fbxhome node id: Home
// Assistant and Alarmo entities are named after it.
type RadioClient struct {
	mcu   radioMCU
	store *config.Store
	log   *slog.Logger

	// Files on the camera; tests point them elsewhere.
	KeysPath    string // vendor keys, for pairing
	ManifestDir string // update_manifest.json: sensor firmware → bytecode
	BytecodeDir string // <bytecode hash>.bin
	FbxhomeXML  string // glob of fbxhome's state files, imported once

	keys       []domus.VendorKey
	gateway    uint32
	work       chan func(*radio.Engine)
	events     chan SensorEvent
	done       chan struct{} // Close was called
	stopped    chan struct{} // the engine goroutine is gone
	closing    sync.Once
	wg         sync.WaitGroup
	alarmSiren uint32 // the siren sensor configs carry; engine goroutine only

	mu      sync.Mutex
	sensors map[int]Sensor     // live state by id
	nodes   map[int]radio.Node // what the engine serves, by id
	ids     map[uint32]int     // radio address → id
	pairing *pairing
}

// radioMCU is the part of charmux.Client the radio client uses.
type radioMCU interface {
	domus.MCU
	Connect(ctx context.Context) error
	SendPKT(ctx context.Context, data []byte) error
	SendShutter(open bool) error
	Events() chan charmux.Event
	Close() error
}

// pairing is a pairing in progress: the engine goroutine forwards it the
// MCU's CTRL frames.
type pairing struct {
	frames chan []byte
	cancel context.CancelFunc
	done   chan struct{}
	sensor *Sensor // set before done closes
	err    error
}

// pairingWindow is how long a pairing waits for the sensor, like the old
// charmux client.
const pairingWindow = 2 * time.Minute

// NewRadioClient returns a client that serves the sensors of store's
// config through mcu, once connected.
func NewRadioClient(mcu radioMCU, store *config.Store, log *slog.Logger) *RadioClient {
	return &RadioClient{
		mcu: mcu, store: store, log: log,
		KeysPath:    "/etc/hl/vendors.keys",
		ManifestDir: "/etc/hl",
		BytecodeDir: "/lib/firmwares/bytecode",
		FbxhomeXML:  fbxhomeXMLGlob,
		work:        make(chan func(*radio.Engine)),
		events:      make(chan SensorEvent, 64),
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
		sensors:     make(map[int]Sensor),
		nodes:       make(map[int]radio.Node),
		ids:         make(map[uint32]int),
	}
}

// Connect takes the radio over: it fails while fbxhome runs, as fbxhome
// holds the charmux ports.
func (c *RadioClient) Connect(ctx context.Context) error {
	if err := c.mcu.Connect(ctx); err != nil {
		return fmt.Errorf("radio: %w", err)
	}
	info, err := c.gatewayInfo(ctx)
	if err != nil {
		_ = c.mcu.Close()
		return err
	}
	c.gateway = uint32(info.Address)

	if ids, err := ImportFbxhomeRadio(c.store, c.FbxhomeXML); err != nil {
		c.log.Warn("radio: fbxhome.xml import failed", "error", err)
	} else if len(ids) > 0 {
		c.log.Info("radio: sensors imported from fbxhome.xml", "ids", ids)
	}
	for _, se := range c.store.Get().Sensors {
		if se.Radio.Addr == 0 && se.Type != "" {
			c.log.Warn("radio: sensor without a radio address, pair it again", "id", se.ID, "type", se.Type)
		}
	}
	if c.keys, err = domus.LoadVendorKeys(c.KeysPath); err != nil {
		c.log.Warn("radio: no vendor keys, pairing unavailable", "error", err)
	}

	e, err := radio.New(radio.Options{Gateway: c.gateway, Bytecode: c.bytecodeSource()})
	if err != nil {
		_ = c.mcu.Close()
		return err
	}
	nodes, _, siren := c.load()
	for _, n := range nodes {
		c.serve(e, n)
	}
	e.SetAlarmSiren(siren)
	c.alarmSiren = siren
	c.log.Info("radio: gateway up", "addr", info.Address, "netid", info.NetworkID, "sensors", len(nodes))

	c.wg.Add(1)
	go c.run(e)
	return nil
}

// gatewayInfo reads the gateway's radio address, retrying: right after a
// boot, charmux can take a few seconds to wire the UART up. GetNet
// follows, as with fbxhome.
func (c *RadioClient) gatewayInfo(ctx context.Context) (*charmux.MCUInfo, error) {
	var err error
	for range 3 {
		var info *charmux.MCUInfo
		if info, err = c.mcu.GetInfo(ctx); err == nil {
			if _, err := c.mcu.GetNet(ctx); err != nil {
				c.log.Warn("radio: GetNet failed", "error", err)
			}
			return info, nil
		}
	}
	return nil, fmt.Errorf("radio: MCU GetInfo: %w", err)
}

// bytecodeSource returns where the engine gets the bytecode of a sensor
// that lost it, nil without the camera's update manifest.
func (c *RadioClient) bytecodeSource() func(fwHash []byte) ([][]byte, error) {
	m, err := domusvm.LoadManifest(c.ManifestDir + "/update_manifest.json")
	if err != nil {
		c.log.Warn("radio: no update manifest, a sensor that loses its bytecode stays mute", "error", err)
		return nil
	}
	return func(fwHash []byte) ([][]byte, error) { return m.Frames(c.BytecodeDir, fwHash) }
}

var radioModels = map[string]radio.Model{"DWS": radio.DWS, "PIR": radio.PIR, "KPD": radio.KPD, "SRN": radio.SRN}

// load makes the client's view of the paired sensors match the config.
// It returns what the engine must serve, the addresses it must forget
// and the alarm siren: the lowest-id siren, as fbxhome takes the first
// HlSrn it finds.
func (c *RadioClient) load() (nodes []radio.Node, removed []uint32, siren uint32) {
	fresh := make(map[int]radio.Node)
	uids := make(map[int]string)
	types := make(map[int]string)
	for _, se := range c.store.Get().Sensors {
		model, ok := radioModels[se.Type]
		if !ok || se.Radio.Addr == 0 {
			continue
		}
		n := radio.Node{Addr: se.Radio.Addr, Model: model, SystemIndex: se.Radio.SystemIndex}
		if model == radio.KPD && se.KPDCode != "" {
			n.PINs = []string{se.KPDCode}
		}
		fresh[se.ID], uids[se.ID], types[se.ID] = n, se.Radio.UID, se.Type
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for id, n := range c.nodes {
		if f, ok := fresh[id]; !ok || f.Addr != n.Addr {
			removed = append(removed, n.Addr)
		}
		if _, ok := fresh[id]; !ok {
			delete(c.sensors, id)
		}
	}
	c.nodes = fresh
	c.ids = make(map[uint32]int, len(fresh))
	for _, id := range slices.Sorted(maps.Keys(fresh)) {
		n := fresh[id]
		c.ids[n.Addr] = id
		nodes = append(nodes, n)
		if siren == 0 && n.Model == radio.SRN {
			siren = n.Addr
		}
		s, ok := c.sensors[id]
		if !ok || s.ItemID != uids[id] {
			s = Sensor{ID: id, Reachable: true}
		}
		s.Type, s.ItemID = types[id], uids[id]
		c.sensors[id] = s
	}
	return nodes, removed, siren
}

// Reload applies the config's paired sensors and keypad codes to the
// engine: codes go out at the keypad's next wake.
func (c *RadioClient) Reload() error {
	nodes, removed, siren := c.load()
	return c.do(func(e *radio.Engine) {
		for _, addr := range removed {
			e.RemoveNode(addr)
		}
		for _, n := range nodes {
			c.serve(e, n)
		}
		if siren != c.alarmSiren {
			e.SetAlarmSiren(siren)
			c.alarmSiren = siren
		}
	})
}

// serve hands a sensor to the engine. A keypad whose code is not valid
// (the config was edited by hand) is served without it, not left mute.
func (c *RadioClient) serve(e *radio.Engine, n radio.Node) {
	if err := e.SetNode(n); err != nil {
		c.log.Warn("radio: keypad code ignored", "addr", n.Addr, "error", err)
		n.PINs = nil
		_ = e.SetNode(n)
	}
}

// run is the goroutine that owns the engine.
func (c *RadioClient) run(e *radio.Engine) {
	defer c.wg.Done()
	defer close(c.stopped)
	_ = c.send(e, e.Start())
	events := c.mcu.Events()
	for {
		select {
		case <-c.done:
			return
		case fn := <-c.work:
			fn(e)
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Channel {
			case charmux.ChannelPKT:
				c.receive(e, ev.Data)
			case charmux.ChannelCTRL:
				c.ctrl(ev.Data)
			}
		}
	}
}

var errRadioStopped = errors.New("radio: gateway stopped")

// do runs fn on the engine goroutine and waits for it. Never call it from
// that goroutine.
func (c *RadioClient) do(fn func(*radio.Engine)) error {
	finished := make(chan struct{})
	select {
	case c.work <- func(e *radio.Engine) { fn(e); close(finished) }:
		<-finished
		return nil
	case <-c.stopped:
		return errRadioStopped
	}
}

func (c *RadioClient) receive(e *radio.Engine, data []byte) {
	rx, err := charmux.DeserializeManagedFrame(data)
	if err != nil {
		c.log.Warn("radio: unreadable frame", "len", len(data), "error", err)
		c.log.Debug("radio: unreadable frame", "hex", hex.EncodeToString(data))
		return
	}
	c.trace("rx", rx.Src, *rx)
	if rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) != 0 { // not an MCU report
		c.mu.Lock()
		if id, ok := c.ids[rx.Src]; ok {
			s := c.sensors[id]
			s.LastSeen = time.Now().Unix()
			c.sensors[id] = s
		}
		c.mu.Unlock()
	}
	_ = c.send(e, e.Receive(time.Now(), *rx))
}

// send puts the engine's frames on the air and publishes its events. A
// frame that cannot be sent is lost for the engine too.
func (c *RadioClient) send(e *radio.Engine, res radio.Result) error {
	var errs []error
	for _, f := range res.Send {
		c.trace("tx", f.GWDst, f)
		if err := c.mcu.SendPKT(context.Background(), f.Serialize()); err != nil {
			c.log.Warn("radio: send failed", "dst", f.GWDst, "error", err)
			res.Events = append(res.Events, e.Lost(f.Counter).Events...)
			errs = append(errs, err)
		}
	}
	for _, ev := range res.Events {
		c.publish(ev)
	}
	return errors.Join(errs...)
}

// trace logs a frame at debug level. A keypad's payloads carry its codes:
// only their length is logged.
func (c *RadioClient) trace(dir string, peer uint32, f charmux.ManagedFrame) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	payload := hex.EncodeToString(f.Payload)
	c.mu.Lock()
	if c.nodes[c.ids[peer]].Model == radio.KPD {
		payload = fmt.Sprintf("(%d bytes)", len(f.Payload))
	}
	c.mu.Unlock()
	c.log.Debug("radio "+dir, "peer", peer, "cnt", f.Counter, "flags", fmt.Sprintf("%04x", f.Flags),
		"wflags", fmt.Sprintf("%02x", f.WFlags), "payload", payload)
}

// publish turns an engine event into the sensor state openqiarad
// publishes. A keypad button is an action, not a state: it goes out once
// as KPDState, which forwardEvents turns into an alarm command.
func (c *RadioClient) publish(ev radio.Event) {
	c.mu.Lock()
	id, known := c.ids[ev.Addr]
	s := c.sensors[id]
	before := s
	action := ""
	switch ev.Kind {
	case radio.Opened, radio.Closed:
		s.Open = ev.Kind == radio.Opened
	case radio.MotionStart, radio.MotionEnd:
		s.Motion = ev.Kind == radio.MotionStart
	case radio.Battery:
		s.Battery = ev.Value
	case radio.ArmedAway:
		action = "armed_away"
	case radio.ArmedNight:
		action = "armed_night"
	case radio.Disarmed:
		action = "disarmed"
	}
	if known {
		c.sensors[id] = s
	}
	c.mu.Unlock()

	switch {
	case !known:
		c.log.Warn("radio: frame from an address not paired", "addr", ev.Addr)
	case action != "":
		s.KPDState = action
		c.emit(s)
	case s != before:
		c.emit(s)
	case ev.Kind == radio.Tamper:
		c.log.Warn("radio: sensor tampered with", "id", id)
	case ev.Kind == radio.Emergency:
		c.log.Warn("radio: keypad emergency button, not handled", "id", id)
	case ev.Kind == radio.SirenState:
		c.log.Info("radio: siren state", "id", id, "state", ev.Value)
	case ev.Kind == radio.Rebooted:
		c.log.Info("radio: sensor rebooted, provisioning it", "id", id)
	case ev.Kind == radio.NoBytecode:
		c.log.Error("radio: no bytecode for the sensor's firmware, it stays mute", "id", id)
	case ev.Kind == radio.DeliveryFailed:
		c.log.Warn("radio: frame not delivered", "id", id, "counter", ev.Value)
	case ev.Kind == radio.Unhandled:
		c.log.Warn("radio: frame not understood", "id", id, "value", ev.Value)
	}
}

func (c *RadioClient) emit(s Sensor) {
	select {
	case c.events <- SensorEvent{SensorID: s.ID, Sensor: s}:
	default:
		c.log.Warn("radio: event channel full, dropping event", "id", s.ID)
	}
}

// ctrl hands a CTRL frame to the pairing in progress.
func (c *RadioClient) ctrl(data []byte) {
	c.mu.Lock()
	p := c.pairing
	c.mu.Unlock()
	if p == nil || isClosed(p.done) {
		c.log.Info("radio: CTRL frame", "hex", hex.EncodeToString(data))
		return
	}
	select {
	case p.frames <- data:
	default:
		c.log.Warn("radio: CTRL frame dropped, pairing too slow")
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// StartPairing waits for a sensor in pairing mode. Its type comes from
// its beacon, sensorType only has to be one openqiara knows.
func (c *RadioClient) StartPairing(_ context.Context, sensorType, _ string) (int, error) {
	if _, ok := radioModels[sensorType]; !ok {
		return 0, fmt.Errorf("radio: unknown sensor type %q", sensorType)
	}
	if len(c.keys) == 0 {
		return 0, errors.New("radio: no vendor keys, pairing unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pairing != nil && !isClosed(c.pairing.done) {
		return 0, errors.New("radio: a pairing is already running")
	}
	addr, err := c.nextAddr()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairingWindow)
	p := &pairing{frames: make(chan []byte, 16), cancel: cancel, done: make(chan struct{})}
	c.pairing = p
	go func() {
		defer close(p.done)
		defer cancel()
		res, err := domus.Pair(ctx, c.mcu, p.frames, c.keys, addr, c.log)
		if err == nil {
			p.sensor, err = c.adopt(res)
		}
		p.err = err
	}()
	return 1, nil
}

// nextAddr is the address the next pairing gives: above every address
// ever given, as the MCU keeps the sensors it knew, and never the
// gateway's own.
func (c *RadioClient) nextAddr() (byte, error) {
	cfg := c.store.Get()
	next := max(cfg.RadioNextAddr, 2) // 0 broadcast, 1 gateway on production cameras
	for _, se := range cfg.Sensors {
		next = max(next, se.Radio.Addr+1)
	}
	if next == c.gateway {
		next++
	}
	if next > 0xff {
		return 0, errors.New("radio: no radio address left")
	}
	return byte(next), nil
}

// adopt registers a sensor the MCU just paired. A sensor paired again
// keeps its id and its system index, so that Home Assistant and Alarmo
// keep their entities.
func (c *RadioClient) adopt(res *domus.PairingResult) (*Sensor, error) {
	typ := sensorTypeOfModel(res.Model)
	if typ == "" {
		return nil, fmt.Errorf("radio: unknown sensor model %q", res.Model)
	}
	node := config.RadioNode{Addr: uint32(res.Address), UID: hex.EncodeToString(res.DeviceUID[:])}
	var id int
	var full bool
	err := c.store.Update(func(cfg *config.Config) {
		sensors := slices.Clone(cfg.Sensors) // Get() copies share the old array
		i := slices.IndexFunc(sensors, func(se config.SensorEntry) bool { return se.Radio.UID == node.UID })
		if i >= 0 {
			node.SystemIndex = sensors[i].Radio.SystemIndex
		} else {
			if node.SystemIndex, full = freeSystemIndex(sensors); full {
				return
			}
			sensors = append(sensors, config.SensorEntry{ID: nextID(*cfg)})
			i = len(sensors) - 1
		}
		for j := range sensors {
			if j != i && sensors[j].Radio.Addr == node.Addr {
				// The MCU gave that address away: the sensor it belonged
				// to is no longer reachable.
				sensors[j].Radio = config.RadioNode{}
			}
		}
		sensors[i].Type, sensors[i].Radio = typ, node
		id = sensors[i].ID
		cfg.Sensors = sensors
		cfg.RadioNextAddr = max(cfg.RadioNextAddr, node.Addr+1)
	})
	if err != nil {
		return nil, fmt.Errorf("radio: save the paired sensor: %w", err)
	}
	if full {
		return nil, errors.New("radio: 64 sensors paired, no system index left")
	}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	c.log.Info("radio: sensor paired", "id", id, "type", typ, "addr", node.Addr)
	s, err := c.ReadSensor(context.Background(), id, typ, nil)
	return s, err
}

// sensorTypeOfModel reads the type out of a beacon model, e.g.
// HOMELABDWS00ACFD.
func sensorTypeOfModel(model string) string {
	for typ, prefix := range NodeType {
		if strings.HasPrefix(model, prefix) {
			return typ
		}
	}
	return ""
}

// nextID is the id of a new sensor: above every id ever used, deleted
// ones included, so that no Home Assistant entity changes sensor.
func nextID(cfg config.Config) int {
	id := 0
	for _, se := range cfg.Sensors {
		id = max(id, se.ID)
	}
	for _, d := range cfg.DeletedIDs {
		id = max(id, d)
	}
	return id + 1
}

// freeSystemIndex is the lowest system index no paired sensor has, as
// fbxhome allocates them (bitmap 0..63); full when there is none.
func freeSystemIndex(sensors []config.SensorEntry) (index uint8, full bool) {
	for i := range uint8(64) {
		if !slices.ContainsFunc(sensors, func(se config.SensorEntry) bool {
			return se.Radio.Addr != 0 && se.Radio.SystemIndex == i
		}) {
			return i, false
		}
	}
	return 0, true
}

// PollPairing reports the pairing in progress: done with the sensor, or
// its error.
func (c *RadioClient) PollPairing(context.Context, int) (*Sensor, bool, error) {
	c.mu.Lock()
	p := c.pairing
	c.mu.Unlock()
	if p == nil {
		return nil, false, errors.New("radio: no pairing running")
	}
	if !isClosed(p.done) {
		return nil, false, nil
	}
	return p.sensor, p.err == nil, p.err
}

// StopPairing gives up the pairing in progress: the MCU leaves pairing
// mode.
func (c *RadioClient) StopPairing(context.Context, int) error {
	c.mu.Lock()
	p := c.pairing
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	return nil
}

// DeleteSensor stops serving a sensor. The MCU keeps it, having no way to
// forget one: its frames keep coming, unanswered.
func (c *RadioClient) DeleteSensor(_ context.Context, id int) error {
	err := c.store.Update(func(cfg *config.Config) {
		cfg.Sensors = slices.DeleteFunc(slices.Clone(cfg.Sensors), func(se config.SensorEntry) bool { return se.ID == id })
	})
	if err != nil {
		return err
	}
	return c.Reload()
}

// CachedSensors returns the paired sensors' live state, by id.
func (c *RadioClient) CachedSensors() []Sensor {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Sensor, 0, len(c.sensors))
	for _, id := range slices.Sorted(maps.Keys(c.sensors)) {
		out = append(out, c.sensors[id])
	}
	return out
}

// Sensors is CachedSensors: the sensors report, nothing is polled.
func (c *RadioClient) Sensors(context.Context) ([]Sensor, error) {
	return c.CachedSensors(), nil
}

// ReadSensor returns a sensor's live state.
func (c *RadioClient) ReadSensor(_ context.Context, id int, _ string, _ []string) (*Sensor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sensors[id]
	if !ok {
		return nil, fmt.Errorf("radio: no paired sensor %d", id)
	}
	return &s, nil
}

var errFbxhomeOnly = errors.New("radio: fbxhome mode only")

// EndpointsRead is fbxhome's API.
func (c *RadioClient) EndpointsRead(context.Context, int, []string) ([]EndpointValue, error) {
	return nil, errFbxhomeOnly
}

// EndpointsWrite is fbxhome's API.
func (c *RadioClient) EndpointsWrite(context.Context, int, []EndpointWriteEntry) error {
	return errFbxhomeOnly
}

// OpenStream is fbxhome's API.
func (c *RadioClient) OpenStream(context.Context) (StreamInfo, error) {
	return StreamInfo{}, errFbxhomeOnly
}

// SendPKT sends raw bytes to the MCU, outside the engine: debug only.
func (c *RadioClient) SendPKT(ctx context.Context, data []byte) error {
	return c.mcu.SendPKT(ctx, data)
}

// Siren payloads, after the application class byte. The tones and the
// wake frame were found by ear in April 2026; the wail and the stop come
// from HlSrn::on_write (fbxhome 0xab734). fbxhome itself sends no wake
// frame.
var (
	sirenWake = []byte{0x55, 0x0b, 0, 0, 0, 0, 0, 0}
	sirenStop = []byte{0x55, 0x05, 0x00, 0x84}
	sirenTest = []byte{0x55, 0x05, 0x01, 0x0a, 0x28} // power 10
	sirenWail = []byte{0x55, 0x05, 0x01, 0x64, 0x3c} // power 100
)

// SirenTone is a short sound of the siren's bytecode.
type SirenTone byte

const (
	ToneArming SirenTone = 0x02
	ToneDisarm SirenTone = 0x03
)

// TriggerSiren plays the siren's discreet test sound.
func (c *RadioClient) TriggerSiren(ctx context.Context, id int) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	return c.siren(ctx, addr, sirenTest, true, 3*time.Second, true)
}

// TriggerSirenAlarm fires the full-power wail for duration (3 s if
// unset), then stops it, even when ctx ends first.
func (c *RadioClient) TriggerSirenAlarm(ctx context.Context, id int, duration time.Duration) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	if duration <= 0 {
		duration = 3 * time.Second
	}
	return c.siren(ctx, addr, sirenWail, true, duration, true)
}

// StopSiren stops whatever the siren plays.
func (c *RadioClient) StopSiren(_ context.Context, id int) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	return c.command(addr, sirenStop)
}

// SirenBeep plays a short tone. Of the tones 0x00..0x0f only 0x02 and
// 0x03 are audible; the timings and volume bytes change nothing.
func (c *RadioClient) SirenBeep(ctx context.Context, id int, tone SirenTone) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	beep := []byte{0x55, 0x04, 0x1e, 0x1e, 0x96, 0x05, 0x64, byte(tone),
		0, 0, 0, 0, 0, 0, 0, 0x03, 0, 0, 0, 0, 0, 0, 0, 0x03}
	return c.siren(ctx, addr, beep, true, 3400*time.Millisecond, true)
}

// SendSirenDebug sends any siren payload to a paired radio address, for
// the debug API: [wake] → payload → [hold, stop].
func (c *RadioClient) SendSirenDebug(ctx context.Context, addr uint32, payload []byte, wake, stop bool, hold time.Duration) error {
	c.mu.Lock()
	_, ok := c.ids[addr]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("radio: address %d not paired", addr)
	}
	return c.siren(ctx, addr, payload, wake, hold, stop)
}

func (c *RadioClient) sirenAddr(id int) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.nodes[id]
	if !ok || n.Model != radio.SRN {
		return 0, fmt.Errorf("radio: no paired siren %d", id)
	}
	return n.Addr, nil
}

// siren plays a sequence: the wake frame, the command, and after hold the
// stop frame, sent even when ctx ends first so that a wail never outlives
// its caller.
func (c *RadioClient) siren(ctx context.Context, addr uint32, cmd []byte, wake bool, hold time.Duration, stop bool) error {
	if wake {
		if err := c.command(addr, sirenWake); err != nil {
			return err
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	if err := c.command(addr, cmd); err != nil || !stop {
		return err
	}
	err := sleepCtx(ctx, hold)
	return errors.Join(err, c.command(addr, sirenStop))
}

// command sends an application payload to a siren, which listens all the
// time.
func (c *RadioClient) command(addr uint32, payload []byte) error {
	var err error
	if derr := c.do(func(e *radio.Engine) { err = c.send(e, e.Command(addr, payload)) }); derr != nil {
		return derr
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetShutter moves the privacy shutter through the MCU, and sets hlcamd's
// IR-cut mode like fbxhome did for it (PR #41): forced day while closed,
// or the IR-cut relay clicks all night behind the shutter.
func (c *RadioClient) SetShutter(ctx context.Context, open bool) error {
	if err := c.mcu.SendShutter(open); err != nil {
		return fmt.Errorf("radio: shutter: %w", err)
	}
	mode := nightDayModeForceDay
	if open {
		mode = nightDayModeAuto
	}
	settings := fmt.Sprintf(`{"parameters":{"config.sensor.night_day_mode":{"val":%d}}}`, mode)
	if out, err := exec.CommandContext(ctx, "fbxbusctl", "set", "hlcamd", "video_settings", settings).CombinedOutput(); err != nil {
		return fmt.Errorf("radio: hlcamd night mode: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Events returns the sensor state changes and keypad actions.
func (c *RadioClient) Events() <-chan SensorEvent {
	return c.events
}

// Close stops the gateway and cancels a pairing in progress.
func (c *RadioClient) Close() error {
	var err error
	c.closing.Do(func() {
		c.mu.Lock()
		if c.pairing != nil {
			c.pairing.cancel()
		}
		c.mu.Unlock()
		close(c.done)
		c.wg.Wait()
		close(c.events)
		err = c.mcu.Close()
	})
	return err
}
