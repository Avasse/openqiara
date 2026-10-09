package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
)

// fakeSiren records what the mirror of Alarmo asks of the siren.
type fakeSiren struct{ calls []string }

func (f *fakeSiren) Arm(night, delayed bool) {
	call := "arm"
	if night {
		call += " night"
	}
	if delayed {
		call += " delayed"
	}
	f.calls = append(f.calls, call)
}
func (f *fakeSiren) Disarm()     { f.calls = append(f.calls, "disarm") }
func (f *fakeSiren) EntryDelay() { f.calls = append(f.calls, "entry delay") }
func (f *fakeSiren) Alert()      { f.calls = append(f.calls, "alert") }

// TestMirrorFollowsAlarmo: each Alarmo state is one command to the siren;
// Alarmo's exit delay arms it for away, then for the mode once armed.
func TestMirrorFollowsAlarmo(t *testing.T) {
	siren := &fakeSiren{}
	sc := newSirenController(context.Background(), siren, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc.synchronous = true

	for _, step := range [][2]string{
		{"disarmed", ""}, {"arming", "disarmed"}, {"armed_night", "arming"}, {"armed_night", "armed_night"},
		{"pending", "armed_night"}, {"triggered", "pending"}, {"disarmed", "triggered"},
		{"armed_away", "disarmed"}, {"armed_home", "armed_away"},
	} {
		sc.Handle(step[0], step[1])
	}
	want := []string{"disarm", "arm delayed", "arm night", "entry delay", "alert", "disarm", "arm", "arm night"}
	if !slices.Equal(siren.calls, want) {
		t.Errorf("siren calls = %q, want %q", siren.calls, want)
	}
}
