// Package alarm implements the standalone alarm.
//
// The siren keeps the alarm, as it did under fbxhome: armed, it counts the
// exit and entry delays, plays their beeps and wails, and it hears the
// sensors itself when the gateway is gone. The engine arms and disarms it,
// relays it the sensors' alarms, and takes its state from what the siren
// reports. A relayed alarm the siren does not answer within relayTimeout
// sets the alarm off here. Without a siren (or with siren_sounds none),
// the engine counts the delays itself.
//
// The engine also keeps what the siren does not know: the mode (away or
// night), when it was armed and which sensor set it off. That and the
// state are persisted across reboots; the siren's next report reconciles
// them.
package alarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// State represents the current alarm state.
type State string

const (
	StateDisarmed   State = "disarmed"
	StateArming     State = "arming" // exit delay, counted by the siren
	StateArmedNight State = "armed_night"
	StateArmedAway  State = "armed_away"
	StatePending    State = "pending"   // entry delay, counted by the siren
	StateTriggered  State = "triggered" // set off, until disarmed
)

// Default delays, shown to the user while the siren counts them.
const (
	DefaultArmingDelay  = 60 * time.Second
	DefaultPendingDelay = 60 * time.Second
)

// rearmEvery spaces out the re-arming of a siren found off while the alarm
// is armed (it rebooted): a siren that keeps refusing does not loop.
const rearmEvery = 30 * time.Second

// relayTimeout is how long a relayed alarm waits for the siren's report
// before the alarm goes off without it (a mute or unreachable siren). A
// variable for the tests.
var relayTimeout = 10 * time.Second

// persistedState is the on-disk representation.
type persistedState struct {
	State       State `json:"state"`
	Mode        State `json:"mode,omitempty"` // armed_away or armed_night, set while not disarmed
	ArmedAt     int64 `json:"armed_at,omitempty"`
	TriggeredBy int   `json:"triggered_by,omitempty"`
	// PreviousState is the old name of Mode, read for files written before.
	PreviousState State `json:"previous_state,omitempty"`
}

// Snapshot is an immutable view of the engine state, returned to callers.
type Snapshot struct {
	State         State `json:"state"`
	ArmedAt       int64 `json:"armed_at,omitempty"`
	TriggeredBy   int   `json:"triggered_by,omitempty"`
	PreviousState State `json:"previous_state,omitempty"` // the armed mode
	// TimerRemaining is the seconds left in the exit or entry delay, as
	// the siren should count them; 0 otherwise.
	TimerRemaining int `json:"timer_remaining,omitempty"`
}

// SensorConfig tells the engine how a given sensor behaves in each mode.
type SensorConfig struct {
	// NightAllowed: the sensor is ignored when armed for the night.
	NightAllowed bool
	// Instant: the sensor sets the alarm off with no entry delay (a
	// window: nobody comes in through it).
	Instant bool
}

// ConfigProvider returns per-sensor configuration at runtime.
type ConfigProvider func(sensorID int) SensorConfig

// StateChangeCallback is invoked whenever the engine transitions to a new state.
// It is called under the engine's lock, so callbacks must not call back into the engine.
type StateChangeCallback func(snap Snapshot)

// Siren is the paired siren, as the engine drives it. Its state comes back
// through HandleSirenState.
type Siren interface {
	// Present tells whether there is a siren to keep the alarm.
	Present() bool
	// Arm arms it for the mode, after the exit delay or at once.
	Arm(night, delayed bool) error
	// Relay hands it a sensor's alarm: it starts the entry delay or the
	// alert by itself.
	Relay(sensorID int) error
	// Disarm disarms it and stops its sound, whether it keeps the alarm or
	// not (siren_sounds none): it must never stay armed.
	Disarm() error
	// Wail plays its sound, when the alarm goes off without it.
	Wail() error
}

// SirenState is a state the siren reports.
type SirenState string

const (
	SirenOff        SirenState = "off"
	SirenTest       SirenState = "test"
	SirenExitDelay  SirenState = "exit_delay"
	SirenArmed      SirenState = "armed"
	SirenEntryDelay SirenState = "entry_delay"
	SirenAlert      SirenState = "alert"
	SirenAlertOver  SirenState = "alert_over" // alert ended, still set off
)

// Engine is the alarm.
type Engine struct {
	mu        sync.Mutex
	state     State
	mode      State // armed_away or armed_night while not disarmed
	armedAt   int64
	trigBy    int
	deadline  time.Time // end of the delay in progress, for display
	lastRearm time.Time
	sirenSeen time.Time // last report of the siren
	expectOff int       // "off" reports our own disarming will bring
	timerGen  int       // bumped by every transition: stale timers do nothing

	armingDelay  time.Duration
	pendingDelay time.Duration

	// lastAlarm is whether each sensor is in alarm (door open, motion), so
	// that a sensor still in alarm when the exit delay ends sets it off.
	lastAlarm map[int]bool

	siren     Siren
	path      string
	configFor ConfigProvider
	onChange  StateChangeCallback
	logger    *slog.Logger
}

// New creates the engine. siren may be nil: no siren.
func New(path string, siren Siren, configFor ConfigProvider, onChange StateChangeCallback, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	if configFor == nil {
		configFor = func(int) SensorConfig { return SensorConfig{} }
	}
	return &Engine{
		state:        StateDisarmed,
		siren:        siren,
		path:         path,
		configFor:    configFor,
		onChange:     onChange,
		logger:       logger,
		lastAlarm:    make(map[int]bool),
		armingDelay:  DefaultArmingDelay,
		pendingDelay: DefaultPendingDelay,
	}
}

// SetTimings sets the delays shown while the siren counts them; the siren
// gets them at its next arming. 0 keeps the current value.
func (e *Engine) SetTimings(arming, pending time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if arming > 0 {
		e.armingDelay = arming
	}
	if pending > 0 {
		e.pendingDelay = pending
	}
}

// Load restores the engine state from disk, as it was: the siren's next
// report reconciles it. A missing file leaves the alarm disarmed.
func (e *Engine) Load() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	data, err := os.ReadFile(e.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("alarm: read state: %w", err)
	}
	var p persistedState
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("alarm: parse state: %w", err)
	}
	mode := p.Mode
	if mode == "" {
		mode = p.PreviousState
	}
	if mode != StateArmedNight {
		mode = StateArmedAway
	}
	switch p.State {
	case StateArming, StateArmedAway, StateArmedNight, StatePending, StateTriggered:
		e.state, e.mode, e.armedAt, e.trigBy = p.State, mode, p.ArmedAt, p.TriggeredBy
	default:
		e.state = StateDisarmed
	}
	if !e.hasSiren() && (e.state == StateArming || e.state == StatePending) {
		e.state = mode // its timer died with the process
	}
	e.logger.Info("alarm: state loaded", "state", e.state, "mode", e.mode)
	return nil
}

// Snapshot returns the current state.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Source identifie l'origine d'une commande utilisateur, pour adapter le
// comportement (notamment le délai d'armement).
type Source string

const (
	// SourceLocal : commande émise depuis un dispositif physique sur place
	// (KPD). L'utilisateur a besoin du délai d'armement pour quitter les
	// lieux avant que la surveillance ne démarre.
	SourceLocal Source = "local"
	// SourceRemote : commande émise à distance (HK, web UI, MQTT/HA).
	// L'utilisateur n'est pas sur place — surveiller immédiatement, sans
	// délai d'armement qui laisserait un intrus s'échapper.
	SourceRemote Source = "remote"
)

// HandleCommand processes a user command: "arm_away", "arm_night" or
// "disarm". Only a local command (the keypad) gets the exit delay.
func (e *Engine) HandleCommand(cmd string, source Source) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch cmd {
	case "disarm":
		if e.state == StateDisarmed {
			return
		}
		e.transitionLocked(StateDisarmed, "user disarm")
		if e.siren != nil {
			e.sirenErr("disarm", e.siren.Disarm())
		}

	case "arm_away", "arm_night":
		mode := StateArmedAway
		if cmd == "arm_night" {
			mode = StateArmedNight
		}
		switch e.state {
		case StateDisarmed:
			e.armLocked(mode, source == SourceLocal)
		case StateArmedAway, StateArmedNight:
			if e.state == mode {
				return
			}
			// The siren takes the mode's sensors only when armed from off.
			e.mode = mode
			e.transitionLocked(mode, "switch mode")
			if e.hasSiren() {
				e.expectOff++
				e.sirenErr("disarm", e.siren.Disarm())
				e.sirenErr("arm", e.siren.Arm(mode == StateArmedNight, false))
			}
		default:
			// Arming, pending or triggered: disarm first.
		}

	default:
		e.logger.Warn("alarm: unknown command", "cmd", cmd)
	}
}

// armLocked arms from disarmed, with or without the exit delay.
func (e *Engine) armLocked(mode State, delayed bool) {
	e.mode, e.trigBy = mode, 0
	if !e.hasSiren() {
		if !delayed {
			e.armedLocked("armed, no siren")
			return
		}
		e.transitionLocked(StateArming, "arming, no siren")
		e.afterLocked(e.armingDelay, func() {
			if e.state == StateArming {
				e.armedLocked("exit delay over")
			}
		})
		return
	}
	// Armed before anything is relayed to it: an unarmed siren ignores it.
	e.sirenErr("arm", e.siren.Arm(mode == StateArmedNight, delayed))
	if delayed {
		e.transitionLocked(StateArming, "arming")
	} else {
		e.armedLocked("armed")
	}
}

// armedLocked enters the armed mode, then sets the alarm off for a sensor
// still in alarm.
func (e *Engine) armedLocked(reason string) {
	e.armedAt = time.Now().Unix()
	e.transitionLocked(e.mode, reason)
	for id, inAlarm := range e.lastAlarm {
		if inAlarm && e.watchedLocked(id) {
			e.logger.Info("alarm: sensor still in alarm once armed", "sensor_id", id)
			e.sensorAlarmLocked(id)
			return
		}
	}
}

// HandleSensorEvent processes a sensor state change. inAlarm is whether
// the sensor is in alarm (door open, motion).
func (e *Engine) HandleSensorEvent(sensorID int, sensorType string, inAlarm bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastAlarm[sensorID] = inAlarm
	if sensorType == "KPD" || !inAlarm || !e.watchedLocked(sensorID) {
		return
	}
	switch e.state {
	case StateArmedAway, StateArmedNight, StatePending, StateTriggered:
		e.sensorAlarmLocked(sensorID)
	}
}

// watchedLocked tells whether a sensor sets the alarm off in its mode.
func (e *Engine) watchedLocked(sensorID int) bool {
	return !(e.mode == StateArmedNight && e.configFor(sensorID).NightAllowed)
}

// sensorAlarmLocked hands a sensor's alarm to the siren, which decides and
// reports; a siren that stays mute leaves the alarm to go off here.
// Without a siren, the engine runs the entry delay itself.
func (e *Engine) sensorAlarmLocked(sensorID int) {
	e.logger.Info("alarm: sensor in alarm", "sensor_id", sensorID, "state", e.state)
	armed := e.state == StateArmedAway || e.state == StateArmedNight
	if armed {
		e.trigBy = sensorID
	}
	if !e.hasSiren() {
		e.localAlarmLocked(armed, e.configFor(sensorID).Instant)
		return
	}
	e.sirenErr("relay", e.siren.Relay(sensorID))
	if armed {
		relayed := time.Now()
		e.afterLocked(relayTimeout, func() {
			if e.sirenSeen.Before(relayed) && (e.state == StateArmedAway || e.state == StateArmedNight) {
				e.logger.Error("alarm: the siren did not answer a sensor's alarm, going off without it", "sensor_id", sensorID)
				e.transitionLocked(StateTriggered, "siren mute")
				e.sirenErr("wail", e.siren.Wail())
			}
		})
	}
}

// localAlarmLocked runs the entry delay and the alert without a siren.
func (e *Engine) localAlarmLocked(armed, instant bool) {
	switch {
	case e.state == StateTriggered:
	case instant:
		e.transitionLocked(StateTriggered, "sensor, no siren")
	case armed:
		e.transitionLocked(StatePending, "entry delay, no siren")
		e.afterLocked(e.pendingDelay, func() {
			if e.state == StatePending {
				e.transitionLocked(StateTriggered, "entry delay over")
			}
		})
	}
}

// afterLocked runs fn under the lock after d, unless the state changed in
// the meantime.
func (e *Engine) afterLocked(d time.Duration, fn func()) {
	gen := e.timerGen
	time.AfterFunc(d, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.timerGen == gen {
			fn()
		}
	})
}

// HandleSirenState takes the siren's report as the alarm's state. A siren
// found armed while the alarm is disarmed is disarmed; one found off while
// the alarm is armed (it rebooted) is armed again, at once.
func (e *Engine) HandleSirenState(s SirenState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasSiren() {
		return
	}
	e.sirenSeen = time.Now()

	var next State
	switch s {
	case SirenOff:
		if e.expectOff > 0 {
			e.expectOff-- // our own disarming, on a change of mode
			return
		}
		if e.state != StateDisarmed && time.Since(e.lastRearm) > rearmEvery {
			e.lastRearm = time.Now()
			e.logger.Warn("alarm: siren found off while armed, arming it again", "state", e.state)
			e.sirenErr("arm", e.siren.Arm(e.mode == StateArmedNight, false))
		}
		return
	case SirenExitDelay:
		next = StateArming
	case SirenArmed:
		next = e.mode
	case SirenEntryDelay:
		next = StatePending
	case SirenAlert, SirenAlertOver:
		next = StateTriggered
	default:
		return // the test sound
	}
	if e.state == StateDisarmed {
		e.logger.Warn("alarm: siren found armed while disarmed, disarming it", "siren", s)
		e.sirenErr("disarm", e.siren.Disarm())
		return
	}
	if next == e.state {
		return
	}
	if e.state == StateArming && next == e.mode {
		e.armedLocked("exit delay over")
		return
	}
	e.transitionLocked(next, "siren "+string(s))
}

func (e *Engine) hasSiren() bool {
	return e.siren != nil && e.siren.Present()
}

func (e *Engine) sirenErr(what string, err error) {
	if err != nil {
		e.logger.Error("alarm: siren "+what+" failed", "error", err)
	}
}

// transitionLocked changes state, persists, notifies.
func (e *Engine) transitionLocked(newState State, reason string) {
	old := e.state
	e.state = newState
	e.deadline = time.Time{}
	e.timerGen++
	switch newState {
	case StateDisarmed:
		e.armedAt, e.trigBy, e.mode = 0, 0, ""
		e.lastAlarm = make(map[int]bool)
	case StateArming:
		e.deadline = time.Now().Add(e.armingDelay)
	case StatePending:
		e.deadline = time.Now().Add(e.pendingDelay)
	}
	e.logger.Info("alarm: state transition", "from", old, "to", newState, "reason", reason)
	if e.onChange != nil {
		e.onChange(e.snapshotLocked())
	}
	e.saveLocked()
}

func (e *Engine) snapshotLocked() Snapshot {
	snap := Snapshot{State: e.state, ArmedAt: e.armedAt, TriggeredBy: e.trigBy, PreviousState: e.mode}
	if left := time.Until(e.deadline); !e.deadline.IsZero() && left > 0 {
		snap.TimerRemaining = int(left.Seconds())
	}
	return snap
}

func (e *Engine) saveLocked() {
	p := persistedState{State: e.state, Mode: e.mode, ArmedAt: e.armedAt, TriggeredBy: e.trigBy}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		e.logger.Error("alarm: marshal state", "error", err)
		return
	}
	if err := os.WriteFile(e.path, data, 0644); err != nil {
		e.logger.Error("alarm: write state", "error", err)
	}
}
