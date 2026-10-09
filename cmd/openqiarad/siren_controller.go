package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// alarmoSiren is what the mirror of Alarmo asks of the siren (nativeSiren).
type alarmoSiren interface {
	Present() bool
	State() string
	Arm(night, delayed bool) error
	EntryDelay() error
	Alert() error
	Wail() error
	Disarm() error
}

// sirenController mirrors Alarmo's state on the siren, in alarmo mode:
// Alarmo decides and counts, the siren follows, armed natively so that it
// keeps the alarm by itself if the gateway goes. Its exit and entry delays
// are openqiara's (config): set them to Alarmo's, they only matter when
// the gateway is gone.
//
// Handle (Alarmo's state topic) and SirenState (the siren's reports) come
// from different goroutines: their commands go through one queue, in
// order (a disarm must not overtake an arming). In tests, synchronous runs
// them inline.
type sirenController struct {
	siren  alarmoSiren
	logger *slog.Logger
	ctx    context.Context

	mu        sync.Mutex
	alarmo    string // Alarmo's last state
	lastRearm time.Time
	expectOff int   // "off" reports our own disarming will bring
	night     *bool // the mode we armed the siren for, nil if unknown

	synchronous bool // tests only
	queue       chan func()
	startOnce   sync.Once
}

func newSirenController(ctx context.Context, siren alarmoSiren, logger *slog.Logger) *sirenController {
	return &sirenController{siren: siren, logger: logger, ctx: ctx, queue: make(chan func(), 16)}
}

// nightStates are Alarmo's armed states where night-allowed sensors are
// left out; the others watch every sensor.
var nightStates = map[string]bool{"armed_night": true, "armed_home": true, "armed_custom_bypass": true}

func armedState(state string) bool {
	switch state {
	case "armed_away", "armed_vacation", "armed_night", "armed_home", "armed_custom_bypass":
		return true
	}
	return false
}

// Handle follows a change of Alarmo's state.
func (s *sirenController) Handle(newState, prevState string) {
	if newState == prevState {
		return
	}
	s.mu.Lock()
	s.alarmo = newState
	s.mu.Unlock()

	s.run(func() {
		if !s.siren.Present() {
			_ = s.siren.Disarm() // siren_sounds none: keep it quiet
			return
		}
		var err error
		switch {
		case newState == "disarmed":
			err = s.siren.Disarm()
		case newState == "arming":
			err = s.siren.Arm(false, true)
		case armedState(newState):
			night := nightStates[newState]
			if s.keepArmed(night) {
				return
			}
			// An armed siren takes no new arming: off first (silent).
			s.mu.Lock()
			s.expectOff++
			s.night = &night
			s.mu.Unlock()
			if err = s.siren.Disarm(); err == nil {
				err = s.siren.Arm(night, false)
			}
		case newState == "pending":
			err = s.siren.EntryDelay()
		case newState == "triggered":
			switch s.siren.State() {
			case "armed", "entry_delay", "alert", "alert_over":
				err = s.siren.Alert()
			default: // not armed natively: the test sound at full power
				err = s.siren.Wail()
			}
		}
		if err != nil {
			s.logger.Error("siren: following alarmo failed", "state", newState, "error", err)
		}
	})
}

// keepArmed tells whether the siren already holds the armed mode: armed for
// it, or for a mode not known (openqiarad restarted), or in the middle of
// an alarm it saw while openqiarad was down, which re-arming would erase.
func (s *sirenController) keepArmed(night bool) bool {
	switch s.siren.State() {
	case "entry_delay", "alert", "alert_over":
		return true
	case "armed":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.night == nil {
			s.night = &night
			return true
		}
		return *s.night == night
	}
	return false
}

// SirenState reconciles the siren with Alarmo: armed while Alarmo is
// disarmed, it is disarmed; off while Alarmo is armed (it rebooted), it is
// armed again, at once and not more than every rearmEvery.
func (s *sirenController) SirenState(state string) {
	s.mu.Lock()
	alarmo := s.alarmo
	if state == "off" && s.expectOff > 0 {
		s.expectOff-- // our own disarming, before an arming
		s.mu.Unlock()
		return
	}
	rearm := alarmo != "disarmed" && alarmo != "" && state == "off" && time.Since(s.lastRearm) > rearmEvery
	if rearm {
		s.lastRearm = time.Now()
	}
	s.mu.Unlock()

	switch {
	case (alarmo == "disarmed") && state != "off" && state != "test" && state != "":
		s.run(func() {
			s.logger.Warn("siren: armed while alarmo is disarmed, disarming it", "siren", state)
			_ = s.siren.Disarm()
		})
	case rearm && s.siren.Present():
		s.run(func() {
			s.logger.Warn("siren: off while alarmo is armed, arming it again", "alarmo", alarmo)
			_ = s.siren.Arm(nightStates[alarmo], false)
		})
	}
}

// rearmEvery spaces out the re-arming of a siren that keeps refusing.
const rearmEvery = 30 * time.Second

// run runs fn in the siren's command queue, or inline in tests.
func (s *sirenController) run(fn func()) {
	if s.synchronous {
		fn()
		return
	}
	s.startOnce.Do(func() {
		go func() {
			for {
				select {
				case fn := <-s.queue:
					fn()
				case <-s.ctx.Done():
					return
				}
			}
		}()
	})
	select {
	case s.queue <- fn:
	case <-s.ctx.Done():
	}
}
