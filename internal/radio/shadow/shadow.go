// Package shadow runs the radio engine in the shadow of fbxhome: fed with
// every frame fbxhome received, as its debug log recorded them, the engine
// must send what fbxhome sent and report what fbxhome understood. The
// tests do it on the anonymised fixtures of internal/fbxreplay,
// cmd/radio-shadow on logs pulled from a camera. Nothing goes on the air.
package shadow

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/fbxreplay"
	"github.com/caligone/openqiara/internal/radio"
)

// Wire values, as in internal/radio.
const (
	wfApp          byte = 0x01
	wfSigfox       byte = 0x02 // class 2, Sigfox info: 55 0f
	wfManageAnswer byte = 0x81
	wfClass5       byte = 0x85
	wfTime         byte = 0xc8
	wfReadStatus   byte = 0xcc
	wfBytecode     byte = 0xcd

	readStatusAll      byte = 0x78
	statusNeedBytecode byte = 0x40
)

// Report is how the engine compared with fbxhome.
type Report struct {
	Runs          int            // fbxhome runs replayed
	Reproduced    int            // fbxhome frames the engine sent identically
	Excused       map[string]int // fbxhome frames not reproduced on purpose, by reason
	EventsChecked int            // engine events compared with fbxhome's reading
	Divergences   []string       // every difference, "line N: ..."; none means alike
}

func (r *Report) diverge(line int, format string, args ...any) {
	r.Divergences = append(r.Divergences, fmt.Sprintf("line %d: %s", line, fmt.Sprintf(format, args...)))
}

// Compare replays frames through the engine, a fresh engine per fbxhome
// run. o describes the camera's sensors; its Bytecode is replaced by the
// frames fbxhome pushed in the transcript.
func Compare(frames []fbxreplay.Frame, o radio.Options) (Report, error) {
	rep := Report{Excused: make(map[string]int)}
	for start := 0; start < len(frames); {
		end := start + 1
		for end < len(frames) && frames[end].Run == frames[start].Run {
			end++
		}
		if err := replayRun(frames, start, end, o, &rep); err != nil {
			return rep, err
		}
		rep.Runs++
		start = end
	}
	return rep, nil
}

func replayRun(frames []fbxreplay.Frame, start, end int, o radio.Options, rep *Report) error {
	siren := uint32(0)
	models := make(map[uint32]radio.Model)
	for _, n := range o.Nodes {
		models[n.Addr] = n.Model
		if n.Model == radio.SRN {
			siren = n.Addr
		}
	}
	o.Bytecode = transcriptBytecode(frames[start:end])
	e, err := radio.New(o)
	if err != nil {
		return err
	}
	ours := make(map[int]bool) // fbxhome frames the engine reproduced

	if f := frames[start]; f.Sent && f.Route == siren && bytes.Equal(f.Payload, []byte{0x55, 0x06}) {
		// The run starts with fbxhome asking the siren for its state:
		// the engine starts too.
		sent := e.Start().Send
		if len(sent) == 1 && sameFrame(sent[0], f, false) == nil {
			ours[start] = true
			rep.Reproduced++
		} else {
			rep.diverge(f.Line, "Start sent %d frames, fbxhome %s", len(sent), describe(f.ManagedFrame))
		}
	}

	for i := start; i < end; i++ {
		rx := frames[i]
		if rx.Sent || rx.Reason != "" {
			continue // MCU reports name fbxhome's counters, not the engine's
		}
		res := e.Receive(rx.Time, rx.ManagedFrame)
		checkEvents(rx, models[rx.Src], res.Events, rep)
		if rx.AckOf >= 0 && !ours[rx.AckOf] {
			continue // answers a frame the engine did not send
		}
		var want []int
		for j := i + 1; j < end; j++ {
			if frames[j].Sent && frames[j].AckOf == i {
				want = append(want, j)
			}
		}
		if len(res.Send) != len(want) {
			rep.diverge(rx.Line, "engine sent %d frames, fbxhome %d, to %s", len(res.Send), len(want), describe(rx.ManagedFrame))
			continue
		}
		for k, j := range want {
			if err := sameFrame(res.Send[k], frames[j], !rx.Time.IsZero()); err != nil {
				rep.diverge(rx.Line, "%v", err)
				continue
			}
			ours[j] = true
			rep.Reproduced++
		}
	}

	for j := start; j < end; j++ {
		f := frames[j]
		if !f.Sent || ours[j] {
			continue
		}
		if why := excuse(f, siren); why != "" {
			rep.Excused[why]++
		} else {
			rep.diverge(f.Line, "fbxhome sent %s, the engine would not", describe(f.ManagedFrame))
		}
	}
	return nil
}

// excuse says why the engine does not reproduce an fbxhome frame on
// purpose, or "" when it should have.
func excuse(f fbxreplay.Frame, siren uint32) string {
	switch {
	case f.Route != 0 && f.WFlags == wfSigfox && bytes.Equal(f.Payload, []byte{0x55, 0x0f}):
		return "Sigfox info request (HlSrn::send_get_sf_info): the Sigfox cloud is dead"
	case f.Route != 0 && f.WFlags == wfReadStatus && bytes.Equal(f.Payload, []byte{0x08}):
		return "siren reboot step 1, routed read_status(0x08): not supported"
	case f.WFlags == wfClass5:
		return "siren reboot step 2, class 5 frame: not supported"
	case f.Route == siren && siren != 0 && f.WFlags == wfApp && !bytes.Equal(f.Payload, []byte{0x55, 0x06}):
		return "siren command from fbxhome's own alarm logic: openqiara's alarm sends it through Command"
	}
	return ""
}

// transcriptBytecode serves the bytecode fbxhome pushed in the transcript:
// the frames that follow a read_status answer asking for bytecode.
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
// drift apart. A time frame carries the clock (equal within a second when
// the log is timestamped); keypad code lists compare by structure only.
func sameFrame(got charmux.ManagedFrame, want fbxreplay.Frame, clock bool) error {
	w := want.ManagedFrame
	if got.GWDst != w.GWDst || got.GWSrc != w.GWSrc || got.Src != w.Src || got.Flags != w.Flags ||
		got.Route != w.Route || got.AckDst != w.AckDst || got.AckCnt != w.AckCnt || got.WFlags != w.WFlags {
		return fmt.Errorf("engine %s, fbxhome line %d %s", describe(got), want.Line, describe(w))
	}
	switch {
	case w.WFlags == wfTime && len(got.Payload) == 4 && len(w.Payload) == 4:
		d := int64(binary.LittleEndian.Uint32(got.Payload)) - int64(binary.LittleEndian.Uint32(w.Payload))
		if clock && (d < -1 || d > 1) {
			return fmt.Errorf("time %d s off fbxhome line %d", d, want.Line)
		}
	case isCodeList(w):
		if !sameCodeLists(got.Payload, w.Payload) {
			return fmt.Errorf("engine %s, fbxhome line %d %s", describe(got), want.Line, describe(w))
		}
	case !bytes.Equal(got.Payload, w.Payload):
		return fmt.Errorf("engine %s, fbxhome line %d %s", describe(got), want.Line, describe(w))
	}
	return nil
}

// isCodeList: the code list the gateway pushes to a keypad.
func isCodeList(f charmux.ManagedFrame) bool {
	return f.WFlags == wfApp && len(f.Payload) >= 2 && f.Payload[0] != 0x55
}

// sameCodeLists compares two code lists without looking at the digits:
// byte count, then the length of each code.
func sameCodeLists(a, b []byte) bool {
	if len(a) != len(b) || !bytes.Equal(a[:2], b[:2]) {
		return false
	}
	for i := 2; i < len(a); i += 1 + (int(a[i])+1)/2 {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// describe prints a frame for a report, keypad codes masked.
func describe(f charmux.ManagedFrame) string {
	payload := hex.EncodeToString(f.Payload)
	switch p := f.Payload; {
	case isCodeList(f):
		payload = fmt.Sprintf("<%d-byte code list>", len(p))
	case len(p) >= 8 && p[0] == 0x55 && p[1] == 0x01 && p[6]&0x80 != 0:
		payload = "<code entry>"
	}
	return fmt.Sprintf("{dst %d src %d flags %#04x route %d ack %d/%d wflags %#02x payload %s}",
		f.GWDst, f.GWSrc, f.Flags, f.Route, f.AckDst, f.AckCnt, f.WFlags, payload)
}

var (
	fbxReading = map[radio.Model]*regexp.Regexp{
		radio.PIR: regexp.MustCompile(`mvt (start|end)`),
		radio.DWS: regexp.MustCompile(`Dws:\s+\d+ (open|close)`),
		radio.KPD: regexp.MustCompile(`KPD_(DAY_ALARM|NIGHT_ALARM|ALARM_OFF|EMERGENCY|TAMPER)`),
		radio.SRN: regexp.MustCompile(`HlSrn state change from \w+ to (\w+)`),
	}
	fbxBattery  = regexp.MustCompile(`set_battery_level \(?(\d+)`)
	fromFbxhome = map[string]radio.EventKind{
		"start": radio.MotionStart, "end": radio.MotionEnd, "open": radio.Opened, "close": radio.Closed,
		"DAY_ALARM": radio.ArmedAway, "NIGHT_ALARM": radio.ArmedNight, "ALARM_OFF": radio.Disarmed,
		"EMERGENCY": radio.Emergency, "TAMPER": radio.Tamper,
	}
	sirenStates = map[string]int{
		"OFF": 0, "TIMEOUT_BEFORE_ARMED": 2, "ARMED": 3, "TIMEOUT_BEFORE_ALERT": 4, "ALERT_WITH_SRN": 5,
	}
	stateKinds = []radio.EventKind{radio.Opened, radio.Closed, radio.MotionStart, radio.MotionEnd,
		radio.ArmedAway, radio.ArmedNight, radio.Disarmed, radio.Emergency, radio.Tamper,
		radio.SirenState, radio.Unhandled}
)

// fbxhomeReading is the state event fbxhome logged about a received frame.
func fbxhomeReading(model radio.Model, rx fbxreplay.Frame) (radio.Event, bool) {
	pattern := fbxReading[model]
	if pattern == nil {
		return radio.Event{}, false
	}
	for _, note := range rx.Notes {
		m := pattern.FindStringSubmatch(note)
		if m == nil {
			continue
		}
		ev := radio.Event{Addr: rx.Src}
		if model == radio.SRN {
			state, known := sirenStates[m[1]]
			ev.Kind, ev.Value = radio.SirenState, state
			return ev, known
		}
		ev.Kind = fromFbxhome[m[1]]
		return ev, true
	}
	return radio.Event{}, false
}

// checkEvents: the state events the engine reports are exactly what
// fbxhome's log says the sensor meant (a siren only when fbxhome logs a
// state change), and battery levels are the ones fbxhome set.
func checkEvents(rx fbxreplay.Frame, model radio.Model, events []radio.Event, rep *Report) {
	var got []radio.Event
	for _, ev := range events {
		if slices.Contains(stateKinds, ev.Kind) {
			got = append(got, ev)
		}
	}
	want, ok := fbxhomeReading(model, rx)
	switch {
	case ok:
		rep.EventsChecked++
		// A code disarm carries the code; fbxhome's log line does not.
		if len(got) != 1 || got[0].Kind != want.Kind || got[0].Value != want.Value || got[0].Addr != want.Addr {
			rep.diverge(rx.Line, "engine read %v, fbxhome %v, in %s", got, want, describe(rx.ManagedFrame))
		}
	case len(got) > 0 && model != radio.SRN:
		rep.diverge(rx.Line, "engine read %v, fbxhome nothing, in %s", got, describe(rx.ManagedFrame))
	}
	if rx.WFlags != 0x82 { // battery only from status heartbeats
		return
	}
	for _, note := range rx.Notes {
		if b := fbxBattery.FindStringSubmatch(note); b != nil {
			level, _ := strconv.Atoi(b[1])
			rep.EventsChecked++
			if !slices.Contains(events, radio.Event{Addr: rx.Src, Kind: radio.Battery, Value: level}) {
				rep.diverge(rx.Line, "engine %v, fbxhome battery %d", events, level)
			}
			return
		}
	}
}
