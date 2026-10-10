package fbxbus

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeDaemon plays fbxbusd for one client: it answers hello and the name
// request, takes the filters, then relays one known and one unknown call.
func fakeDaemon(t *testing.T, l *net.UnixListener, rules int) (replies chan Message) {
	replies = make(chan Message, 4)
	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		serial := uint32(100)
		send := func(m *Message) {
			serial++
			m.Serial = serial
			_, _ = conn.Write(m.header())
			if len(m.Body) > 0 {
				_, _ = conn.Write(m.Body)
			}
		}
		hello, _ := read(conn)
		send(&Message{Type: TypeReply, ReplySerial: hello.Serial, Signature: "s", Body: appendString(nil, ":1-1")})
		name, _ := read(conn)
		if s, _ := bodyString(name.Body); s != "hl_event_collectd" || name.Body[len(name.Body)-1] != 1 {
			t.Errorf("name request body % x", name.Body)
		}
		send(&Message{Type: TypeReply, ReplySerial: name.Serial, Signature: "i", Body: binary.LittleEndian.AppendUint32(nil, 3)})
		for range rules {
			if f, _ := read(conn); f.Path != "/fbxbus/filter" || f.Member != "add" {
				t.Errorf("filter add expected, got %s %s", f.Path, f.Member)
			}
		}
		send(&Message{Type: TypeCall, Path: "/hl_event_collectd", Member: "new_notification", Signature: "s",
			Body: appendString(nil, `{"type":"iv_event","data":{}}`)})
		send(&Message{Type: TypeCall, Path: "/hl_event_collectd", Member: "nope"})
		for range 2 {
			m, err := read(conn)
			if err != nil {
				return
			}
			replies <- m
		}
	}()
	return replies
}

func TestServiceTakesTheNameAndAnswers(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "bus")
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: addr, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	got := make(chan string, 1)
	svc := &Service{
		Name: "hl_event_collectd", Addr: addr,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Methods: map[string]Handler{
			"new_event":        func(string) {},
			"new_notification": func(s string) { got <- s },
		},
	}
	replies := fakeDaemon(t, l, len(svc.Methods))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	select {
	case s := <-got:
		if s != `{"type":"iv_event","data":{}}` {
			t.Fatalf("handler got %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no call handled")
	}
	r := <-replies
	if r.Type != TypeReply || r.ReplySerial != 103 || len(r.Body) != 0 {
		t.Fatalf("reply to the call: %+v", r)
	}
	if e := <-replies; e.Type != TypeError || e.ErrorCode != ErrNotImplemented || e.ReplySerial != 104 {
		t.Fatalf("answer to an unknown method: %+v", e)
	}
}
