package radio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/fbxreplay"
)

// The fbxhome transcripts of internal/fbxreplay, with the sensors paired on
// the camera they come from (addresses and system indexes from its
// fbxhome.xml; keypad codes are anonymised to 0000 in the fixtures).
var transcripts = []struct {
	file  string
	boot  bool // the capture starts when fbxhome starts
	opts  Options
	siren uint32
}{
	{"steady_day.log", true, Options{Gateway: 1, AlarmSiren: 6, Nodes: []Node{
		{Addr: 2, Model: KPD, SystemIndex: 0, PINs: []string{"0000"}},
		{Addr: 3, Model: PIR, SystemIndex: 1},
		{Addr: 5, Model: DWS, SystemIndex: 2},
		{Addr: 6, Model: SRN, SystemIndex: 3},
	}}, 6},
	{"alarm_cycle.log", false, Options{Gateway: 1, AlarmSiren: 7, Nodes: []Node{
		{Addr: 3, Model: DWS},
		{Addr: 4, Model: PIR},
		{Addr: 6, Model: KPD, PINs: []string{"0000"}},
		{Addr: 7, Model: SRN},
	}}, 7},
}

// excuse is a kind of fbxhome frame the engine does not reproduce, on
// purpose, and why.
type excuse struct {
	why   string
	match func(f fbxreplay.Frame, siren uint32) bool
}

// notReproduced: any other fbxhome frame the engine misses fails the replay.
var notReproduced = []excuse{
	{"Sigfox info request (HlSrn::send_get_sf_info in the raw log): the Sigfox cloud is dead",
		func(f fbxreplay.Frame, _ uint32) bool {
			return f.Route != 0 && f.WFlags == 0x02 && bytes.Equal(f.Payload, []byte{0x55, 0x0f})
		}},
	{"siren reboot step 1, routed read_status(0x08): not supported",
		func(f fbxreplay.Frame, _ uint32) bool {
			return f.Route != 0 && f.WFlags == wfReadStatus && bytes.Equal(f.Payload, []byte{0x08})
		}},
	{"siren reboot step 2, class 5 frame: not supported",
		func(f fbxreplay.Frame, _ uint32) bool { return f.WFlags == 0x85 }},
	{"siren command decided by fbxhome's own alarm logic: openqiara's alarm sends it through Command",
		func(f fbxreplay.Frame, siren uint32) bool {
			return f.Route == siren && f.WFlags == wfApp && !bytes.Equal(f.Payload, []byte{0x55, 0x06})
		}},
}

func loadTranscript(t *testing.T, name string) []fbxreplay.Frame {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "fbxreplay", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	frames, err := fbxreplay.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func newEngine(t *testing.T, o Options) *Engine {
	t.Helper()
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// transcriptBytecode serves the bytecode fbxhome pushed in the transcript,
// so VM frames compare byte for byte: the fixtures keep the frame
// structure, not the proprietary data.
func transcriptBytecode(frames []fbxreplay.Frame) func([]byte) ([][]byte, error) {
	pushes := make(map[string][][]byte)
	for i, f := range frames {
		p := f.Payload
		if f.Sent || f.WFlags != wfManageAnswer || len(p) < 10 || p[1] != readStatusAll || p[0]&statusNeedBytecode == 0 {
			continue
		}
		var vm [][]byte
		for _, g := range frames[i+1:] {
			if !g.Sent || g.GWDst != f.Src {
				continue
			}
			if g.WFlags != wfBytecode {
				break
			}
			vm = append(vm, g.Payload)
		}
		pushes[string(p[2:10])] = vm
	}
	return func(fw []byte) ([][]byte, error) { return pushes[string(fw)], nil }
}

// sameFrame compares an engine frame with fbxhome's, counter aside: the
// engine does not send what fbxhome sent on its own, so the two counters
// drift apart. A time frame carries the clock: equal within a second.
func sameFrame(got charmux.ManagedFrame, want fbxreplay.Frame) error {
	w := want.ManagedFrame
	if got.GWDst != w.GWDst || got.GWSrc != w.GWSrc || got.Src != w.Src || got.Flags != w.Flags ||
		got.Route != w.Route || got.AckDst != w.AckDst || got.AckCnt != w.AckCnt || got.WFlags != w.WFlags {
		return fmt.Errorf("got %+v, fbxhome line %d sent %+v", got, want.Line, w)
	}
	if w.WFlags == wfTime && len(got.Payload) == 4 && len(w.Payload) == 4 {
		d := int64(binary.LittleEndian.Uint32(got.Payload)) - int64(binary.LittleEndian.Uint32(w.Payload))
		if d < -1 || d > 1 {
			return fmt.Errorf("time %x, fbxhome line %d sent %x", got.Payload, want.Line, w.Payload)
		}
		return nil
	}
	if !bytes.Equal(got.Payload, w.Payload) {
		return fmt.Errorf("payload %x, fbxhome line %d sent %x", got.Payload, want.Line, w.Payload)
	}
	return nil
}

// TestReplayAgainstFbxhome feeds the engine every sensor frame fbxhome
// received, checks it answers exactly what fbxhome answered, and that every
// fbxhome frame it does not reproduce is listed in notReproduced.
func TestReplayAgainstFbxhome(t *testing.T) {
	for _, tr := range transcripts {
		t.Run(tr.file, func(t *testing.T) {
			frames := loadTranscript(t, tr.file)
			opts := tr.opts
			opts.Bytecode = transcriptBytecode(frames)
			e := newEngine(t, opts)
			ours := make(map[int]bool) // fbxhome frames the engine reproduced

			if tr.boot {
				first := slices.IndexFunc(frames, func(f fbxreplay.Frame) bool { return f.Sent })
				start := e.Start().Send
				if len(start) != 1 {
					t.Fatalf("Start sent %d frames, want the siren get-state", len(start))
				}
				if err := sameFrame(start[0], frames[first]); err != nil {
					t.Errorf("Start: %v", err)
				}
				ours[first] = true
			}

			for i, rx := range frames {
				if rx.Sent || rx.Reason != "" {
					continue // MCU reports name fbxhome's counters, not the engine's
				}
				got := e.Receive(rx.Time, rx.ManagedFrame).Send
				if rx.AckOf >= 0 && !ours[rx.AckOf] {
					continue // answers a frame the engine did not send
				}
				var want []int
				for j, f := range frames {
					if f.Sent && f.AckOf == i {
						want = append(want, j)
					}
				}
				if len(got) != len(want) {
					t.Errorf("line %d: engine sent %d frames, fbxhome %d", rx.Line, len(got), len(want))
					continue
				}
				for k, j := range want {
					if err := sameFrame(got[k], frames[j]); err != nil {
						t.Errorf("line %d: %v", rx.Line, err)
						continue
					}
					ours[j] = true
				}
			}

			excused := make(map[string]int)
			for j, f := range frames {
				if !f.Sent || ours[j] {
					continue
				}
				k := slices.IndexFunc(notReproduced, func(x excuse) bool { return x.match(f, tr.siren) })
				if k < 0 {
					t.Errorf("fbxhome line %d not reproduced: %+v", f.Line, f.ManagedFrame)
					continue
				}
				excused[notReproduced[k].why]++
			}
			t.Logf("%d fbxhome frames reproduced; not reproduced on purpose: %v", len(ours), excused)
		})
	}
}

var (
	fbxMotion  = regexp.MustCompile(`mvt (start|end)`)
	fbxDoor    = regexp.MustCompile(`Dws:\s+\d+ (open|close)`)
	fbxKeypad  = regexp.MustCompile(`KPD_(DAY_ALARM|NIGHT_ALARM|ALARM_OFF|EMERGENCY|TAMPER)`)
	fbxSiren   = regexp.MustCompile(`HlSrn state change from \w+ to (\w+)`)
	fbxBattery = regexp.MustCompile(`set_battery_level \(?(\d+)`)

	fromFbxhome = map[string]EventKind{
		"start": MotionStart, "end": MotionEnd, "open": Opened, "close": Closed,
		"DAY_ALARM": ArmedAway, "NIGHT_ALARM": ArmedNight, "ALARM_OFF": Disarmed,
		"EMERGENCY": Emergency, "TAMPER": Tamper,
	}
	sirenStates = map[string]int{
		"OFF": 0, "TIMEOUT_BEFORE_ARMED": 2, "ARMED": 3, "TIMEOUT_BEFORE_ALERT": 4, "ALERT_WITH_SRN": 5,
	}
	stateEvents = []EventKind{Opened, Closed, MotionStart, MotionEnd, ArmedAway, ArmedNight,
		Disarmed, Emergency, Tamper, SirenState, Unhandled}
)

// fbxhomeReading is the state event fbxhome logged about a received frame.
func fbxhomeReading(model Model, rx fbxreplay.Frame) (Event, bool) {
	ev := Event{Addr: rx.Src}
	pattern := map[Model]*regexp.Regexp{PIR: fbxMotion, DWS: fbxDoor, KPD: fbxKeypad, SRN: fbxSiren}[model]
	for _, note := range rx.Notes {
		m := pattern.FindStringSubmatch(note)
		switch {
		case m == nil:
			continue
		case model == SRN:
			state, known := sirenStates[m[1]]
			ev.Kind, ev.Value = SirenState, state
			return ev, known
		default:
			ev.Kind = fromFbxhome[m[1]]
			return ev, true
		}
	}
	return ev, false
}

// TestEventsMatchFbxhomeReading: for every received frame, the state events
// the engine reports are exactly what fbxhome's log says the sensor meant
// (a siren is only checked when fbxhome logs a state change), and battery
// levels are the ones fbxhome set.
func TestEventsMatchFbxhomeReading(t *testing.T) {
	checked := 0
	for _, tr := range transcripts {
		models := make(map[uint32]Model)
		for _, n := range tr.opts.Nodes {
			models[n.Addr] = n.Model
		}
		e := newEngine(t, tr.opts)
		for _, rx := range loadTranscript(t, tr.file) {
			if rx.Sent || rx.Reason != "" {
				continue
			}
			events := e.Receive(rx.Time, rx.ManagedFrame).Events
			var got []Event
			for _, ev := range events {
				if slices.Contains(stateEvents, ev.Kind) {
					got = append(got, ev)
				}
			}
			model := models[rx.Src]
			if want, ok := fbxhomeReading(model, rx); ok {
				checked++
				if len(got) != 1 || got[0] != want {
					t.Errorf("%s line %d (payload %x): engine %+v, fbxhome read %+v", tr.file, rx.Line, rx.Payload, got, want)
				}
			} else if len(got) > 0 && model != SRN {
				t.Errorf("%s line %d (payload %x): engine %+v, fbxhome read nothing", tr.file, rx.Line, rx.Payload, got)
			}

			if rx.WFlags == wfStatus {
				for _, note := range rx.Notes {
					if b := fbxBattery.FindStringSubmatch(note); b != nil {
						level, _ := strconv.Atoi(b[1])
						checked++
						if !slices.Contains(events, Event{Addr: rx.Src, Kind: Battery, Value: level}) {
							t.Errorf("%s line %d: engine %+v, fbxhome battery %d", tr.file, rx.Line, events, level)
						}
						break
					}
				}
			}
		}
	}
	t.Logf("%d events checked against fbxhome's reading", checked)
	if checked == 0 {
		t.Error("nothing checked")
	}
}
