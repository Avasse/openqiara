package shadow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caligone/openqiara/internal/fbxreplay"
	"github.com/caligone/openqiara/internal/radio"
)

// The fixtures of internal/fbxreplay, with the sensors paired on the camera
// they come from (addresses and system indexes from its fbxhome.xml; keypad
// codes are anonymised to 0000).
var fixtures = []struct {
	file string
	opts radio.Options
}{
	{"steady_day.log", radio.Options{Gateway: 1, AlarmSiren: 6, Nodes: []radio.Node{
		{Addr: 2, Model: radio.KPD, SystemIndex: 0, PINs: []string{"0000"}},
		{Addr: 3, Model: radio.PIR, SystemIndex: 1},
		{Addr: 5, Model: radio.DWS, SystemIndex: 2},
		{Addr: 6, Model: radio.SRN, SystemIndex: 3},
	}}},
	{"alarm_cycle.log", radio.Options{Gateway: 1, AlarmSiren: 7, Nodes: []radio.Node{
		{Addr: 3, Model: radio.DWS},
		{Addr: 4, Model: radio.PIR},
		{Addr: 6, Model: radio.KPD, PINs: []string{"0000"}},
		{Addr: 7, Model: radio.SRN},
	}}},
}

func load(t *testing.T, name string) []fbxreplay.Frame {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "fbxreplay", "testdata", name))
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

// TestFixturesMatchFbxhome: on both transcripts the engine sends what
// fbxhome sent, frame for frame, reads what fbxhome read, and every fbxhome
// frame it does not reproduce has a listed reason.
func TestFixturesMatchFbxhome(t *testing.T) {
	for _, fx := range fixtures {
		rep, err := Compare(load(t, fx.file), fx.opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range rep.Divergences {
			t.Errorf("%s %s", fx.file, d)
		}
		t.Logf("%s: %d frames reproduced, %d events checked, excused %v",
			fx.file, rep.Reproduced, rep.EventsChecked, rep.Excused)
		if rep.Reproduced == 0 || rep.EventsChecked == 0 {
			t.Errorf("%s: nothing compared", fx.file)
		}
	}
}

// TestDivergencesAreReported: a wrong setup must show, or a silent
// comparator would pass anything.
func TestDivergencesAreReported(t *testing.T) {
	opts := fixtures[0].opts
	opts.Nodes = append([]radio.Node(nil), opts.Nodes...)
	opts.Nodes[2].SystemIndex = 9   // the DWS config is no longer fbxhome's
	opts.Nodes[1].Model = radio.DWS // the PIR's events are read as a door's
	rep, err := Compare(load(t, "steady_day.log"), opts)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rep.Divergences, "\n")
	if !strings.Contains(all, "55000906000000") || !strings.Contains(all, "fbxhome nothing") {
		t.Fatalf("divergences not reported:\n%s", all)
	}
}
