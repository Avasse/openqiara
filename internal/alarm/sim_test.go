package alarm

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// simSiren is the siren as its bytecode behaves and as the hardware showed
// it (2026-10-09), delays sped up: armed from off only; it counts its exit
// delay, entry delay and alert by itself and reports their ends; every
// command gets a report; with the gateway gone it hears the sensors and
// goes off by itself, again on every frame once its alert is over. It can
// answer back-to-back commands out of order, reboot, or be torn off.
type simSiren struct {
	mu      sync.Mutex
	state   SirenState
	active  map[int]bool // the sensors it was armed with
	delayed map[int]bool
	gen     int // bumped on every change of state: stale timers do nothing
	gone    bool
	absent  bool // siren_sounds none

	exit, entry, alert time.Duration
	sensors            map[int]SensorConfig // the doors and motion sensors of the config

	reverse bool // answers to back-to-back commands arrive in reverse order
	mute    bool // reports are lost (the gateway is gone)
	outbox  chan SirenState
	deliver func(SirenState)
}

func newSimSiren(sensors map[int]SensorConfig) *simSiren {
	s := &simSiren{state: SirenOff, sensors: sensors, outbox: make(chan SirenState, 64),
		exit: 40 * time.Millisecond, entry: 40 * time.Millisecond, alert: 40 * time.Millisecond}
	go s.post()
	return s
}

// post delivers the reports, a few milliseconds late, gathering those of
// back-to-back commands: in reverse order if asked.
func (s *simSiren) post() {
	for first := range s.outbox {
		batch := []SirenState{first}
		time.Sleep(3 * time.Millisecond)
	drain:
		for {
			select {
			case r := <-s.outbox:
				batch = append(batch, r)
			default:
				break drain
			}
		}
		s.mu.Lock()
		reverse, deliver := s.reverse, s.deliver
		s.mu.Unlock()
		if reverse {
			slices.Reverse(batch)
		}
		for _, r := range batch {
			if deliver != nil {
				deliver(r)
			}
		}
	}
}

// reportLocked sends the state, unless it is gone or its reports are lost.
func (s *simSiren) reportLocked() {
	if !s.gone && !s.mute {
		s.outbox <- s.state
	}
}

// setLocked changes state, and counts the next step if there is one.
func (s *simSiren) setLocked(st SirenState) {
	s.state = st
	s.gen++
	gen := s.gen
	var next SirenState
	var after time.Duration
	switch st {
	case SirenExitDelay:
		next, after = SirenArmed, s.exit
	case SirenEntryDelay:
		next, after = SirenAlert, s.entry
	case SirenAlert:
		next, after = SirenAlertOver, s.alert
	case SirenTest:
		next, after = SirenOff, s.alert
	default:
		return
	}
	time.AfterFunc(after, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gen == gen && !s.gone {
			s.setLocked(next)
			s.reportLocked() // an end it reports by itself
		}
	})
}

// command runs a command the siren takes in the states of its bytecode,
// then reports its state, taken or not.
func (s *simSiren) command(from []SirenState, to SirenState, also func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return nil
	}
	if from == nil || slices.Contains(from, s.state) {
		if also != nil {
			also()
		}
		s.setLocked(to)
	}
	s.reportLocked()
	return nil
}

func (s *simSiren) Present() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.absent }

func (s *simSiren) Arm(night, delayed bool) error {
	to := SirenArmed
	if delayed {
		to = SirenExitDelay
	}
	return s.command([]SirenState{SirenOff}, to, func() {
		s.active, s.delayed = map[int]bool{}, map[int]bool{}
		for id, cfg := range s.sensors {
			if night && cfg.NightAllowed {
				continue
			}
			s.active[id] = true
			s.delayed[id] = !cfg.Instant
		}
	})
}

func (s *simSiren) Disarm() error { return s.command(nil, SirenOff, nil) }
func (s *simSiren) EntryDelay() error {
	return s.command([]SirenState{SirenArmed}, SirenEntryDelay, nil)
}
func (s *simSiren) Alert() error {
	return s.command([]SirenState{SirenArmed, SirenEntryDelay, SirenAlertOver}, SirenAlert, nil)
}
func (s *simSiren) Wail() error         { return s.command([]SirenState{SirenOff}, SirenTest, nil) }
func (s *simSiren) RequestState() error { return s.command([]SirenState{}, "", nil) }

// sensorFrame is a sensor's alarm reaching the siren directly, the gateway
// gone: its bytecode's 55 01 handler. It reports nothing.
func (s *simSiren) sensorFrame(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone || !s.active[id] {
		return
	}
	switch {
	case s.state == SirenArmed && s.delayed[id]:
		s.setLocked(SirenEntryDelay)
	case s.state == SirenArmed || s.state == SirenEntryDelay || s.state == SirenAlertOver:
		if !s.delayed[id] || s.state == SirenAlertOver {
			s.setLocked(SirenAlert)
		}
	}
}

// reboot loses its arming; it reports off once its bytecode is back.
func (s *simSiren) reboot(silent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active, s.delayed = nil, nil
	s.setLocked(SirenOff)
	if !silent {
		s.reportLocked()
	}
}

func (s *simSiren) set(f func(*simSiren)) { s.mu.Lock(); defer s.mu.Unlock(); f(s) }

func (s *simSiren) now() SirenState { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

// eventually waits for cond, at most a second.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// holds checks cond stays true for a while.
func holds(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(150 * time.Millisecond); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if !cond() {
			t.Fatalf("not held: %s", what)
		}
	}
}
