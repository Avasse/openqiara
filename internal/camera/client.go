// Package camera is openqiarad's side of the Qiara camera: the radio
// gateway that serves the paired sensors (RadioClient), the shutter, and
// the video pipeline helpers.
package camera

import (
	"context"
	"time"
)

// Client drives the camera's sensors and actuators.
type Client interface {
	// Connect takes the radio over.
	Connect(ctx context.Context) error

	// CachedSensors returns the paired sensors with their live state.
	CachedSensors() []Sensor

	// ReadSensor returns a sensor's live state.
	ReadSensor(ctx context.Context, id int) (*Sensor, error)

	// StartPairing waits for a sensor of the given type (DWS, PIR, SRN, KPD)
	// in pairing mode. Returns a session ID for polling.
	StartPairing(ctx context.Context, sensorType string) (int, error)

	// PollPairing checks the status of an ongoing pairing session.
	// Returns the paired sensor and true when pairing completes.
	PollPairing(ctx context.Context, session int) (*Sensor, bool, error)

	// StopPairing cancels an ongoing pairing session.
	StopPairing(ctx context.Context, session int) error

	// DeleteSensor stops serving a paired sensor.
	DeleteSensor(ctx context.Context, id int) error

	// SendPKT sends raw bytes to the MCU's radio channel (debug only).
	SendPKT(ctx context.Context, data []byte) error

	// TriggerSiren plays the siren's discreet test sound.
	TriggerSiren(ctx context.Context, sensorID int) error

	// TriggerSirenAlarm plays the test sound at full power for duration.
	TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error

	// StopSiren disarms the siren and stops whatever it plays.
	StopSiren(ctx context.Context, sensorID int) error

	// ArmSiren arms the siren, which then keeps the alarm's delays and
	// sounds, and hears the sensors itself when the gateway is gone.
	ArmSiren(ctx context.Context, sensorID int, a SirenArming) error

	// SirenEntryDelay starts an armed siren's entry delay.
	SirenEntryDelay(ctx context.Context, sensorID int) error

	// SirenAlert sets an armed siren off.
	SirenAlert(ctx context.Context, sensorID int) error

	// RequestSirenState asks the siren for its state (Sensor.SirenState).
	RequestSirenState(ctx context.Context, sensorID int) error

	// SetShutter opens or closes the camera shutter.
	SetShutter(ctx context.Context, open bool) error

	// Events returns a channel of sensor state changes.
	// The channel is closed when Close is called.
	Events() <-chan SensorEvent

	// Close releases the radio.
	Close() error
}
