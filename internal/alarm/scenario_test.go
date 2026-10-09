package alarm

import (
	"path/filepath"
	"testing"
	"time"
)

// Non-regression scenarios: the alarm, the siren driver and a simulated
// siren together, replaying what the hardware sessions of 2026-10-09 went
// through and what the grills found. The siren answers out of order in all
// of them.

// The sensors of the tests: a door (delayed), a motion sensor left out at
// night, a window (instant).
var simSensors = map[int]SensorConfig{23: {}, 17: {NightAllowed: true}, 30: {Instant: true}}

var simTypes = map[int]string{23: "DWS", 17: "PIR", 30: "DWS"}

type rig struct {
	sim    *simSiren
	driver *SirenDriver
	engine *Engine // nil: Alarmo is the master, the test plays it on the driver
}

// newRig wires the alarm as main does: every report goes to the driver,
// then, standalone, to the engine. path holds the engine's state.
func newRig(t *testing.T, sim *simSiren, standalone bool, path string) *rig {
	t.Helper()
	old := sirenMargin
	t.Cleanup(func() { sirenMargin = old })
	sirenMargin = 30 * time.Millisecond

	r := &rig{sim: sim, driver: NewSirenDriver(sim, nil)}
	if standalone {
		r.engine = New(path, r.driver, func(id int) SensorConfig { return simSensors[id] }, nil, nil)
		r.engine.SetTimings(sim.exit, sim.entry)
	}
	sim.set(func(s *simSiren) {
		s.reverse = true
		s.deliver = func(st SirenState) {
			r.driver.HandleReport(st)
			if r.engine != nil {
				r.engine.HandleSirenState(st)
			}
		}
	})
	return r
}

func (r *rig) sensor(id int) { r.engine.HandleSensorEvent(id, simTypes[id], true) }

func (r *rig) alarmIs(s State) func() bool {
	return func() bool { return r.engine.Snapshot().State == s }
}

func (r *rig) sirenIs(s SirenState) func() bool { return func() bool { return r.sim.now() == s } }

func newStandalone(t *testing.T) *rig {
	return newRig(t, newSimSiren(simSensors), true, filepath.Join(t.TempDir(), "alarm.json"))
}

// TestScenarioKeypadCycle: the hardware cycle of 17:13 — keypad arming,
// the siren's exit delay, a door, its entry delay, the alert, the alert
// over, the keypad disarming.
func TestScenarioKeypadCycle(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceLocal)
	eventually(t, "siren in its exit delay", r.sirenIs(SirenExitDelay))
	eventually(t, "armed", r.alarmIs(StateArmedAway))
	eventually(t, "siren armed", r.sirenIs(SirenArmed))

	r.sensor(23)
	eventually(t, "siren in its entry delay", r.sirenIs(SirenEntryDelay))
	eventually(t, "set off", r.alarmIs(StateTriggered))
	eventually(t, "siren alert over", r.sirenIs(SirenAlertOver))

	r.engine.HandleCommand("disarm", SourceLocal)
	eventually(t, "siren off", r.sirenIs(SirenOff))
	holds(t, "disarmed, siren off", func() bool { return r.alarmIs(StateDisarmed)() && r.sirenIs(SirenOff)() })
}

// TestScenarioNightMode: armed for the night, a night-allowed sensor sets
// nothing off, the gateway present or gone; a change to away arms the
// siren again for every sensor.
func TestScenarioNightMode(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_night", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.sensor(17)
	r.sim.sensorFrame(17)
	holds(t, "still armed", func() bool { return r.alarmIs(StateArmedNight)() && r.sirenIs(SirenArmed)() })

	r.engine.HandleCommand("arm_away", SourceRemote)
	time.Sleep(20 * time.Millisecond)
	r.sim.set(func(s *simSiren) { s.mute = true })
	r.sim.sensorFrame(17)
	eventually(t, "siren hears the motion sensor in away mode", r.sirenIs(SirenEntryDelay))
}

// TestScenarioInstantWindow: an instant sensor sets everything off at once.
func TestScenarioInstantWindow(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.sensor(30)
	eventually(t, "set off", r.alarmIs(StateTriggered))
	eventually(t, "siren alert", r.sirenIs(SirenAlert))
}

// TestScenarioSirenReboots: a siren that reboots while armed is armed
// again (grill B4).
func TestScenarioSirenReboots(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	time.Sleep(rearmEvery / 100)
	r.sim.reboot(false)
	eventually(t, "siren armed again", r.sirenIs(SirenArmed))
	holds(t, "still armed", r.alarmIs(StateArmedAway))
}

// TestScenarioUnseenRebootThenIntrusion: a siren off without anyone
// knowing, then a door: the intrusion is not forgotten, the siren is armed
// again and set off (grill B2).
func TestScenarioUnseenRebootThenIntrusion(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.sim.reboot(true)
	r.sensor(23)
	eventually(t, "set off", r.alarmIs(StateTriggered))
	eventually(t, "siren alert", func() bool { s := r.sim.now(); return s == SirenAlert || s == SirenAlertOver })
}

// TestScenarioSirenTornOff: torn off during the entry delay, the siren
// reports nothing more: the alarm goes off all the same (grill B1).
func TestScenarioSirenTornOff(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.sensor(23)
	eventually(t, "entry delay", r.alarmIs(StatePending))
	r.sim.set(func(s *simSiren) { s.gone = true })
	eventually(t, "set off without the siren", r.alarmIs(StateTriggered))
}

// TestScenarioNeverBack: once set off, a stray report, a reboot or the
// siren's own end of alert never brings the alarm back to armed (grill
// B3); only a disarm does.
func TestScenarioNeverBack(t *testing.T) {
	r := newStandalone(t)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.sensor(30)
	eventually(t, "set off", r.alarmIs(StateTriggered))
	r.engine.HandleSirenState(SirenArmed)
	r.sim.reboot(false)
	holds(t, "still set off", r.alarmIs(StateTriggered))
	eventually(t, "siren armed again after its reboot", r.sirenIs(SirenArmed))
}

// TestScenarioOutage: the hardware outage of 17:16 — armed, openqiarad
// stopped, a motion; the siren goes off alone; openqiarad starts again,
// reloads armed, and the siren's report sets the alarm off. The siren
// keeps its alarm until the keypad disarms.
func TestScenarioOutage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarm.json")
	sim := newSimSiren(simSensors)
	r := newRig(t, sim, true, path)
	r.engine.HandleCommand("arm_away", SourceRemote)
	eventually(t, "siren armed", r.sirenIs(SirenArmed))

	sim.set(func(s *simSiren) { s.mute, s.deliver = true, nil }) // openqiarad stopped
	sim.sensorFrame(17)
	eventually(t, "siren alert, alone", func() bool { s := sim.now(); return s == SirenAlert || s == SirenAlertOver })

	again := newRig(t, sim, true, path) // openqiarad started again
	sim.set(func(s *simSiren) { s.mute = false })
	if err := again.engine.Load(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "set off once back", again.alarmIs(StateTriggered))
	holds(t, "the siren keeps its alarm", func() bool { s := sim.now(); return s == SirenAlert || s == SirenAlertOver })

	again.engine.HandleCommand("disarm", SourceLocal)
	eventually(t, "siren off", again.sirenIs(SirenOff))
}

// TestScenarioAlarmoMirror: Alarmo's states, as the mirror maps them, on
// the driver: the hardware cycles of 16:12 and 16:39, then a night arming
// after an away one keeps the night sensors out (grill B5).
func TestScenarioAlarmoMirror(t *testing.T) {
	sim := newSimSiren(simSensors)
	r := newRig(t, sim, false, "")
	r.driver.Disarm()
	r.driver.Arm(false, true) // arming
	eventually(t, "siren in its exit delay", r.sirenIs(SirenExitDelay))
	r.driver.Arm(false, false) // armed_away
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	r.driver.EntryDelay() // pending
	eventually(t, "siren in its entry delay", r.sirenIs(SirenEntryDelay))
	r.driver.Alert() // triggered
	eventually(t, "siren alert", r.sirenIs(SirenAlert))
	r.driver.Disarm()
	eventually(t, "siren off", r.sirenIs(SirenOff))
	holds(t, "siren stays off", r.sirenIs(SirenOff))

	r.driver.Arm(false, false)
	eventually(t, "siren armed away", r.sirenIs(SirenArmed))
	r.driver.Disarm()
	r.driver.Arm(false, true)
	r.driver.Arm(true, false) // armed_night
	eventually(t, "siren armed", r.sirenIs(SirenArmed))
	sim.set(func(s *simSiren) { s.mute = true })
	sim.sensorFrame(17)
	holds(t, "the night sensor sets nothing off", r.sirenIs(SirenArmed))
}

// TestScenarioAlarmoExitShorter: Alarmo's exit delay ends before the
// siren's; armed at once then, the siren takes an alert right away
// instead of staying mute until its own exit delay ends (third grill).
func TestScenarioAlarmoExitShorter(t *testing.T) {
	sim := newSimSiren(simSensors)
	sim.set(func(s *simSiren) { s.exit = time.Second })
	r := newRig(t, sim, false, "")
	r.driver.Disarm()
	r.driver.Arm(false, true) // arming
	eventually(t, "siren in its exit delay", r.sirenIs(SirenExitDelay))
	r.driver.Arm(false, false) // armed_away, Alarmo's delay over
	r.driver.Alert()           // triggered at once
	eventually(t, "siren alert well before its own exit delay ends", r.sirenIs(SirenAlert))
}

// TestScenarioBootKeepsTheOutageAlarm: openqiarad starts while the siren
// holds an alarm raised during the outage; Alarmo's retained state says
// armed: the siren keeps it (grill R1). Disarmed, it stops.
func TestScenarioBootKeepsTheOutageAlarm(t *testing.T) {
	sim := newSimSiren(simSensors)
	sim.set(func(s *simSiren) { s.state, s.active = SirenAlertOver, map[int]bool{17: true} })
	r := newRig(t, sim, false, "")
	_ = sim.RequestState() // what the radio asks at its start
	r.driver.Arm(false, false)
	holds(t, "the siren keeps its alarm", r.sirenIs(SirenAlertOver))
	r.driver.Disarm()
	eventually(t, "siren off", r.sirenIs(SirenOff))
}

// TestScenarioSirenLeftOut: siren_sounds none — the alarm runs on its own
// delays and a siren found armed is disarmed.
func TestScenarioSirenLeftOut(t *testing.T) {
	sim := newSimSiren(simSensors)
	sim.set(func(s *simSiren) { s.absent = true; s.state = SirenArmed })
	r := newRig(t, sim, true, filepath.Join(t.TempDir(), "alarm.json"))
	_ = sim.RequestState()
	eventually(t, "siren disarmed", r.sirenIs(SirenOff))
	r.engine.HandleCommand("arm_away", SourceLocal)
	eventually(t, "armed on its own delay", r.alarmIs(StateArmedAway))
	r.sensor(23)
	eventually(t, "set off on its own delay", r.alarmIs(StateTriggered))
	holds(t, "the siren stays off", r.sirenIs(SirenOff))
}
