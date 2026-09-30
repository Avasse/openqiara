package camera

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/config"
)

// fakeMCU stands for charmux: frames in through rx and ctrlRx, frames out
// on pkt.
type fakeMCU struct {
	events chan charmux.Event
	pkt    chan charmux.ManagedFrame

	mu   sync.Mutex
	ctrl [][]byte
}

func newFakeMCU() *fakeMCU {
	return &fakeMCU{events: make(chan charmux.Event, 16), pkt: make(chan charmux.ManagedFrame, 64)}
}

func (m *fakeMCU) Connect(context.Context) error { return nil }
func (m *fakeMCU) GetInfo(context.Context) (*charmux.MCUInfo, error) {
	return &charmux.MCUInfo{Address: 1}, nil
}
func (m *fakeMCU) GetNet(context.Context) (byte, error) { return 5, nil }
func (m *fakeMCU) SendWatchdog()                        {}
func (m *fakeMCU) SendShutter(bool) error               { return nil }
func (m *fakeMCU) Events() chan charmux.Event           { return m.events }
func (m *fakeMCU) Close() error                         { close(m.events); return nil }

func (m *fakeMCU) SendPKT(_ context.Context, b []byte) error {
	f, err := charmux.DeserializeManagedFrame(b)
	if err != nil {
		return err
	}
	m.pkt <- *f
	return nil
}

func (m *fakeMCU) SendRawCTRL(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctrl = append(m.ctrl, append([]byte(nil), b...))
	return nil
}

func (m *fakeMCU) rx(f charmux.ManagedFrame) {
	m.events <- charmux.Event{Channel: charmux.ChannelPKT, Data: f.Serialize()}
}

func (m *fakeMCU) ctrlRx(b ...byte) {
	m.events <- charmux.Event{Channel: charmux.ChannelCTRL, Data: b}
}

// sent is the next frame the client put on the air.
func (m *fakeMCU) sent(t *testing.T) charmux.ManagedFrame {
	t.Helper()
	select {
	case f := <-m.pkt:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame sent")
		return charmux.ManagedFrame{}
	}
}

func (m *fakeMCU) quiet(t *testing.T) {
	t.Helper()
	select {
	case f := <-m.pkt:
		t.Fatalf("unexpected frame %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// A made-up vendor key: the real ones stay on the camera.
var testKey = [32]byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 31: 0xff}

// The production camera's sensors, with made-up UIDs and code.
var (
	kpd = config.SensorEntry{ID: 14, Type: "KPD", KPDCode: "1234", Radio: config.RadioNode{Addr: 2, UID: "0000000000000014", SystemIndex: 0}}
	dws = config.SensorEntry{ID: 23, Type: "DWS", Radio: config.RadioNode{Addr: 5, UID: "0000000000000023", SystemIndex: 2}}
	srn = config.SensorEntry{ID: 29, Type: "SRN", Radio: config.RadioNode{Addr: 6, UID: "0000000000000029", SystemIndex: 3}}
)

func newRadio(t *testing.T, sensors ...config.SensorEntry) (*RadioClient, *fakeMCU, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	if err := store.Update(func(c *config.Config) { c.Sensors = sensors }); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "vendors.keys")
	if err := os.WriteFile(keys, []byte("test: "+base64.StdEncoding.EncodeToString(testKey[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcu := newFakeMCU()
	c := NewRadioClient(mcu, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.KeysPath, c.ManifestDir, c.BytecodeDir = keys, dir, dir
	c.FbxhomeXML = filepath.Join(dir, "fbxhome.xml.*")
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mcu, store
}

func fromSensor(addr, counter uint32, wflags byte, payload ...byte) charmux.ManagedFrame {
	return charmux.ManagedFrame{GWDst: 1, GWSrc: addr, Counter: counter, Src: addr,
		Flags: 0x80 | charmux.FlagZ | charmux.FlagW, WFlags: wflags, Payload: payload}
}

// state is a sensor's state report: 55 01 <ts:4> <kind|signal> <value>.
func state(addr, counter uint32, value byte) charmux.ManagedFrame {
	return fromSensor(addr, counter, 0x01, 0x55, 0x01, 0, 0, 0, 0, 0x01, value)
}

func nextEvent(t *testing.T, c *RadioClient) SensorEvent {
	t.Helper()
	select {
	case ev := <-c.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no sensor event")
		return SensorEvent{}
	}
}

// TestRadioEvents: frames of sensors imported from fbxhome come out under
// their fbxhome ids, and get answered.
func TestRadioEvents(t *testing.T) {
	c, mcu, _ := newRadio(t, kpd, dws, srn)
	if f := mcu.sent(t); f.Route != 6 || !bytes.Equal(f.Payload, []byte{0x55, 0x06}) {
		t.Fatalf("first frame = %+v, want the siren asked for its state", f)
	}

	if _, err := c.ReadSensor(context.Background(), 23, "DWS", nil); err == nil {
		t.Error("door state read before the door reported it")
	}
	mcu.rx(state(5, 10, 0)) // door opened
	if ev := nextEvent(t, c); ev.SensorID != 23 || !ev.Sensor.Open || ev.Sensor.Type != "DWS" {
		t.Errorf("event = %+v, want door 23 open", ev)
	}
	if ack := mcu.sent(t); ack.GWDst != 5 || ack.AckCnt != 10 || ack.Flags != 0x0084 {
		t.Errorf("ack = %+v, want fbxhome's DWS ack", ack)
	}
	if s, err := c.ReadSensor(context.Background(), 23, "DWS", nil); err != nil || !s.Open {
		t.Errorf("read %+v %v, want open", s, err)
	}
	// Opened again (its close was lost): it must go out again.
	mcu.rx(state(5, 11, 0))
	if ev := nextEvent(t, c); ev.SensorID != 23 || !ev.Sensor.Open {
		t.Errorf("event = %+v, want door 23 open again", ev)
	}
	mcu.sent(t)

	mcu.rx(state(2, 12, 1)) // keypad day button
	if ev := nextEvent(t, c); ev.SensorID != 14 || ev.Sensor.KPDState != "armed_away" {
		t.Errorf("event = %+v, want keypad 14 armed_away", ev)
	}
	mcu.sent(t)

	// A battery report must not replay the keypad's last button: main
	// turns any KPDState into an alarm command.
	mcu.rx(fromSensor(2, 13, 0x82, 0x81, 0xff))
	if ev := nextEvent(t, c); ev.Sensor.KPDState != "" || ev.Sensor.Battery != 255 {
		t.Errorf("event = %+v, want a battery update without action", ev)
	}
	if f := mcu.sent(t); f.GWDst != 2 || f.WFlags != 0xcc {
		t.Errorf("answer = %+v, want read_status", f)
	}
}

// TestRadioSiren: the wail carries its duration, in quarter seconds, like
// fbxhome's: the siren stops by itself, no stop frame follows and the
// call does not last the wail.
func TestRadioSiren(t *testing.T) {
	c, mcu, _ := newRadio(t, dws, srn)
	mcu.sent(t) // get-state

	for _, tc := range []struct {
		d    time.Duration
		want byte
	}{{60 * time.Second, 0xf0}, {2 * time.Minute, 0xff}} {
		if err := c.TriggerSirenAlarm(context.Background(), 29, tc.d); err != nil {
			t.Fatal(err)
		}
		for _, want := range [][]byte{sirenWake, {0x55, 0x05, 0x01, 0x64, tc.want}} {
			if f := mcu.sent(t); f.Route != 6 || f.WFlags != 0x01 || !bytes.Equal(f.Payload, want) {
				t.Errorf("frame = %+v, want %x to the siren", f, want)
			}
		}
		mcu.quiet(t)
	}
	if err := c.TriggerSirenAlarm(context.Background(), 23, 0); err == nil {
		t.Error("a door was made to wail")
	}
}

// TestRadioReachability: a frame the siren never got marks it
// unreachable, its next frame reachable again.
func TestRadioReachability(t *testing.T) {
	c, mcu, _ := newRadio(t, srn)
	start := mcu.sent(t)
	// UNREACHABLE report, as the MCU words it (charmux TestMCUDeliveryReport).
	report := []byte{0x01, 0x01, 0x00, 0x2a, 0x01, 0x40, 0x00, 0x01, byte(start.Counter)}
	mcu.events <- charmux.Event{Channel: charmux.ChannelPKT, Data: report}
	if ev := nextEvent(t, c); ev.SensorID != 29 || ev.Sensor.Reachable {
		t.Errorf("event = %+v, want siren 29 unreachable", ev)
	}
	mcu.rx(charmux.ManagedFrame{GWDst: 1, GWSrc: 6, Counter: 40, Src: 6, Flags: 0x0082, WFlags: 0x01, Payload: []byte{0x55, 0x0e}})
	if ev := nextEvent(t, c); ev.SensorID != 29 || !ev.Sensor.Reachable {
		t.Errorf("event = %+v, want siren 29 reachable", ev)
	}
}

// TestRadioSensorsNotServed: a sensor without a radio address, or a keypad
// whose code is not valid, is not served and shows unreachable; the
// keypad is not left to disarm with its off button alone.
func TestRadioSensorsNotServed(t *testing.T) {
	badKPD := kpd
	badKPD.KPDCode = "12a4"
	c, mcu, _ := newRadio(t, badKPD, config.SensorEntry{ID: 40, Type: "PIR"})
	mcu.rx(fromSensor(2, 3, 0x01, 0x55, 0x09))
	mcu.quiet(t)
	for _, s := range c.CachedSensors() {
		if s.Reachable {
			t.Errorf("sensor %d shows reachable", s.ID)
		}
	}
	if len(c.CachedSensors()) != 2 {
		t.Errorf("sensors = %+v, want both listed", c.CachedSensors())
	}
}

// TestRadioPairing: a new sensor gets the next id, address and system
// index, and the engine serves it; paired again, it keeps its id.
func TestRadioPairing(t *testing.T) {
	c, mcu, store := newRadio(t, kpd, dws, srn)
	mcu.sent(t)

	uid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	pair := func(addr byte) *Sensor {
		t.Helper()
		if _, err := c.StartPairing(context.Background(), "PIR", ""); err != nil {
			t.Fatal(err)
		}
		mcu.ctrlRx(append(append(append([]byte{0x17}, testKey[:6]...), uid...), "HOMELABPIR00ACFD"...)...)
		mcu.ctrlRx(append([]byte{0x1f}, uid...)...)
		mcu.ctrlRx(append(append([]byte{0x1e}, uid...), addr, 0, 0, 0)...)
		mcu.ctrlRx(0x16)
		for range 100 {
			s, done, err := c.PollPairing(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if done {
				return s
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("pairing never finished")
		return nil
	}

	s := pair(7)
	if s.ID != 30 || s.Type != "PIR" {
		t.Errorf("paired %+v, want PIR 30", s)
	}
	se := store.Get().Sensors[3]
	if se.ID != 30 || se.Radio.Addr != 7 || se.Radio.SystemIndex != 1 || store.Get().RadioNextAddr != 8 {
		t.Errorf("saved %+v next %d, want addr 7, index 1, next 8", se, store.Get().RadioNextAddr)
	}
	mcu.mu.Lock()
	start := mcu.ctrl[0]
	mcu.mu.Unlock()
	if start[0] != 0x15 || start[1] != 7 {
		t.Errorf("pairing started with %x, want address 7", start)
	}

	mcu.rx(fromSensor(7, 0, 0x82, 0xf1, 0x00, 0x01))
	if f := mcu.sent(t); f.GWDst != 7 || f.WFlags != 0xcc {
		t.Errorf("answer = %+v, want read_status to the new sensor", f)
	}

	if s := pair(8); s.ID != 30 {
		t.Errorf("paired again as %d, want 30", s.ID)
	}
	if se := store.Get().Sensors[3]; se.Radio.Addr != 8 || se.Radio.SystemIndex != 1 || len(store.Get().Sensors) != 4 {
		t.Errorf("saved %+v, want addr 8 and the same index", se)
	}
}

// TestRadioDeleteSensor: a deleted sensor is no longer answered.
func TestRadioDeleteSensor(t *testing.T) {
	c, mcu, store := newRadio(t, dws, srn)
	mcu.sent(t)
	if err := c.DeleteSensor(context.Background(), 23); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(store.Get().Sensors, func(se config.SensorEntry) bool { return se.ID == 23 }) {
		t.Error("sensor 23 still in the config")
	}
	mcu.rx(state(5, 10, 0))
	mcu.quiet(t)
	if len(c.CachedSensors()) != 1 {
		t.Errorf("sensors = %+v, want the siren only", c.CachedSensors())
	}
}

// TestRadioKeypadCode: a new code in the config goes out at the keypad's
// next wake after Reload.
func TestRadioKeypadCode(t *testing.T) {
	c, mcu, store := newRadio(t, kpd)
	if err := store.Update(func(cfg *config.Config) {
		cfg.Sensors = []config.SensorEntry{kpd}
		cfg.Sensors[0].KPDCode = "5678"
	}); err != nil {
		t.Fatal(err)
	}
	c.Reload()
	mcu.rx(fromSensor(2, 3, 0x01, 0x55, 0x09))
	// '5'..'8' are 4..7, two digits a byte, low nibble first.
	if f := mcu.sent(t); !bytes.Equal(f.Payload, []byte{0x03, 0x00, 0x04, 0x54, 0x76}) {
		t.Errorf("code list = %x", f.Payload)
	}
}

const fbxhomeXMLTemplate = `<?xml version="1.0" encoding="utf-8"?>
<domus counter="%COUNTER%" max_id="31">
  <Adapter discarded="false" domus_addr="1" domus_next_addr="%NEXT%" id="12" type="Adapter.DomusAdapter">
  </Adapter>
  <Node alarm_type="0" discarded="false" id="2" type="Node.HlAlarm">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="2" domus_ch_key="secret" domus_item_id="00000000000000aa" domus_key="secret" id="14" system_index="0" type="Node.DomusNode.HLKpd">
    <Code label="admin" password="0000" valid="true" />
  </Node>
  <Node adapter="12" discarded="false" domus_addr="3" domus_item_id="00000000000000bb" id="17" system_index="1" type="Node.DomusNode.HlPir">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="4" domus_item_id="00000000000000cc" id="20" system_index="4" type="Node.DomusNode.HlDws">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="6" domus_item_id="00000000000000dd" id="29" system_index="3" type="Node.DomusNode.HlSrn">
  </Node>
</domus>
`

func writeFbxhomeXML(t *testing.T, path string, counter, next string) {
	t.Helper()
	x := bytes.ReplaceAll([]byte(fbxhomeXMLTemplate), []byte("%COUNTER%"), []byte(counter))
	x = bytes.ReplaceAll(x, []byte("%NEXT%"), []byte(next))
	if err := os.WriteFile(path, x, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestImportFbxhomeRadio: the newest fbxhome state that reads gives the
// sensors their radio identity by id, and the keypad fbxhome's code when
// the config has none; deleted sensors and sensors that have one are
// left alone.
func TestImportFbxhomeRadio(t *testing.T) {
	dir := t.TempDir()
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.3"), "12", "7")
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.0"), "9", "5") // older
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.5"), "13", "8")
	cut, err := os.ReadFile(filepath.Join(dir, "fbxhome.xml.5"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fbxhome.xml.5"), cut[:len(cut)/2], 0o600); err != nil {
		t.Fatal(err) // the newest, cut short: fbxhome stopped while writing it
	}
	store := config.NewStore(filepath.Join(dir, "config.json"))
	paired := config.RadioNode{Addr: 9, UID: "00000000000000ee", SystemIndex: 5}
	if err := store.Update(func(c *config.Config) {
		c.Sensors = []config.SensorEntry{{ID: 14, Type: "KPD"}, {ID: 29, Type: "SRN", Radio: paired}}
		c.DeletedIDs = []int{20}
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := ImportFbxhomeRadio(store, filepath.Join(dir, "fbxhome.xml.*"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int{14, 17}) {
		t.Errorf("imported %v, want 14 and 17", ids)
	}
	cfg := store.Get()
	want := []config.SensorEntry{
		{ID: 14, Type: "KPD", KPDCode: "0000", Radio: config.RadioNode{Addr: 2, UID: "00000000000000aa"}},
		{ID: 29, Type: "SRN", Radio: paired},
		{ID: 17, Type: "PIR", Radio: config.RadioNode{Addr: 3, UID: "00000000000000bb", SystemIndex: 1}},
	}
	if !slices.Equal(cfg.Sensors, want) || cfg.RadioNextAddr != 7 {
		t.Errorf("config = %+v next %d,\nwant %+v next 7", cfg.Sensors, cfg.RadioNextAddr, want)
	}

	if ids, err := ImportFbxhomeRadio(store, filepath.Join(dir, "fbxhome.xml.*")); err != nil || ids != nil {
		t.Errorf("second import = %v %v, want nothing", ids, err)
	}
}
