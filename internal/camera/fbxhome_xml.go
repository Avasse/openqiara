package camera

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/caligone/openqiara/internal/config"
)

// fbxhomeXMLGlob is where fbxhome keeps its state.
const fbxhomeXMLGlob = "/data/fbxhome.xml.*"

var errNoFbxhomeXML = errors.New("no fbxhome.xml.N found")

var counterRe = regexp.MustCompile(`counter="(\d+)"`)

// latestFbxhomeXML reads the newest of fbxhome's state files. fbxhome
// writes fbxhome.xml.0..9 wear-levelled, not round-robin: the newest is
// the one with the highest counter="N", not .0.
func latestFbxhomeXML(glob string) ([]byte, error) {
	matches, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("glob xml: %w", err)
	}
	var latest []byte
	best := -1
	for _, p := range matches {
		// Skip .bak and any file that's not strictly .N (digit).
		if _, err := strconv.Atoi(strings.TrimPrefix(filepath.Ext(p), ".")); err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m := counterRe.FindSubmatch(data)
		if m == nil {
			continue
		}
		if c, _ := strconv.Atoi(string(m[1])); c > best {
			best, latest = c, data
		}
	}
	if latest == nil {
		return nil, errNoFbxhomeXML
	}
	return latest, nil
}

// fbxhomeRadio is what openqiarad takes from fbxhome's state when it
// takes the radio over. fbxhome.xml also holds the sensors' keys and the
// keypad codes: they are not read.
type fbxhomeRadio struct {
	Adapter struct {
		NextAddr uint32 `xml:"domus_next_addr,attr"`
	} `xml:"Adapter"`
	Nodes []struct {
		ID          int    `xml:"id,attr"`
		Type        string `xml:"type,attr"`
		Discarded   bool   `xml:"discarded,attr"`
		Addr        uint32 `xml:"domus_addr,attr"`
		UID         string `xml:"domus_item_id,attr"`
		SystemIndex uint8  `xml:"system_index,attr"`
	} `xml:"Node"`
}

// ImportFbxhomeRadio gives each sensor the radio identity fbxhome knew it
// by, so that charmux mode serves the sensors fbxhome paired without
// pairing them again. Sensors match on their id, which is fbxhome's node
// id; the ones fbxhome knew and the config does not are added, unless
// they were deleted. A sensor that already has a radio identity keeps it.
// It returns the ids imported, none without fbxhome state.
func ImportFbxhomeRadio(store *config.Store, glob string) ([]int, error) {
	data, err := latestFbxhomeXML(glob)
	if errors.Is(err, errNoFbxhomeXML) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st fbxhomeRadio
	if err := xml.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse fbxhome.xml: %w", err)
	}

	cfg := store.Get()
	var imported []int
	for _, n := range st.Nodes {
		typ := sensorTypeFromNodeType[n.Type]
		i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == n.ID })
		if typ == "" || n.Discarded || n.Addr == 0 || slices.Contains(cfg.DeletedIDs, n.ID) ||
			i >= 0 && cfg.Sensors[i].Radio.Addr != 0 {
			continue
		}
		imported = append(imported, n.ID)
	}
	if len(imported) == 0 && cfg.RadioNextAddr >= st.Adapter.NextAddr {
		return nil, nil
	}
	err = store.Update(func(cfg *config.Config) {
		sensors := slices.Clone(cfg.Sensors) // Get() copies share the old array
		for _, n := range st.Nodes {
			if !slices.Contains(imported, n.ID) {
				continue
			}
			i := slices.IndexFunc(sensors, func(se config.SensorEntry) bool { return se.ID == n.ID })
			if i < 0 {
				sensors = append(sensors, config.SensorEntry{ID: n.ID})
				i = len(sensors) - 1
			}
			sensors[i].Type = sensorTypeFromNodeType[n.Type]
			sensors[i].Radio = config.RadioNode{Addr: n.Addr, UID: n.UID, SystemIndex: n.SystemIndex}
		}
		cfg.Sensors = sensors
		cfg.RadioNextAddr = max(cfg.RadioNextAddr, st.Adapter.NextAddr)
	})
	return imported, err
}
