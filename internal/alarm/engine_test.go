package alarm

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeSiren records what the engine asks of the siren.
type fakeSiren struct {
	absent bool
	calls  []string
}

func (f *fakeSiren) Present() bool { return !f.absent }
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
func (f *fakeSiren) Disarm() error      { f.calls = append(f.calls, "disarm"); return nil }
func (f *fakeSiren) Wail() error        { f.calls = append(f.calls, "wail"); return nil }

// take returns the calls since the last take.
func (f *fakeSiren) take() []string {
	c := f.calls
	f.calls = nil
	return c
}

func newTestEngine(t *testing.T, cfg map[int]SensorConfig) (*Engine, *fakeSiren) {
	t.Helper()
	siren := &fakeSiren{}
	e := New(filepath.Join(t.TempDir(), "alarm.json"), siren, func(id int) SensorConfig { return cfg[id] }, nil, nil)
	return e, siren
}

func expect(t *testing.T, e *Engine, siren *fakeSiren, state State, calls ...string) {
	t.Helper()
	if s := e.Snapshot().State; s != state {
		t.Errorf("state = %q, want %q", s, state)
	}
	if got := siren.take(); !slices.Equal(got, calls) {
		t.Errorf("siren calls = %q, want %q", got, calls)
	}
}

// TestKeypadArmFollowsTheSiren: the keypad arms with the exit delay, which
// the siren counts; its reports drive the state, down to the alert.
func TestKeypadArmFollowsTheSiren(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	expect(t, e, siren, StateArming, "arm delayed")
	if e.Snapshot().TimerRemaining == 0 {
		t.Error("no exit delay shown")
	}
	e.HandleSirenState(SirenExitDelay)
	expect(t, e, siren, StateArming)
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateArmedAway)

	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StateArmedAway, "entry delay")
	e.HandleSirenState(SirenEntryDelay)
	expect(t, e, siren, StatePending)
	e.HandleSirenState(SirenAlert)
	expect(t, e, siren, StateTriggered)
	e.HandleSirenState(SirenAlertOver)
	expect(t, e, siren, StateTriggered)
	if by := e.Snapshot().TriggeredBy; by != 23 {
		t.Errorf("triggered by %d, want 23", by)
	}

	e.HandleCommand("disarm", SourceLocal)
	expect(t, e, siren, StateDisarmed, "disarm")
	e.HandleSirenState(SirenOff)
	expect(t, e, siren, StateDisarmed)
}

// TestSirenCommandPerSensor: a delayed sensor starts the entry delay, an
// instant one the alert; once the alarm is under way, any sensor sets it
// off again, except a delayed one during the entry delay.
func TestSirenCommandPerSensor(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{30: {Instant: true}})
	e.HandleCommand("arm_away", SourceRemote)
	siren.take()
	e.HandleSensorEvent(30, "DWS", true)
	expect(t, e, siren, StateArmedAway, "alert")

	e.HandleSirenState(SirenEntryDelay)
	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StatePending)
	e.HandleSensorEvent(30, "DWS", false)
	e.HandleSensorEvent(30, "DWS", true)
	expect(t, e, siren, StatePending, "alert")

	e.HandleSirenState(SirenAlertOver)
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StateTriggered, "alert")
}

// TestRemoteArmIsImmediate: Home Assistant and HomeKit arm with no exit
// delay, and a sensor already in alarm is relayed once armed.
func TestRemoteArmIsImmediate(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleCommand("arm_night", SourceRemote)
	expect(t, e, siren, StateArmedNight, "arm night", "entry delay")
}

// TestSensorInAlarmWhenTheExitDelayEnds: a door left open sets the alarm
// off once the exit delay is over, not before.
func TestSensorInAlarmWhenTheExitDelayEnds(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StateArming, "arm delayed")
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateArmedAway, "entry delay")
}

// TestWhatIsNotRelayed: the keypad, a sensor back to rest, anything while
// disarmed, and a night-allowed sensor when armed for the night.
func TestWhatIsNotRelayed(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{17: {NightAllowed: true}})
	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StateDisarmed)
	e.HandleSensorEvent(23, "DWS", false)

	e.HandleCommand("arm_night", SourceRemote)
	siren.take()
	e.HandleSensorEvent(14, "KPD", true)
	e.HandleSensorEvent(23, "DWS", false)
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StateArmedNight)

	e.HandleCommand("arm_away", SourceRemote)
	expect(t, e, siren, StateArmedAway, "disarm", "arm")
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StateArmedAway, "entry delay")
}

// TestModeSwitchOffNotARestart: the "off" our own disarming brings on a
// change of mode is not a rebooted siren; a real one later is re-armed.
func TestModeSwitchOffNotARestart(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleCommand("arm_night", SourceRemote)
	siren.take()
	e.HandleSirenState(SirenOff)
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateArmedNight)
	e.HandleSirenState(SirenOff)
	expect(t, e, siren, StateArmedNight, "arm night")
}

// TestSirenReconciled: a siren armed while the alarm is disarmed is
// disarmed; one found off while armed (it rebooted) is armed again, once.
func TestSirenReconciled(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateDisarmed, "disarm")

	e.HandleCommand("arm_away", SourceRemote)
	siren.take()
	e.HandleSirenState(SirenOff)
	expect(t, e, siren, StateArmedAway, "arm")
	e.HandleSirenState(SirenOff)
	expect(t, e, siren, StateArmedAway)
	e.HandleSirenState(SirenTest)
	expect(t, e, siren, StateArmedAway)
}

// TestNoSiren: without a siren (or with siren_sounds none), the engine
// counts the delays itself; a siren that exists is still disarmed.
func TestNoSiren(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{30: {Instant: true}})
	siren.absent = true
	e.SetTimings(20*time.Millisecond, 20*time.Millisecond)
	e.HandleCommand("arm_away", SourceLocal)
	expect(t, e, siren, StateArming)
	time.Sleep(60 * time.Millisecond)
	expect(t, e, siren, StateArmedAway)

	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StatePending)
	time.Sleep(60 * time.Millisecond)
	expect(t, e, siren, StateTriggered)
	e.HandleCommand("disarm", SourceLocal)
	expect(t, e, siren, StateDisarmed, "disarm")

	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(30, "DWS", true)
	expect(t, e, siren, StateTriggered)
	e.HandleCommand("disarm", SourceLocal)
	siren.take()

	// A disarm stops the entry delay's timer.
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleCommand("disarm", SourceLocal)
	time.Sleep(60 * time.Millisecond)
	expect(t, e, siren, StateDisarmed, "disarm")
}

// TestMuteSiren: a relayed alarm the siren does not answer sets the alarm
// off without it; an answer, whatever it is, leaves it to the siren.
func TestMuteSiren(t *testing.T) {
	defer func(d time.Duration) { relayTimeout = d }(relayTimeout)
	relayTimeout = 20 * time.Millisecond

	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	time.Sleep(60 * time.Millisecond)
	expect(t, e, siren, StateTriggered, "arm", "entry delay", "wail")

	e.HandleCommand("disarm", SourceLocal)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleSirenState(SirenArmed) // not in its masks: it stays armed
	time.Sleep(60 * time.Millisecond)
	expect(t, e, siren, StateArmedAway, "disarm", "arm", "entry delay")
}

// TestLoad: the state comes back as it was, the mode with it; the siren's
// next report reconciles it. Files from before keep their mode.
func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		file  string
		state State
		mode  State
	}{
		{`{"state":"arming","mode":"armed_night"}`, StateArming, StateArmedNight},
		{`{"state":"pending","previous_state":"armed_night"}`, StatePending, StateArmedNight},
		{`{"state":"armed_away","armed_at":12}`, StateArmedAway, StateArmedAway},
		{`{"state":"bogus"}`, StateDisarmed, ""},
	} {
		path := filepath.Join(t.TempDir(), "alarm.json")
		if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
			t.Fatal(err)
		}
		e := New(path, &fakeSiren{}, nil, nil, nil)
		if err := e.Load(); err != nil {
			t.Fatal(err)
		}
		if s := e.Snapshot(); s.State != tc.state || s.PreviousState != tc.mode {
			t.Errorf("%s: loaded %+v, want %s/%s", tc.file, s, tc.state, tc.mode)
		}
	}
}

// TestStatePersisted: what is saved loads back.
func TestStatePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarm.json")
	e := New(path, &fakeSiren{}, nil, nil, nil)
	e.HandleCommand("arm_night", SourceRemote)
	again := New(path, &fakeSiren{}, nil, nil, nil)
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if s := again.Snapshot(); s.State != StateArmedNight || s.ArmedAt == 0 {
		t.Errorf("loaded %+v, want armed_night with its arming time", s)
	}
}

// TestCallbackFiresOnTransition: every change of state is announced.
func TestCallbackFiresOnTransition(t *testing.T) {
	var got []State
	e := New(filepath.Join(t.TempDir(), "alarm.json"), &fakeSiren{}, nil,
		func(s Snapshot) { got = append(got, s.State) }, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSirenState(SirenArmed)
	e.HandleCommand("disarm", SourceLocal)
	if want := []State{StateArming, StateArmedAway, StateDisarmed}; !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}
