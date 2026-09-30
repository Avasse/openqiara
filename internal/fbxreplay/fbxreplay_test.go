package fbxreplay

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caligone/openqiara/internal/charmux"
)

func TestParse(t *testing.T) {
	log := `2020-01-01 00:04:45 [debug] [gwdst:1, gwsrc:3, cnt:13794, src:3, rf_sig:93, rf_cfg:240, flags:131ZW, wflags:2M, payload:81ff]
2020-01-01 00:04:45 [debug] DomusNode (Capteur de mouvement) need status
2020-01-01 00:04:45 [debug] Sent manage: [gwdst:3, gwsrc:1, cnt:2, src:1, rf_sig:37, rf_cfg:102, flags:1351ZWAE, ackdst:3, ackcnt:13794, wflags:12GM, payload:78]
2020-01-01 08:29:02 [debug] Sent: [gwdst:6, gwsrc:1, cnt:11, src:1, rf_sig:0, rf_cfg:0, flags:3395ZWE, rt:6, wflags:1, payload:5506]
2020-01-01 08:29:03 [debug] [gwdst:1, gwsrc:1, cnt:8807242, src:1, rf_sig:62, rf_cfg:164, flags:64, reason:UNREACHABLE, waitsrc:1, waitcnt:11]
2020-01-01 08:35:07 [debug] Sent: [gwdst:5, gwsrc:1, cnt:12, src:1, rf_sig:37, rf_cfg:96, flags:132A, ackdst:5, ackcnt:1240]
`
	frames, err := Parse(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}

	status, readStatus, routed, report, bareAck := frames[0], frames[1], frames[2], frames[3], frames[4]
	if status.Sent || status.GWSrc != 3 || status.Counter != 13794 || status.Flags != 0x83 ||
		status.WFlags != 0x82 || !bytes.Equal(status.Payload, []byte{0x81, 0xff}) || status.AckOf != -1 {
		t.Errorf("status heartbeat: %+v", status)
	}
	if len(status.Notes) != 1 || status.Notes[0] != "DomusNode (Capteur de mouvement) need status" {
		t.Errorf("status heartbeat notes: %q", status.Notes)
	}
	if !readStatus.Sent || !readStatus.Manage || readStatus.Flags != 0x0547 || readStatus.WFlags != 0xcc ||
		readStatus.AckOf != 0 || readStatus.Line != 3 {
		t.Errorf("read_status: %+v", readStatus)
	}
	if routed.Manage || routed.Flags != 0x0d43 || routed.Route != 6 || routed.AckOf != -1 {
		t.Errorf("routed command: %+v", routed)
	}
	if report.Reason != "UNREACHABLE" || report.AckDst != 1 || report.AckCnt != 11 || report.AckOf != 2 {
		t.Errorf("MCU report: %+v", report)
	}
	if bareAck.Flags != 0x0084 || bareAck.WFlags != 0 || bareAck.Payload != nil || bareAck.AckOf != -1 {
		t.Errorf("bare ack: %+v", bareAck)
	}
}

// fixtures are anonymised fbxhome transcripts, see testdata/README.md.
var fixtures = []struct {
	file   string
	models map[uint32]string // radio address → sensor type
}{
	{"steady_day.log", map[uint32]string{2: "KPD", 3: "PIR", 5: "DWS", 6: "SRN"}},
	{"alarm_cycle.log", map[uint32]string{3: "DWS", 4: "PIR", 6: "KPD", 7: "SRN"}},
}

func load(t *testing.T, name string) []Frame {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	frames, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

// reaction is one row of fbxhome's reaction table: what the gateway sends
// back to a sensor frame that asks for an answer (flag Z).
type reaction struct {
	name    string
	trigger func(rx Frame) bool
	reply   func(tx Frame, model string) bool
}

var reactions = []reaction{
	{
		"status heartbeat → read_status(0x78)",
		func(rx Frame) bool { return rx.WFlags == 0x82 },
		func(tx Frame, _ string) bool { return tx.WFlags == 0xcc && bytes.Equal(tx.Payload, []byte{0x78}) },
	},
	{
		"read_status(0x78) answer, bytecode needed → first VM write",
		func(rx Frame) bool { return isReadStatusAnswer(rx, 0x78) && rx.Payload[0]&0x40 != 0 },
		func(tx Frame, _ string) bool { return tx.WFlags == 0xcd },
	},
	{
		"read_status(0x78) answer, bytecode loaded → config once, then bare ack",
		func(rx Frame) bool { return isReadStatusAnswer(rx, 0x78) && rx.Payload[0]&0x40 == 0 },
		func(tx Frame, _ string) bool { return isConfig(tx) || isBareAck(tx, 0x0004) },
	},
	{
		"read_status(0x08) answer → class 5 frame",
		func(rx Frame) bool { return isReadStatusAnswer(rx, 0x08) },
		func(tx Frame, _ string) bool { return tx.WFlags == 0x85 },
	},
	{
		"VM write ack → next VM write, then time",
		func(rx Frame) bool { return rx.WFlags == 0x81 && len(rx.Payload) == 5 },
		func(tx Frame, _ string) bool { return tx.WFlags == 0xcd || tx.WFlags == 0xc8 },
	},
	{
		"empty answer to the time frame → config",
		func(rx Frame) bool { return rx.Flags&charmux.FlagW == 0 },
		func(tx Frame, _ string) bool { return isConfig(tx) },
	},
	{
		"event → bare ack (0x0084 for a DWS, 0x0544 otherwise)",
		func(rx Frame) bool { return hasPayload(rx, 0x01, 0x55, 0x01) },
		func(tx Frame, model string) bool {
			if model == "DWS" {
				return isBareAck(tx, 0x0084)
			}
			return isBareAck(tx, 0x0544)
		},
	},
	{
		"KPD wake → kpd-post (PIN push)",
		func(rx Frame) bool { return hasPayload(rx, 0x01, 0x55, 0x09) },
		func(tx Frame, _ string) bool { return hasPayload(tx, 0x01, 0x03, 0x00) },
	},
}

func isReadStatusAnswer(f Frame, mask byte) bool {
	return f.WFlags == 0x81 && len(f.Payload) >= 2 && f.Payload[1] == mask
}

func isConfig(f Frame) bool {
	return hasPayload(f, 0x01, 0x55, 0x00) || hasPayload(f, 0x01, 0x55, 0x06)
}

func isBareAck(f Frame, flags uint16) bool {
	return f.Flags == flags && len(f.Payload) == 0
}

func hasPayload(f Frame, wflags byte, prefix ...byte) bool {
	return f.WFlags == wflags && bytes.HasPrefix(f.Payload, prefix)
}

// TestReactionTable replays every sensor frame of the fixtures against the
// reaction table: fbxhome answers exactly the frames flagged Z, once, and
// the answer is the one the table predicts. Every row must be exercised.
func TestReactionTable(t *testing.T) {
	hits := make([]int, len(reactions))
	for _, fx := range fixtures {
		t.Run(fx.file, func(t *testing.T) {
			frames := load(t, fx.file)
			replies := make(map[int][]Frame)
			for _, f := range frames {
				if f.Sent && f.AckOf >= 0 {
					replies[f.AckOf] = append(replies[f.AckOf], f)
				}
			}
			for i, rx := range frames {
				if rx.Sent || rx.Reason != "" {
					continue
				}
				got := replies[i]
				if rx.Flags&charmux.FlagZ == 0 {
					if len(got) != 0 {
						t.Errorf("line %d: answer to a frame without Z", rx.Line)
					}
					continue
				}
				if len(got) != 1 {
					t.Errorf("line %d: %d answers to a Z frame, want 1", rx.Line, len(got))
					continue
				}
				row := -1
				for r := range reactions {
					if !reactions[r].trigger(rx) {
						continue
					}
					if row >= 0 {
						t.Errorf("line %d: matches %q and %q", rx.Line, reactions[row].name, reactions[r].name)
					}
					row = r
				}
				if row < 0 {
					t.Errorf("line %d: Z frame outside the reaction table (wflags %#02x, payload %x)", rx.Line, rx.WFlags, rx.Payload)
					continue
				}
				hits[row]++
				if !reactions[row].reply(got[0], fx.models[rx.Src]) {
					t.Errorf("line %d: %s, got line %d (flags %#04x, wflags %#02x, payload %x)",
						rx.Line, reactions[row].name, got[0].Line, got[0].Flags, got[0].WFlags, got[0].Payload)
				}
			}
		})
	}
	for r, n := range hits {
		t.Logf("%3d × %s", n, reactions[r].name)
		if n == 0 {
			t.Errorf("row %q never exercised by the fixtures", reactions[r].name)
		}
	}
}

// TestGatewayCounterIsGlobal: fbxhome uses one TX counter for all sensors,
// incremented on every frame it sends, bare acks included.
func TestGatewayCounterIsGlobal(t *testing.T) {
	for _, fx := range fixtures {
		prev := -1
		for _, f := range load(t, fx.file) {
			if !f.Sent {
				continue
			}
			if prev >= 0 && int(f.Counter) != prev+1 {
				t.Errorf("%s line %d: counter %d after %d", fx.file, f.Line, f.Counter, prev)
			}
			prev = int(f.Counter)
		}
	}
}

// TestConfigCarriesSirenAddress: the DWS/PIR/KPD config is
// `55 00 <system_index> <u32 LE>` where the u32 is the siren's radio
// address (fbxhome RE: node field +0xfc of the Node.DomusNode.HlSrn,
// written only when a Node.HlAlarm exists).
func TestConfigCarriesSirenAddress(t *testing.T) {
	systemIndex := map[string]byte{"KPD": 0, "PIR": 1, "DWS": 2} // from fbxhome.xml
	fx := fixtures[0]
	const sirenAddr = 6
	n := 0
	for _, f := range load(t, fx.file) {
		if !f.Sent || !hasPayload(f, 0x01, 0x55, 0x00) {
			continue
		}
		n++
		model := fx.models[f.GWDst]
		if len(f.Payload) != 7 || f.Payload[2] != systemIndex[model] ||
			binary.LittleEndian.Uint32(f.Payload[3:]) != sirenAddr {
			t.Errorf("line %d: %s config %x", f.Line, model, f.Payload)
		}
	}
	if n != 3 {
		t.Errorf("got %d configs, want one per DWS/PIR/KPD", n)
	}
}

// TestMCUReportsTargetGatewayFrames: MCU delivery reports name the gateway
// frame they are about.
func TestMCUReportsTargetGatewayFrames(t *testing.T) {
	for _, fx := range fixtures {
		frames := load(t, fx.file)
		for _, f := range frames {
			if f.Reason == "" {
				continue
			}
			if f.AckOf < 0 || !frames[f.AckOf].Sent {
				t.Errorf("%s line %d: %s report without its gateway frame", fx.file, f.Line, f.Reason)
			}
		}
	}
}
