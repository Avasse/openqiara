package fbxreplay

import (
	"bytes"
	"strings"
	"testing"
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
	if !readStatus.Sent || readStatus.Flags != 0x0547 || readStatus.WFlags != 0xcc ||
		readStatus.AckOf != 0 || readStatus.Line != 3 {
		t.Errorf("read_status: %+v", readStatus)
	}
	if !routed.Sent || routed.Flags != 0x0d43 || routed.Route != 6 || routed.AckOf != -1 {
		t.Errorf("routed command: %+v", routed)
	}
	if report.Reason != "UNREACHABLE" || report.AckDst != 1 || report.AckCnt != 11 || report.AckOf != 2 {
		t.Errorf("MCU report: %+v", report)
	}
	if bareAck.Flags != 0x0084 || bareAck.WFlags != 0 || bareAck.Payload != nil || bareAck.AckOf != -1 {
		t.Errorf("bare ack: %+v", bareAck)
	}
}
