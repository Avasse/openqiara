package alarm

import (
	"slices"
	"testing"
)

// fakeRadio records what the driver sends the siren.
type fakeRadio struct {
	absent bool
	calls  []string
}

func (f *fakeRadio) Present() bool { return !f.absent }
func (f *fakeRadio) Arm(night, delayed bool) error {
	call := "arm"
	if night {
		call += " night"
	}
	if delayed {
		call += " delayed"
	}
	f.calls = append(f.calls, call)
	return nil
}
func (f *fakeRadio) EntryDelay() error   { f.calls = append(f.calls, "entry delay"); return nil }
func (f *fakeRadio) Alert() error        { f.calls = append(f.calls, "alert"); return nil }
func (f *fakeRadio) Wail() error         { f.calls = append(f.calls, "wail"); return nil }
func (f *fakeRadio) Disarm() error       { f.calls = append(f.calls, "disarm"); return nil }
func (f *fakeRadio) RequestState() error { f.calls = append(f.calls, "state?"); return nil }

func sent(t *testing.T, r *fakeRadio, want ...string) {
	t.Helper()
	if !slices.Equal(r.calls, want) {
		t.Errorf("sent %q, want %q", r.calls, want)
	}
	r.calls = nil
}

// TestDriverLeavesTheSirenUntilTold: just after a start, a siren that
// holds an alarm (raised while the camera was down) is left as it is.
func TestDriverLeavesTheSirenUntilTold(t *testing.T) {
	r := &fakeRadio{}
	d := NewSirenDriver(r, nil)
	for _, s := range []SirenState{SirenAlertOver, SirenOff, SirenArmed} {
		d.HandleReport(s)
	}
	sent(t, r)
}

// TestDriverArming: armed from off; an arming while its state is not known
// waits for its report, exit delay kept; a siren holding an alarm keeps it; one armed for
// another mode, or for a mode not known, is armed again.
func TestDriverArming(t *testing.T) {
	r := &fakeRadio{}
	d := NewSirenDriver(r, nil)
	d.Arm(false, true)
	sent(t, r, "state?")
	d.HandleReport(SirenOff)
	sent(t, r, "arm delayed") // the arming that waited keeps its exit delay
	d.HandleReport(SirenArmed)

	d.HandleReport(SirenArmed)
	d.Arm(false, false)
	sent(t, r)
	d.Arm(true, false)
	sent(t, r, "disarm", "arm night")

	d.HandleReport(SirenAlertOver)
	d.Arm(false, false)
	sent(t, r)

	// An immediate arming ends an exit delay of the same mode.
	d.Disarm()
	d.Arm(false, true)
	sent(t, r, "disarm", "arm delayed")
	d.Arm(false, true)
	sent(t, r)
	d.Arm(false, false)
	sent(t, r, "disarm", "arm")

	r2 := &fakeRadio{}
	d2 := NewSirenDriver(r2, nil)
	d2.HandleReport(SirenArmed) // armed before the start, masks not known
	d2.Arm(false, false)
	sent(t, r2, "disarm", "arm")
}

// TestDriverReconciles: off while wanted armed (rebooted, an arming lost),
// armed again, not more than every rearmEvery; armed while wanted off,
// disarmed. A late report of our own disarming re-arms nothing it should
// not: the siren takes an arming only when off, and is armed already.
func TestDriverReconciles(t *testing.T) {
	r := &fakeRadio{}
	d := NewSirenDriver(r, nil)
	d.HandleReport(SirenOff)
	d.Arm(true, false)
	sent(t, r, "arm night")
	d.HandleReport(SirenOff)
	sent(t, r, "arm night")
	d.HandleReport(SirenOff)
	sent(t, r)

	d.Disarm()
	sent(t, r, "disarm")
	d.HandleReport(SirenArmed)
	sent(t, r, "disarm")
	d.HandleReport(SirenTest)
	sent(t, r)
}

// TestDriverAlert: the alert and the full-power test sound both go, each
// taken only in its state; nothing goes without a siren in the alarm, but
// a disarm always does, and a siren found armed then is disarmed.
func TestDriverAlert(t *testing.T) {
	r := &fakeRadio{}
	d := NewSirenDriver(r, nil)
	d.Alert()
	sent(t, r, "alert", "wail")

	r.absent = true
	d.Arm(false, false)
	d.EntryDelay()
	d.Alert()
	sent(t, r)
	d.Disarm()
	sent(t, r, "disarm")
	d.HandleReport(SirenArmed)
	sent(t, r, "disarm")
}
