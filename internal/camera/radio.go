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
// gateway itself, fbxhome does not run. It feeds the radio engine
// (internal/radio) the frames the MCU delivers and sends what the engine
// answers. The engine is not safe for concurrent use: the receive loop,
// siren commands and config reloads take turns on mu.
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

	keys    []domus.VendorKey // set by Connect
	gateway uint32            // set by Connect
	events  chan SensorEvent
	done    chan struct{}
	closing sync.Once
	wg      sync.WaitGroup

	mu      sync.Mutex
	engine  *radio.Engine
	closed  bool
	sensors map[int]Sensor     // live state of every sensor in the config, by id
	nodes   map[int]radio.Node // what the engine serves, by id
	ids     map[uint32]int     // radio address → id
	known   map[int]bool       // door or motion state reported since start
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

// pairing is a pairing in progress: the receive loop forwards it the
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

var errRadioStopped = errors.New("radio: gateway stopped")

// NewRadioClient returns a client that serves the sensors of store's
// config through mcu, once connected.
func NewRadioClient(mcu radioMCU, store *config.Store, log *slog.Logger) *RadioClient {
	return &RadioClient{
		mcu: mcu, store: store, log: log,
		KeysPath:    "/etc/hl/vendors.keys",
		ManifestDir: "/etc/hl",
		BytecodeDir: "/lib/firmwares/bytecode",
		FbxhomeXML:  fbxhomeXMLGlob,
		events:      make(chan SensorEvent, 64),
		done:        make(chan struct{}),
		known:       make(map[int]bool),
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
		c.log.Error("radio: fbxhome.xml import failed", "error", err)
	} else if len(ids) > 0 {
		c.log.Info("radio: sensors imported from fbxhome.xml", "ids", ids)
	}
	if c.keys, err = domus.LoadVendorKeys(c.KeysPath); err != nil {
		c.log.Warn("radio: no vendor keys, pairing unavailable", "error", err)
	}
	e, err := radio.New(radio.Options{Gateway: c.gateway, Bytecode: c.bytecodeSource()})
	if err != nil {
		_ = c.mcu.Close()
		return err
	}

	c.mu.Lock()
	c.engine = e
	c.reload()
	for id, s := range c.sensors {
		if _, served := c.nodes[id]; !served {
			c.log.Error("radio: sensor not served, pair it again", "id", id, "type", s.Type)
		}
	}
	c.log.Info("radio: gateway up", "addr", info.Address, "netid", info.NetworkID, "sensors", len(c.nodes))
	_ = c.send(e.Start())
	c.mu.Unlock()

	c.wg.Add(1)
	go c.run()
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

// Reload applies the config's sensors and keypad codes to the engine;
// codes go out at the keypad's next wake.
func (c *RadioClient) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reload()
}

// reload makes the engine serve the sensors of the config, the source of
// truth: pairing, deletion and keypad codes all go through it. A sensor
// with no radio address is not served; neither is a keypad whose code is
// not valid, rather than served without a code (its off button alone
// would disarm). Called with mu held.
func (c *RadioClient) reload() {
	nodes := make(map[int]radio.Node)
	sensors := make(map[int]Sensor)
	for _, se := range c.store.Get().Sensors {
		model, ok := radioModels[se.Type]
		if !ok {
			continue
		}
		s, ok := c.sensors[se.ID]
		if !ok {
			s = Sensor{ID: se.ID, Reachable: true, Battery: se.Battery}
		}
		s.Type, s.ItemID = se.Type, se.Radio.UID
		if se.Radio.Addr == 0 {
			s.Reachable = false
		} else {
			n := radio.Node{Addr: se.Radio.Addr, Model: model, SystemIndex: se.Radio.SystemIndex}
			if model == radio.KPD && se.KPDCode != "" {
				n.PINs = []string{se.KPDCode}
			}
			nodes[se.ID] = n
		}
		sensors[se.ID] = s
	}

	for id, n := range c.nodes {
		if f, ok := nodes[id]; !ok || f.Addr != n.Addr {
			c.engine.RemoveNode(n.Addr)
		}
	}
	c.ids = make(map[uint32]int, len(nodes))
	var siren uint32
	for _, id := range slices.Sorted(maps.Keys(nodes)) {
		n := nodes[id]
		if err := c.engine.SetNode(n); err != nil {
			c.log.Error("radio: sensor not served", "id", id, "error", err)
			c.engine.RemoveNode(n.Addr)
			delete(nodes, id)
			s := sensors[id]
			s.Reachable = false
			sensors[id] = s
			continue
		}
		c.ids[n.Addr] = id
		if siren == 0 && n.Model == radio.SRN {
			siren = n.Addr // the lowest id, as fbxhome takes the first HlSrn it finds
		}
	}
	c.engine.SetAlarmSiren(siren)
	c.nodes, c.sensors = nodes, sensors
}

// run delivers what the MCU sends.
func (c *RadioClient) run() {
	defer c.wg.Done()
	events := c.mcu.Events()
	for {
		select {
		case <-c.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			c.mu.Lock()
			switch ev.Channel {
			case charmux.ChannelPKT:
				c.receive(ev.Data)
			case charmux.ChannelCTRL:
				c.ctrl(ev.Data)
			}
			c.mu.Unlock()
		}
	}
}

// receive hands a frame to the engine. Called with mu held.
func (c *RadioClient) receive(data []byte) {
	rx, err := charmux.DeserializeManagedFrame(data)
	if err != nil {
		c.log.Warn("radio: unreadable frame", "len", len(data), "error", err)
		return
	}
	c.trace("rx", rx.Src, *rx)
	if id, ok := c.ids[rx.Src]; ok && rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) != 0 {
		// A sensor frame, not an MCU report: the sensor is there.
		s := c.sensors[id]
		s.LastSeen = time.Now().Unix()
		if !s.Reachable {
			s.Reachable = true
			c.emit(s)
		}
		c.sensors[id] = s
	}
	_ = c.send(c.engine.Receive(time.Now(), *rx))
}

// send puts the engine's frames on the air and publishes its events. A
// frame that cannot be sent is lost for the engine too. Called with mu
// held.
func (c *RadioClient) send(res radio.Result) error {
	var errs []error
	for _, f := range res.Send {
		c.trace("tx", f.GWDst, f)
		if err := c.mcu.SendPKT(context.Background(), f.Serialize()); err != nil {
			c.log.Warn("radio: send failed", "dst", f.GWDst, "error", err)
			res.Events = append(res.Events, c.engine.Lost(f.Counter).Events...)
			errs = append(errs, err)
		}
	}
	for _, ev := range res.Events {
		c.publish(ev)
	}
	return errors.Join(errs...)
}

// trace logs a frame at debug level. Keypad payloads carry its codes: of
// a keypad, or of an address not paired, only the length is logged.
func (c *RadioClient) trace(dir string, peer uint32, f charmux.ManagedFrame) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	payload := fmt.Sprintf("(%d bytes)", len(f.Payload))
	if id, ok := c.ids[peer]; ok && c.nodes[id].Model != radio.KPD {
		payload = hex.EncodeToString(f.Payload)
	}
	c.log.Debug("radio "+dir, "peer", peer, "cnt", f.Counter, "flags", fmt.Sprintf("%04x", f.Flags),
		"wflags", fmt.Sprintf("%02x", f.WFlags), "payload", payload)
}

var keypadActions = map[radio.EventKind]string{
	radio.ArmedAway: "armed_away", radio.ArmedNight: "armed_night", radio.Disarmed: "disarmed",
}

// publish turns an engine event into the sensor state openqiarad
// publishes. Every door and motion report goes out, repeated or not: a
// lost close must not hide the next open. A keypad button is an action,
// not a state: it goes out once as KPDState, which forwardEvents turns
// into an alarm command. Called with mu held.
func (c *RadioClient) publish(ev radio.Event) {
	id, ok := c.ids[ev.Addr]
	if !ok {
		c.log.Debug("radio: frame from an address not paired", "addr", ev.Addr)
		return
	}
	s := c.sensors[id]
	before := s
	report := false
	switch ev.Kind {
	case radio.Opened, radio.Closed:
		s.Open, report = ev.Kind == radio.Opened, true
	case radio.MotionStart, radio.MotionEnd:
		s.Motion, report = ev.Kind == radio.MotionStart, true
	case radio.ArmedAway, radio.ArmedNight, radio.Disarmed:
		action := s
		action.KPDState = keypadActions[ev.Kind]
		c.emit(action)
		return
	case radio.Battery:
		if ev.Value != s.Battery {
			c.saveBattery(id, ev.Value)
		}
		s.Battery = ev.Value
	case radio.DeliveryFailed:
		s.Reachable = false
		c.log.Warn("radio: frame not delivered", "id", id, "counter", ev.Value)
	case radio.NoBytecode:
		s.Reachable = false
		c.log.Error("radio: no bytecode for the sensor's firmware, it stays mute", "id", id)
	case radio.Tamper:
		c.log.Warn("radio: sensor tampered with", "id", id)
	case radio.Emergency:
		c.log.Warn("radio: keypad emergency button, not handled", "id", id)
	case radio.SirenState:
		c.log.Info("radio: siren state", "id", id, "state", ev.Value)
	case radio.Rebooted:
		c.log.Info("radio: sensor rebooted, provisioning it", "id", id)
	case radio.Unhandled:
		c.log.Warn("radio: frame not understood", "id", id, "value", ev.Value)
	}
	if report {
		c.known[id] = true
	}
	c.sensors[id] = s
	if report || s != before {
		c.emit(s)
	}
}

// saveBattery keeps a sensor's battery level for the next start, which
// would otherwise publish 0 until the sensor's next heartbeat, hours
// later. Called with mu held.
func (c *RadioClient) saveBattery(id, level int) {
	err := c.store.Update(func(cfg *config.Config) {
		if i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == id }); i >= 0 {
			cfg.Sensors[i].Battery = level
		}
	})
	if err != nil {
		c.log.Warn("radio: battery level not saved", "id", id, "error", err)
	}
}

// emit publishes a sensor's state. Called with mu held.
func (c *RadioClient) emit(s Sensor) {
	if c.closed {
		return
	}
	select {
	case c.events <- SensorEvent{SensorID: s.ID, Sensor: s}:
	default:
		c.log.Warn("radio: event channel full, dropping event", "id", s.ID)
	}
}

// ctrl hands a CTRL frame to the pairing in progress. Called with mu
// held.
func (c *RadioClient) ctrl(data []byte) {
	if p := c.pairing; p != nil && !isClosed(p.done) {
		select {
		case p.frames <- data:
		default:
			c.log.Warn("radio: CTRL frame dropped, pairing too slow")
		}
		return
	}
	if len(data) > 0 {
		c.log.Info("radio: CTRL frame", "op", fmt.Sprintf("%02x", data[0]), "len", len(data))
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

// StartPairing waits for a sensor of sensorType in pairing mode.
func (c *RadioClient) StartPairing(_ context.Context, sensorType, _ string) (int, error) {
	model, ok := NodeType[sensorType]
	if !ok {
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
	addr, err := c.reserveAddr()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairingWindow)
	p := &pairing{frames: make(chan []byte, 16), cancel: cancel, done: make(chan struct{})}
	c.pairing = p
	go func() {
		defer close(p.done)
		defer cancel()
		res, err := domus.Pair(ctx, c.mcu, p.frames, c.keys, addr, model, c.log)
		if err == nil {
			p.sensor, err = c.adopt(res)
		}
		p.err = err
	}()
	return 1, nil
}

// reserveAddr takes the address the next pairing gives: above every
// address ever given, as the MCU keeps the sensors it knew, and never the
// gateway's own. It is saved before the MCU hears of it: a pairing that
// fails half way may have used it. Called with mu held.
func (c *RadioClient) reserveAddr() (byte, error) {
	var addr uint32
	err := c.store.Update(func(cfg *config.Config) {
		addr = max(cfg.RadioNextAddr, 2) // 0 broadcast, 1 gateway on production cameras
		for _, se := range cfg.Sensors {
			addr = max(addr, se.Radio.Addr+1)
		}
		if addr == c.gateway {
			addr++
		}
		cfg.RadioNextAddr = addr + 1
	})
	if err != nil {
		return 0, fmt.Errorf("radio: reserve an address: %w", err)
	}
	if addr > 0xff {
		return 0, errors.New("radio: no radio address left")
	}
	return byte(addr), nil
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
		i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return strings.EqualFold(se.Radio.UID, node.UID) })
		if i >= 0 {
			node.SystemIndex = cfg.Sensors[i].Radio.SystemIndex
		} else if node.SystemIndex, full = freeSystemIndex(cfg.Sensors); full {
			return
		} else {
			cfg.Sensors = append(cfg.Sensors, config.SensorEntry{ID: nextID(*cfg)})
			i = len(cfg.Sensors) - 1
		}
		for j := range cfg.Sensors {
			if j != i && cfg.Sensors[j].Radio.Addr == node.Addr {
				// The MCU gave that address away: the sensor it belonged
				// to is no longer reachable.
				cfg.Sensors[j].Radio = config.RadioNode{}
			}
		}
		cfg.Sensors[i].Type, cfg.Sensors[i].Radio = typ, node
		id = cfg.Sensors[i].ID
	})
	if err != nil {
		return nil, fmt.Errorf("radio: save the paired sensor: %w", err)
	}
	if full {
		return nil, errors.New("radio: 64 sensors paired, no system index left")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reload()
	c.log.Info("radio: sensor paired", "id", id, "type", typ, "addr", node.Addr)
	s := c.sensors[id]
	return &s, nil
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
		cfg.Sensors = slices.DeleteFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == id })
	})
	if err != nil {
		return err
	}
	c.Reload()
	return nil
}

// CachedSensors returns the sensors' live state, by id.
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

// ReadSensor returns a sensor's live state. A door or motion sensor that
// has not reported since the start has none: publishing the default
// would overwrite the state Home Assistant kept.
func (c *RadioClient) ReadSensor(_ context.Context, id int, _ string, _ []string) (*Sensor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sensors[id]
	if !ok {
		return nil, fmt.Errorf("radio: no sensor %d", id)
	}
	if (s.Type == "DWS" || s.Type == "PIR") && !c.known[id] {
		return nil, fmt.Errorf("radio: sensor %d has not reported its state yet", id)
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

// Siren payloads, after the application class byte. 55 05 01 <power>
// <duration> plays a sound the siren stops by itself, the duration in
// quarter seconds (63 s at most), as fbxhome sends it (HlSrn::on_write
// 0xab734, FbxhomeClient.TriggerSirenAlarm); 55 05 00 84 stops it. The
// wake frame was found by ear in April 2026: fbxhome does not send it.
var (
	sirenWake = []byte{0x55, 0x0b, 0, 0, 0, 0, 0, 0}
	sirenStop = []byte{0x55, 0x05, 0x00, 0x84}
)

func sirenSound(power byte, d time.Duration) []byte {
	return []byte{0x55, 0x05, 0x01, power, byte(min(d/(time.Second/4), 0xff))}
}

// TriggerSiren plays the discreet test sound fbxhome plays: power 10 for
// 10 s.
func (c *RadioClient) TriggerSiren(ctx context.Context, id int) error {
	return c.play(ctx, id, sirenSound(10, 10*time.Second))
}

// TriggerSirenAlarm starts the full-power wail, for duration (10 s if
// unset, like fbxhome mode). It returns at once: the siren stops by
// itself.
func (c *RadioClient) TriggerSirenAlarm(ctx context.Context, id int, duration time.Duration) error {
	if duration <= 0 {
		duration = 10 * time.Second
	}
	return c.play(ctx, id, sirenSound(100, duration))
}

// StopSiren stops whatever the siren plays.
func (c *RadioClient) StopSiren(_ context.Context, id int) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	return c.command(addr, sirenStop)
}

func (c *RadioClient) play(ctx context.Context, id int, sound []byte) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	return c.sequence(ctx, addr, sound, true, false, 0)
}

// SendSirenDebug sends any payload to a paired radio address, for the
// debug API: [wake] → payload → [hold, stop].
func (c *RadioClient) SendSirenDebug(ctx context.Context, addr uint32, payload []byte, wake, stop bool, hold time.Duration) error {
	c.mu.Lock()
	_, ok := c.ids[addr]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("radio: address %d not paired", addr)
	}
	return c.sequence(ctx, addr, payload, wake, stop, hold)
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

// sequence sends the wake frame, the command, and after hold the stop
// frame, sent even when ctx ends first.
func (c *RadioClient) sequence(ctx context.Context, addr uint32, cmd []byte, wake, stop bool, hold time.Duration) error {
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
	return errors.Join(sleepCtx(ctx, hold), c.command(addr, sirenStop))
}

// command sends an application payload to a siren, which listens all the
// time.
func (c *RadioClient) command(addr uint32, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errRadioStopped
	}
	return c.send(c.engine.Command(addr, payload))
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
		c.closed = true
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
