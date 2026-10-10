package aacenc

// Profile is an AAC audio object type.
type Profile int

const (
	// LC is AAC-LC, what RTSP clients play.
	LC Profile = 2
	// ELD is AAC-ELD, what HomeKit asks for.
	ELD Profile = 39
)

// FrameSamples is one access unit: 1024 samples for LC; 480 for ELD,
// 30 ms at 16 kHz, the packet time iOS asks for.
func (p Profile) FrameSamples() int {
	if p == ELD {
		return 480
	}
	return 1024
}
