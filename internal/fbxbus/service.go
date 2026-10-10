package fbxbus

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"
)

// DaemonAddr is fbxbusd's abstract socket.
const DaemonAddr = "@fbxbus_daemon"

// Handler gets the string argument of a call. It runs on the reading
// goroutine, after the reply went: it must not block, or fbxbusd, single
// threaded, stalls the whole bus while it waits to write to us.
type Handler func(arg string)

// Service holds a name on the bus and answers its methods, each taking a
// string (hl_event_collectd's are JSON).
type Service struct {
	Name    string
	Methods map[string]Handler
	Addr    string // DaemonAddr unless testing
	Log     *slog.Logger

	serial uint32
}

// Run serves until ctx ends, connecting again whenever the bus drops us.
func (s *Service) Run(ctx context.Context) {
	if s.Addr == "" {
		s.Addr = DaemonAddr
	}
	wait := time.Second
	for ctx.Err() == nil {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.Log.Warn("fbxbus: session ended, connecting again", "name", s.Name, "error", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// session connects, takes the name and serves calls until the connection
// fails.
func (s *Service) session(ctx context.Context) error {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: s.Addr, Net: "unixpacket"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	s.serial = 0

	pid := binary.LittleEndian.AppendUint32(nil, uint32(os.Getpid()))
	if _, err := s.call(conn, "/fbxbus", "hello", "i", pid); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	// replace: the daemon drops whoever holds the name (the vendor's
	// daemon, if it came back).
	req := appendString(nil, s.Name)
	req = append(req, 1)
	rep, err := s.call(conn, "/fbxbus/name", "request", "sb", req)
	if err != nil {
		return fmt.Errorf("name request: %w", err)
	}
	switch code, err := bodyInt32(rep.Body); {
	case err != nil:
		return fmt.Errorf("name request: %w", err)
	case code != 0 && code != 1 && code != 3:
		return fmt.Errorf("name %s refused (%d)", s.Name, code)
	}
	// After the name: a rule another peer holds is dropped silently.
	for member := range s.Methods {
		if err := s.send(conn, &Message{Type: TypeCall, Path: "/fbxbus/filter", Member: "add",
			Signature: "a(iss())", Body: filterBody(1, "/"+s.Name, member)}); err != nil {
			return fmt.Errorf("filter add %s: %w", member, err)
		}
	}
	s.Log.Info("fbxbus: serving", "name", s.Name)

	for {
		m, err := read(conn)
		if err != nil {
			return err
		}
		if m.Type != TypeCall {
			continue // replies to filter add, signals
		}
		h, ok := s.Methods[m.Member]
		if !ok || m.Path != "/"+s.Name {
			msg := appendString(nil, "not implemented")
			if err := s.send(conn, &Message{Type: TypeError, ReplySerial: m.Serial,
				ErrorCode: ErrNotImplemented, Signature: "s", Body: msg}); err != nil {
				return err
			}
			continue
		}
		// Reply first: the caller's tracker in fbxbusd is released.
		if err := s.send(conn, &Message{Type: TypeReply, ReplySerial: m.Serial}); err != nil {
			return err
		}
		arg := ""
		if m.Signature == "s" {
			if arg, err = bodyString(m.Body); err != nil {
				s.Log.Warn("fbxbus: bad call", "member", m.Member, "error", err)
				continue
			}
		}
		h(arg)
	}
}

// call sends a call and waits for its reply, before the name is served:
// nothing else comes meanwhile but signals.
func (s *Service) call(conn *net.UnixConn, path, member, sig string, body []byte) (Message, error) {
	m := &Message{Type: TypeCall, Path: path, Member: member, Signature: sig, Body: body}
	if err := s.send(conn, m); err != nil {
		return Message{}, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		r, err := read(conn)
		if err != nil {
			return Message{}, err
		}
		if r.ReplySerial != m.Serial {
			continue
		}
		if r.Type == TypeError {
			return r, fmt.Errorf("error %#x", r.ErrorCode)
		}
		return r, nil
	}
}

// send numbers m and writes it: the header packet, then the body.
func (s *Service) send(conn *net.UnixConn, m *Message) error {
	s.serial++
	if s.serial == 0 {
		s.serial = 1
	}
	m.Serial = s.serial
	if _, err := conn.Write(m.header()); err != nil {
		return err
	}
	for b := m.Body; len(b) > 0; {
		n := min(len(b), maxBodyChunk)
		if _, err := conn.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// read reads one message: a header packet (which may carry the start of
// the body), then body packets up to the announced length.
func read(conn *net.UnixConn) (Message, error) {
	buf := make([]byte, 0x10000)
	n, err := conn.Read(buf)
	if err != nil {
		return Message{}, err
	}
	m, hdrLen, bodyLen, err := parseHeader(buf[:n])
	if err != nil {
		return Message{}, err
	}
	body := append([]byte(nil), buf[min(hdrLen, n):n]...)
	for len(body) < bodyLen {
		n, err := conn.Read(buf)
		if err != nil {
			return Message{}, err
		}
		body = append(body, buf[:n]...)
	}
	m.Body = body[:bodyLen]
	return m, nil
}
