package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
)

// fakeSiren records what the mirror of Alarmo asks of the siren.
type fakeSiren struct {
	absent bool
	state  string
	calls  []string
}

func (f *fakeSiren) Present() bool { return !f.absent }
func (f *fakeSiren) State() string { return f.state }
func (f *fakeSiren) Arm(night, delayed bool) error {
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
func (f *fakeSiren) EntryDelay() error { f.calls = append(f.calls, "entry delay"); return nil }
func (f *fakeSiren) Alert() error      { f.calls = append(f.calls, "alert"); return nil }
func (f *fakeSiren) Wail() error       { f.calls = append(f.calls, "wail"); return nil }
func (f *fakeSiren) Disarm() error     { f.calls = append(f.calls, "disarm"); return nil }

func (f *fakeSiren) take() []string {
	c := f.calls
	f.calls = nil
	return c
}

func newTestController(siren *fakeSiren) *sirenController {
	sc := newSirenController(context.Background(), siren, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc.synchronous = true
	return sc
}

func expectCalls(t *testing.T, siren *fakeSiren, want ...string) {
	t.Helper()
	if got := siren.take(); !slices.Equal(got, want) {
		t.Errorf("siren calls = %q, want %q", got, want)
	}
}

// TestMirrorFollowsAlarmo: each Alarmo state is a native command; an armed
// siren is disarmed before it takes the mode's sensors.
func TestMirrorFollowsAlarmo(t *testing.T) {
	siren := &fakeSiren{}
	sc := newTestController(siren)

	sc.Handle("arming", "disarmed")
	expectCalls(t, siren, "arm delayed")
	sc.Handle("armed_away", "arming")
	expectCalls(t, siren, "disarm", "arm")
	sc.Handle("armed_night", "armed_away")
	expectCalls(t, siren, "disarm", "arm night")
	sc.Handle("pending", "armed_night")
	expectCalls(t, siren, "entry delay")
	siren.state = "entry_delay"
	sc.Handle("triggered", "pending")
	expectCalls(t, siren, "alert")
	sc.Handle("disarmed", "triggered")
	expectCalls(t, siren, "disarm")
	sc.Handle("disarmed", "disarmed")
	expectCalls(t, siren)
}

// TestTriggeredSirenNotArmed: a siren Alarmo did not arm natively plays
// the test sound at full power instead.
func TestTriggeredSirenNotArmed(t *testing.T) {
	siren := &fakeSiren{state: "off"}
	sc := newTestController(siren)
	sc.Handle("triggered", "disarmed")
	expectCalls(t, siren, "wail")
}

// TestSirenSoundsNone: siren_sounds none keeps the siren off whatever
// Alarmo does.
func TestSirenSoundsNone(t *testing.T) {
	siren := &fakeSiren{absent: true}
	sc := newTestController(siren)
	sc.Handle("armed_away", "disarmed")
	sc.Handle("triggered", "armed_away")
	expectCalls(t, siren, "disarm", "disarm")
}

// TestMirrorReconciled: a siren armed while Alarmo is disarmed is
// disarmed; off while Alarmo is armed (it rebooted), it is armed again,
// once.
func TestMirrorReconciled(t *testing.T) {
	siren := &fakeSiren{}
	sc := newTestController(siren)
	sc.Handle("disarmed", "")
	siren.take()
	sc.SirenState("armed")
	expectCalls(t, siren, "disarm")
	sc.SirenState("off")
	expectCalls(t, siren)

	sc.Handle("armed_night", "disarmed")
	siren.take()
	sc.SirenState("off") // our own disarming, before the arming
	sc.SirenState("armed")
	expectCalls(t, siren)
	sc.SirenState("off") // it rebooted
	expectCalls(t, siren, "arm night")
	sc.SirenState("off")
	expectCalls(t, siren)
}

// TestMirrorKeepsWhatTheSirenHolds: Alarmo's retained state after a
// restart does not re-arm a siren that already holds it, nor erase an
// alarm it saw meanwhile.
func TestMirrorKeepsWhatTheSirenHolds(t *testing.T) {
	for _, state := range []string{"armed", "entry_delay", "alert", "alert_over"} {
		siren := &fakeSiren{state: state}
		sc := newTestController(siren)
		sc.Handle("armed_away", "")
		expectCalls(t, siren)
	}
	siren := &fakeSiren{state: "armed"}
	sc := newTestController(siren)
	sc.Handle("armed_away", "")
	sc.Handle("armed_night", "armed_away")
	expectCalls(t, siren, "disarm", "arm night")
}
