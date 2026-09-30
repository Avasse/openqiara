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
// oracle for replacing fbxhome. testdata/ holds anonymised transcripts;
// internal/radio/shadow replays the radio engine against them.
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
	Run    int       // fbxhome run it belongs to: counters restart with each
	Time   time.Time // log timestamp, zero when the log has none
	Sent   bool      // emitted by fbxhome, i.e. by the gateway
	Reason string    // MCU delivery report, e.g. "UNREACHABLE"

	// Notes are fbxhome's own log lines after a received frame, up to the
	// next received frame: its reading of that frame ("Pir: 17 mvt start").
	Notes []string

	// AckOf is the index of the frame this one acknowledges, or -1 when it
	// acknowledges nothing or a frame the log does not have.
	AckOf int
}

var (
	stampedLine = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \[\w+\] (.*)$`) // /var/log/fbxhome.log
	syslogLine  = regexp.MustCompile(`^fbxhome\[\d+\]: (.*)$`)                          // /data/fbxhome.log
	frameText   = regexp.MustCompile(`^(Sent manage: |Sent: )?\[(gwdst:[^\]]*)\]`)
	runStart    = regexp.MustCompile(`^start loading`)
	field       = regexp.MustCompile(`(\w+):([^,]*)`)
	numPrefix   = regexp.MustCompile(`^\d+`)
)

// Parse reads a log whose timestamps are UTC, as the fixtures are.
func Parse(r io.Reader) ([]Frame, error) {
	return ParseIn(r, time.UTC)
}

// ParseIn returns the frames of a fbxhome debug log, in log order, with
// AckOf resolved, reading timestamps in loc (a camera logs its local time).
// It reads both /var/log/fbxhome.log, timestamped, and the copy of
// fbxhome's output in /data/fbxhome.log, which has no time. Other log lines
// end up in the Notes of the last received frame.
func ParseIn(r io.Reader, loc *time.Location) ([]Frame, error) {
	var frames []Frame
	run, lastRX := 0, -1
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		var stamp, text string
		if m := stampedLine.FindStringSubmatch(sc.Text()); m != nil {
			stamp, text = m[1], m[2]
		} else if m := syslogLine.FindStringSubmatch(sc.Text()); m != nil {
			text = m[1]
		} else {
			continue
		}
		m := frameText.FindStringSubmatch(text)
		if m == nil {
			if runStart.MatchString(text) {
				run, lastRX = run+1, -1
			} else if lastRX >= 0 {
				frames[lastRX].Notes = append(frames[lastRX].Notes, text)
			}
			continue
		}
		f, err := parseFields(m[2])
		if err != nil {
			return nil, fmt.Errorf("fbxreplay: line %d: %w", n, err)
		}
		f.Line, f.Run, f.Sent = n, run, m[1] != ""
		if stamp != "" {
			if f.Time, err = time.ParseInLocation(time.DateTime, stamp, loc); err != nil {
				return nil, fmt.Errorf("fbxreplay: line %d: %w", n, err)
			}
		}
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
// with waitsrc/waitcnt, and that frame always comes from the other side of
// the same fbxhome run.
func link(frames []Frame) {
	for i := range frames {
		f := &frames[i]
		f.AckOf = -1
		if f.Flags&charmux.FlagA == 0 && f.Reason == "" {
			continue
		}
		for j := i - 1; j >= 0 && frames[j].Run == f.Run; j-- {
			g := frames[j]
			if g.Sent != f.Sent && g.Src == f.AckDst && g.Counter == f.AckCnt {
				f.AckOf = j
				break
			}
		}
	}
}
