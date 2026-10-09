package alarm

import (
	"log/slog"
	"sync"
	"time"
)

// SirenRadio is the paired siren as the radio drives it
// (cmd/openqiarad nativeSiren).
type SirenRadio interface {
	// Present tells whether a siren keeps the alarm (siren_sounds none
	// leaves it out).
	Present() bool
	// Arm arms it for the mode; it takes that only when off.
	Arm(night, delayed bool) error
	EntryDelay() error
	Alert() error
	// Wail plays the test sound at full power: only an unarmed siren does.
	Wail() error
	Disarm() error
	RequestState() error
}

// rearmEvery spaces out the re-arming of a siren found off while it should
// be armed, so that one that keeps refusing does not loop.
const rearmEvery = 5 * time.Second

// SirenDriver keeps the siren in the state the alarm wants, whoever the
// alarm's master is (the standalone engine or Alarmo). The siren answers
// out of order, reboots, can be torn off: rather than guess which report
// answers which command, the driver holds the wanted state and brings the
// siren back to it on every report. Until a master says what it wants
// (just after a start), the driver leaves the siren as it is: it may hold
// an alarm it raised while the camera was down.
type SirenDriver struct {
	radio  SirenRadio
	logger *slog.Logger

	mu        sync.Mutex
	want      sirenWant
	night     bool        // the mode wanted, when armed
	armed     *bool       // the mode the siren was last armed with, nil if not known
	last      SirenState  // its last report, "" before the first
	lastRearm time.Time
}

type sirenWant int

const (
	wantUnknown sirenWant = iota
	wantOff
	wantArmed
)

// NewSirenDriver returns a driver that does nothing until told what to want.
func NewSirenDriver(radio SirenRadio, logger *slog.Logger) *SirenDriver {
	if logger == nil {
		logger = slog.Default()
	}
	return &SirenDriver{radio: radio, logger: logger}
}

// Present tells whether a siren keeps the alarm.
func (d *SirenDriver) Present() bool { return d.radio.Present() }

// Arm wants the siren armed for the mode, after the exit delay or at once.
// A siren already in an alarm keeps it; one armed for another mode, or
// for a mode not known, is armed again (off first: it takes an arming only
// when off).
func (d *SirenDriver) Arm(night, delayed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.want, d.night = wantArmed, night
	if !d.radio.Present() {
		return
	}
	switch d.last {
	case "":
		d.err("state", d.radio.RequestState()) // armed on its report
	case SirenEntryDelay, SirenAlert, SirenAlertOver:
	case SirenArmed, SirenExitDelay:
		if d.armed != nil && *d.armed == night && (!delayed || d.last == SirenExitDelay) {
			return
		}
		d.err("disarm", d.radio.Disarm())
		d.armLocked(delayed)
	default:
		d.armLocked(delayed)
	}
}

func (d *SirenDriver) armLocked(delayed bool) {
	night := d.night
	d.armed = &night
	d.err("arm", d.radio.Arm(night, delayed))
}

// Disarm wants the siren off, and stops its sound. It is sent whether the
// siren keeps the alarm or not (siren_sounds none): never left armed.
func (d *SirenDriver) Disarm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.want, d.armed = wantOff, nil
	d.err("disarm", d.radio.Disarm())
}

// EntryDelay starts the siren's entry delay (an armed siren only).
func (d *SirenDriver) EntryDelay() {
	if d.radio.Present() {
		d.err("entry delay", d.radio.EntryDelay())
	}
}

// Alert sets the siren off: the alert if it is armed, else the test sound
// at full power. Both go, each taken only in its state: no guessing from a
// report that may be late.
func (d *SirenDriver) Alert() {
	if d.radio.Present() {
		d.err("alert", d.radio.Alert())
		d.err("wail", d.radio.Wail())
	}
}

// HandleReport takes a report of the siren and brings it back to the
// wanted state: armed while it should be off, disarmed; off while it
// should be armed (it rebooted, an arming was lost), armed again at once.
func (d *SirenDriver) HandleReport(s SirenState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last = s
	switch {
	case !d.radio.Present() && s != SirenOff && s != SirenTest:
		d.logger.Warn("siren: armed while left out of the alarm, disarming it", "siren", s)
		d.err("disarm", d.radio.Disarm())
	case !d.radio.Present():
	case d.want == wantOff && s != SirenOff && s != SirenTest:
		d.logger.Warn("siren: armed while it should be off, disarming it", "siren", s)
		d.err("disarm", d.radio.Disarm())
	case d.want == wantArmed && s == SirenOff && time.Since(d.lastRearm) > rearmEvery:
		d.lastRearm = time.Now()
		d.logger.Warn("siren: off while it should be armed, arming it", "night", d.night)
		d.armLocked(false)
	}
}

func (d *SirenDriver) err(what string, err error) {
	if err != nil {
		d.logger.Error("siren: "+what+" failed", "error", err)
	}
}
