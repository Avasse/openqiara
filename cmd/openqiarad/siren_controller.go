package main

import (
	"context"
	"log/slog"
	"sync"
)

// alarmoSiren is what the mirror of Alarmo asks of the siren
// (alarm.SirenDriver).
type alarmoSiren interface {
	Arm(night, delayed bool)
	Disarm()
	EntryDelay()
	Alert()
}

// sirenController mirrors Alarmo's state on the siren, in alarmo mode:
// Alarmo decides and counts, the siren follows, armed natively so that it
// keeps the alarm by itself if the gateway goes. Its exit and entry delays
// are openqiara's (config): set them to Alarmo's, they only matter when
// the gateway is gone. The driver keeps the siren in the state asked for.
//
// Alarmo's states arrive on MQTT's goroutines: their commands go through
// one queue, in order. In tests, synchronous runs them inline.
type sirenController struct {
	siren  alarmoSiren
	logger *slog.Logger
	ctx    context.Context

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

// Handle follows a change of Alarmo's state. Alarmo's exit delay does not
// say the mode to come: the siren is armed for away, and again for the
// mode once Alarmo is armed.
func (s *sirenController) Handle(newState, prevState string) {
	if newState == prevState {
		return
	}
	s.run(func() {
		switch {
		case newState == "disarmed":
			s.siren.Disarm()
		case newState == "arming":
			s.siren.Arm(false, true)
		case armedState(newState):
			s.siren.Arm(nightStates[newState], false)
		case newState == "pending":
			s.siren.EntryDelay()
		case newState == "triggered":
			s.siren.Alert()
		}
	})
}

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
