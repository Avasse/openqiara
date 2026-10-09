package main

import (
	"context"
	"slices"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
)

// nativeSiren drives the paired siren for the alarm, the standalone engine
// (alarm.Siren) and the mirror of Alarmo alike: armed with the delays and
// the sensors of the config, the siren keeps the alarm by itself.
type nativeSiren struct {
	ctx   context.Context
	cam   camera.Client
	store *config.Store
}

// id is the siren of the alarm: the lowest id, the one whose address the
// sensors get in their config. 0 for none.
func (s nativeSiren) id() int {
	for _, se := range s.cam.CachedSensors() {
		if se.Type == "SRN" {
			return se.ID
		}
	}
	return 0
}

// State is what the siren last reported (camera.Sensor.SirenState).
func (s nativeSiren) State() string {
	id := s.id()
	for _, se := range s.cam.CachedSensors() {
		if se.ID == id {
			return se.SirenState
		}
	}
	return ""
}

// Present tells whether a siren keeps the alarm: siren_sounds none leaves
// it out.
func (s nativeSiren) Present() bool {
	return s.id() != 0 && s.store.Get().SirenSoundsMode() != "none"
}

func (s nativeSiren) Arm(night, delayed bool) error {
	return s.cam.ArmSiren(s.ctx, s.id(), s.arming(night, delayed))
}

func (s nativeSiren) Relay(sensorID int) error {
	return s.cam.RelaySensorAlarm(s.ctx, s.id(), sensorID)
}

func (s nativeSiren) Disarm() error {
	return s.cam.StopSiren(s.ctx, s.id())
}

// arming is the config as the siren takes it: the doors and motion sensors
// watched in the mode, those without instant getting the entry delay.
// siren_sounds alarm_only asks for quiet delays.
func (s nativeSiren) arming(night, delayed bool) camera.SirenArming {
	cfg := s.store.Get()
	a := camera.SirenArming{
		EntryDelay: cfg.PendingDelay(),
		Alert:      cfg.WailDuration(),
		Quiet:      cfg.SirenSoundsMode() == "alarm_only",
	}
	if delayed {
		a.ExitDelay = cfg.ArmingDelay()
	}
	for _, se := range cfg.Sensors {
		if !slices.Contains([]string{"DWS", "PIR"}, se.Type) || night && se.NightAllowed {
			continue
		}
		a.Active = append(a.Active, se.ID)
		if !se.Instant {
			a.Delayed = append(a.Delayed, se.ID)
		}
	}
	return a
}

func (s nativeSiren) EntryDelay() error { return s.cam.SirenEntryDelay(s.ctx, s.id()) }

func (s nativeSiren) Alert() error { return s.cam.SirenAlert(s.ctx, s.id()) }

// Wail plays the test sound at full power for the configured duration: the
// alert of a siren that is not armed.
func (s nativeSiren) Wail() error {
	return s.cam.TriggerSirenAlarm(s.ctx, s.id(), s.store.Get().WailDuration())
}
