package radio

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/fbxreplay"
)

// The fbxhome transcripts of internal/fbxreplay, with the sensors paired on
// the camera they come from (addresses and system indexes from its
// fbxhome.xml; the keypad PIN is anonymised to 0000 in the fixtures).
var transcripts = []struct {
	file  string
	boot  bool // the capture starts when fbxhome starts
	nodes []Node
}{
	{"steady_day.log", true, []Node{
		{Addr: 2, Model: KPD, SystemIndex: 0, PIN: "0000"},
		{Addr: 3, Model: PIR, SystemIndex: 1},
		{Addr: 5, Model: DWS, SystemIndex: 2},
		{Addr: 6, Model: SRN, SystemIndex: 3},
	}},
	{"alarm_cycle.log", false, []Node{
		{Addr: 3, Model: DWS},
		{Addr: 4, Model: PIR},
		{Addr: 6, Model: KPD, PIN: "0000"},
		{Addr: 7, Model: SRN},
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

// transcriptBytecode serves the bytecode fbxhome pushed in the transcript,
// so VM frames compare byte for byte: the fixtures keep the frame
// structure, not the proprietary data.
func transcriptBytecode(frames []fbxreplay.Frame) func([]byte) ([]VMFrame, error) {
	pushes := make(map[string][]VMFrame)
	for i, f := range frames {
		p := f.Payload
		if f.Sent || f.WFlags != wfManageAnswer || len(p) < 10 || p[1] != readStatusAll || p[0]&statusNeedBytecode == 0 {
			continue
		}
		var vm []VMFrame
		for _, g := range frames[i+1:] {
			if !g.Sent || g.GWDst != f.Src {
				continue
			}
			if g.WFlags != wfBytecode {
				break
			}
			vm = append(vm, VMFrame{Payload: g.Payload, Ops: countOps(g.Payload)})
		}
		pushes[string(p[2:10])] = vm
	}
	return func(fw []byte) ([]VMFrame, error) { return pushes[string(fw)], nil }
}

// countOps walks a VM write frame: 0x01 marker, then ops (0x87 write: addr,
// length, data; 0x82 boot: mode, entry; 0x80 start and 0x88 erase: none).
func countOps(p []byte) int {
	ops := 0
	for i := 1; i < len(p); ops++ {
		op := p[i]
		i++
		switch op {
		case 0x87:
			i += 5 + int(p[i+4])
		case 0x82:
			i += 6
		}
	}
	return ops
}

// sameFrame compares an engine frame with fbxhome's, counter aside: the
// engine does not send what fbxhome sent on its own (Sigfox queries, the
// siren reboot), so the two counters drift apart.
func sameFrame(got charmux.ManagedFrame, want fbxreplay.Frame) error {
	w := want.ManagedFrame
	if got.GWDst != w.GWDst || got.GWSrc != w.GWSrc || got.Src != w.Src || got.Flags != w.Flags ||
		got.Route != w.Route || got.AckDst != w.AckDst || got.AckCnt != w.AckCnt || got.WFlags != w.WFlags {
		return fmt.Errorf("got %+v, fbxhome line %d sent %+v", got, want.Line, w)
	}
	if w.WFlags == wfTime { // carries the clock
		if len(got.Payload) != 4 {
			return fmt.Errorf("time payload %x", got.Payload)
		}
	} else if !bytes.Equal(got.Payload, w.Payload) {
		return fmt.Errorf("payload %x, fbxhome line %d sent %x", got.Payload, want.Line, w.Payload)
	}
	return nil
}

// TestReplayAgainstFbxhome feeds the engine every sensor frame fbxhome
// received and checks it answers exactly what fbxhome answered.
func TestReplayAgainstFbxhome(t *testing.T) {
	for _, tr := range transcripts {
		t.Run(tr.file, func(t *testing.T) {
			frames := loadTranscript(t, tr.file)
			e := New(Options{Gateway: 1, Nodes: tr.nodes, Bytecode: transcriptBytecode(frames)})
			ours := make(map[int]bool) // fbxhome frames the engine reproduced

			if tr.boot {
				first := 0
				for !frames[first].Sent {
					first++
				}
				start := e.Start().Send
				if len(start) != 1 {
					t.Fatalf("Start sent %d frames, want the siren get-state", len(start))
				}
				if err := sameFrame(start[0], frames[first]); err != nil {
					t.Errorf("Start: %v", err)
				}
				ours[first] = true
			}

			answered := 0
			for i, rx := range frames {
				if rx.Sent || rx.Reason != "" {
					continue // MCU reports name fbxhome's counters, not the engine's
				}
				got := e.Receive(rx.Time, rx.ManagedFrame).Send
				if rx.AckOf >= 0 && !ours[rx.AckOf] {
					continue // answers a frame fbxhome sent on its own
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
					answered++
				}
			}
			t.Logf("%d answers identical to fbxhome's", answered)
			if answered == 0 {
				t.Error("nothing compared")
			}
		})
	}
}

// TestCommandsMatchFbxhome: the siren commands fbxhome sent on its own
// (arming beep, alert...) are what Command builds.
func TestCommandsMatchFbxhome(t *testing.T) {
	n := 0
	for _, tr := range transcripts {
		e := New(Options{Gateway: 1, Nodes: tr.nodes})
		for _, f := range loadTranscript(t, tr.file) {
			if !f.Sent || f.Route == 0 || f.WFlags != wfApp {
				continue
			}
			got := e.Command(f.GWDst, f.Payload).Send
			if len(got) != 1 {
				t.Fatalf("%s line %d: no command frame", tr.file, f.Line)
			}
			if err := sameFrame(got[0], f); err != nil {
				t.Errorf("%s line %d: %v", tr.file, f.Line, err)
			}
			n++
		}
	}
	if n == 0 {
		t.Error("no command in the transcripts")
	}
}

var (
	fbxMotion  = regexp.MustCompile(`mvt (start|end)`)
	fbxDoor    = regexp.MustCompile(`Dws:\s+\d+ (open|close)`)
	fbxKeypad  = regexp.MustCompile(`KPD_(DAY_ALARM|NIGHT_ALARM|ALARM_OFF)`)
	fbxSiren   = regexp.MustCompile(`HlSrn state change from \w+ to (\w+)`)
	fbxBattery = regexp.MustCompile(`set_battery_level \(?(\d+)`)

	fromFbxhome = map[string]EventKind{
		"start": MotionStart, "end": MotionEnd, "open": Opened, "close": Closed,
		"DAY_ALARM": ArmedAway, "NIGHT_ALARM": ArmedNight, "ALARM_OFF": Disarmed,
	}
	sirenStates = map[string]int{
		"OFF": 0, "TIMEOUT_BEFORE_ARMED": 2, "ARMED": 3, "TIMEOUT_BEFORE_ALERT": 4, "ALERT_WITH_SRN": 5,
	}
)

// fbxhomeReading is the event fbxhome logged about a received frame, if any.
func fbxhomeReading(model Model, rx fbxreplay.Frame) (Event, bool) {
	ev := Event{Addr: rx.Src}
	for _, note := range rx.Notes {
		var m []string
		switch {
		case model == PIR:
			m = fbxMotion.FindStringSubmatch(note)
		case model == DWS:
			m = fbxDoor.FindStringSubmatch(note)
		case model == KPD:
			m = fbxKeypad.FindStringSubmatch(note)
		case model == SRN:
			if m = fbxSiren.FindStringSubmatch(note); m != nil {
				state, known := sirenStates[m[1]]
				ev.Kind, ev.Value = SirenState, state
				return ev, known
			}
		}
		if m != nil {
			ev.Kind = fromFbxhome[m[1]]
			return ev, true
		}
		if rx.WFlags == wfStatus {
			if b := fbxBattery.FindStringSubmatch(note); b != nil {
				ev.Kind = Battery
				ev.Value, _ = strconv.Atoi(b[1])
				return ev, true
			}
		}
	}
	return ev, false
}

// TestEventsMatchFbxhomeReading: for every state report and heartbeat,
// the engine reports what fbxhome's log says the sensor meant.
func TestEventsMatchFbxhomeReading(t *testing.T) {
	checked := 0
	for _, tr := range transcripts {
		models := make(map[uint32]Model)
		for _, n := range tr.nodes {
			models[n.Addr] = n.Model
		}
		e := New(Options{Gateway: 1, Nodes: tr.nodes})
		for _, rx := range loadTranscript(t, tr.file) {
			if rx.Sent || rx.Reason != "" {
				continue
			}
			got := e.Receive(rx.Time, rx.ManagedFrame).Events
			want, ok := fbxhomeReading(models[rx.Src], rx)
			if !ok {
				continue
			}
			checked++
			found := false
			for _, ev := range got {
				found = found || ev == want
			}
			if !found {
				t.Errorf("%s line %d (payload %x): engine reported %+v, fbxhome read %+v",
					tr.file, rx.Line, rx.Payload, got, want)
			}
		}
	}
	t.Logf("%d events checked against fbxhome's reading", checked)
	if checked == 0 {
		t.Error("nothing checked")
	}
}
