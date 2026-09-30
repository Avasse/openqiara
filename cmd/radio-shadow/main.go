// Command radio-shadow replays fbxhome debug logs through the radio engine
// and reports every difference with what fbxhome did. Nothing is sent: it
// reads logs pulled from the camera, e.g.
//
//	ssh root@camera cat /var/log/fbxhome.log > today.log
//	ssh root@camera cat /data/fbxhome.log > runs.log
//	radio-shadow -siren 6 -node 2:KPD:0 -node 3:PIR:1 -node 5:DWS:2 -node 6:SRN:3 today.log runs.log
//
// /var/log/fbxhome.log is timestamped in the camera's time zone;
// /data/fbxhome.log spans several fbxhome runs but has no time, so its time
// frames are not compared. Keypad codes are compared by structure and never
// printed. With -manifest and -bytecode (the camera's update manifest and
// bytecode files), bytecode pushes are built like in production and
// compared byte for byte; without, the engine replays fbxhome's own.
// The exit status is 1 when the engine differs from fbxhome.
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caligone/openqiara/internal/domusvm"
	"github.com/caligone/openqiara/internal/fbxreplay"
	"github.com/caligone/openqiara/internal/radio"
	"github.com/caligone/openqiara/internal/radio/shadow"
)

var models = map[string]radio.Model{"DWS": radio.DWS, "PIR": radio.PIR, "KPD": radio.KPD, "SRN": radio.SRN}

type nodeFlags []radio.Node

func (n *nodeFlags) String() string { return fmt.Sprint(*n) }

// Set reads addr:model:system_index, e.g. 5:DWS:2.
func (n *nodeFlags) Set(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return fmt.Errorf("want addr:model:system_index, got %q", s)
	}
	addr, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return err
	}
	model, ok := models[strings.ToUpper(parts[1])]
	if !ok {
		return fmt.Errorf("unknown model %q", parts[1])
	}
	index, err := strconv.ParseUint(parts[2], 10, 8)
	if err != nil {
		return err
	}
	*n = append(*n, radio.Node{Addr: uint32(addr), Model: model, SystemIndex: byte(index)})
	return nil
}

func main() {
	var nodes nodeFlags
	flag.Var(&nodes, "node", "paired sensor as addr:model:system_index (repeat)")
	gateway := flag.Uint("gateway", 1, "gateway radio address")
	siren := flag.Uint("siren", 0, "alarm siren address sent in sensor configs, 0 for none")
	codes := flag.Int("codes", 1, "number of 4-digit codes on the keypad")
	zone := flag.String("tz", "Europe/Paris", "time zone of the camera's log")
	manifest := flag.String("manifest", "", "update_manifest.json of the camera")
	bytecode := flag.String("bytecode", "", "directory of the camera's <hash>.bin bytecode files")
	flag.Parse()

	loc, err := time.LoadLocation(*zone)
	if err != nil {
		fail(err)
	}
	for i := range nodes {
		if nodes[i].Model == radio.KPD {
			nodes[i].PINs = slices.Repeat([]string{"0000"}, *codes) // compared by structure only
		}
	}
	opts := radio.Options{Gateway: uint32(*gateway), AlarmSiren: uint32(*siren), Nodes: nodes}
	if *manifest != "" {
		m, err := domusvm.LoadManifest(*manifest)
		if err != nil {
			fail(err)
		}
		opts.Bytecode = func(fw []byte) ([][]byte, error) { return m.Frames(*bytecode, fw) }
	}

	differs := false
	for _, path := range flag.Args() {
		f, err := os.Open(path)
		if err != nil {
			fail(err)
		}
		frames, err := fbxreplay.ParseIn(f, loc)
		f.Close()
		if err != nil {
			fail(err)
		}
		rep, err := shadow.Compare(frames, opts)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s: %d fbxhome runs, %d frames reproduced, %d events checked, %d differences\n",
			path, rep.Runs, rep.Reproduced, rep.EventsChecked, len(rep.Divergences))
		for why, n := range rep.Excused {
			fmt.Printf("  not reproduced on purpose, %d× %s\n", n, why)
		}
		for _, d := range rep.Divergences {
			fmt.Println("  " + d)
		}
		differs = differs || len(rep.Divergences) > 0
	}
	if differs {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "radio-shadow:", err)
	os.Exit(2)
}
