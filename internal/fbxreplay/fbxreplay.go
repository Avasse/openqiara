// Package fbxreplay reads the radio transcript fbxhome writes to its debug
// log, so that a radio engine can be replayed against it.
//
// fbxhome logs every managed frame it receives
//
//	[gwdst:1, gwsrc:3, cnt:13794, src:3, rf_sig:93, rf_cfg:240, flags:131ZW, wflags:2M, payload:81ff]
//
// and every frame it sends, prefixed with "Sent:" or "Sent manage:". Unlike
// a single pcap, a day of that log covers several sensors, the camera boot,
// steady state, a siren reboot and the KPD PIN push, which makes it the
// oracle for replacing fbxhome. testdata/ holds anonymised transcripts.
//
// To replay a radio engine, feed it the received frames in log order and
// compare what it sends back with the sent frames whose AckOf points to
// them (internal/radio/replay_test.go).
package fbxreplay

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Frame is one managed frame as fbxhome logged it.
type Frame struct {
	charmux.ManagedFrame // AckDst/AckCnt hold waitsrc/waitcnt in MCU reports

	Line   int       // 1-based line number in the log
	Time   time.Time // log timestamp, read as UTC
	Sent   bool      // emitted by fbxhome, i.e. by the gateway
	Reason string    // MCU delivery report, e.g. "UNREACHABLE"

	// Notes are fbxhome's own log lines after a received frame, up to the
	// next received frame: its reading of that frame ("Pir: 17 mvt start").
	Notes []string

	// AckOf is the index of the frame this one acknowledges, or -1 when it
	// acknowledges nothing or a frame older than the log.
	AckOf int
}

var (
	logLine   = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \[\w+\] (.*)$`)
	frameText = regexp.MustCompile(`^(Sent manage: |Sent: )?\[(gwdst:[^\]]*)\]`)
	field     = regexp.MustCompile(`(\w+):([^,]*)`)
	numPrefix = regexp.MustCompile(`^\d+`)
)

// Parse returns the frames of a fbxhome debug log, in log order, with AckOf
// resolved. Other log lines end up in the Notes of the last received frame.
func Parse(r io.Reader) ([]Frame, error) {
	var frames []Frame
	lastRX := -1
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := logLine.FindStringSubmatch(sc.Text())
		if line == nil {
			continue
		}
		stamp, text := line[1], line[2]
		m := frameText.FindStringSubmatch(text)
		if m == nil {
			if lastRX >= 0 {
				frames[lastRX].Notes = append(frames[lastRX].Notes, text)
			}
			continue
		}
		prefix, fields := m[1], m[2]
		f, err := parseFields(fields)
		if err != nil {
			return nil, fmt.Errorf("fbxreplay: line %d: %w", n, err)
		}
		f.Line = n
		f.Time, err = time.Parse(time.DateTime, stamp)
		if err != nil {
			return nil, fmt.Errorf("fbxreplay: line %d: %w", n, err)
		}
		f.Sent = prefix != ""
		if !f.Sent {
			lastRX = len(frames)
		}
		frames = append(frames, f)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fbxreplay: %w", err)
	}
	link(frames)
	return frames, nil
}

func parseFields(s string) (Frame, error) {
	var f Frame
	for _, kv := range field.FindAllStringSubmatch(s, -1) {
		key, val := kv[1], kv[2]
		var err error
		switch key {
		case "gwdst":
			f.GWDst, err = parseUint32(val)
		case "gwsrc":
			f.GWSrc, err = parseUint32(val)
		case "cnt":
			f.Counter, err = parseUint32(val)
		case "src":
			f.Src, err = parseUint32(val)
		case "rt":
			f.Route, err = parseUint32(val)
		case "ackdst", "waitsrc":
			f.AckDst, err = parseUint32(val)
		case "ackcnt", "waitcnt":
			f.AckCnt, err = parseUint32(val)
		case "flags":
			// "1351ZWAE": the number is the whole u16, the letters are
			// fbxhome's rendering of some of its bits.
			var v uint64
			v, err = strconv.ParseUint(numPrefix.FindString(val), 10, 16)
			f.Flags = uint16(v)
		case "wflags":
			f.WFlags, err = parseWFlags(val)
		case "payload":
			f.Payload, err = hex.DecodeString(val)
		case "reason":
			f.Reason = val
		case "rf_sig", "rf_cfg":
			// Radio metrics, not part of the protocol exchange.
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return Frame{}, fmt.Errorf("%s:%s: %w", key, val, err)
		}
	}
	return f, nil
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

// parseWFlags decodes fbxhome's rendering of the wflags byte: the low six
// bits in decimal, then G for 0x40 and M for 0x80 ("12GM" is 0xcc).
func parseWFlags(s string) (byte, error) {
	digits := numPrefix.FindString(s)
	v, err := strconv.ParseUint(digits, 10, 6)
	if err != nil {
		return 0, err
	}
	for _, c := range s[len(digits):] {
		switch c {
		case 'G':
			v |= 0x40
		case 'M':
			v |= 0x80
		default:
			return 0, fmt.Errorf("unknown wflags letter %q", c)
		}
	}
	return byte(v), nil
}

// link resolves AckOf. An acknowledgement names the acknowledged frame by
// its sender address (ackdst) and counter (ackcnt), MCU reports do the same
// with waitsrc/waitcnt, and that frame always comes from the other side.
// Counters restart with fbxhome, so the closest earlier match wins.
func link(frames []Frame) {
	for i := range frames {
		f := &frames[i]
		f.AckOf = -1
		if f.Flags&charmux.FlagA == 0 && f.Reason == "" {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			g := frames[j]
			if g.Sent != f.Sent && g.Src == f.AckDst && g.Counter == f.AckCnt {
				f.AckOf = j
				break
			}
		}
	}
}
