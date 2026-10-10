package camera

import (
	"context"
	"testing"
	"time"
)

// TestResumerShutterClosed: while the shutter is closed, a viewer's
// request (a stream that never moved) resumes nothing: the streams stay
// paused (#50).
func TestResumerShutterClosed(t *testing.T) {
	r := NewHlcamdResumer(func() time.Time { return time.Time{} }, 0, 0, nil)
	r.closed.Store(true)
	if r.ResumeIfStale(context.Background()) {
		t.Error("streams resumed with the shutter closed")
	}
	if err := r.ForceResume(context.Background()); err == nil {
		t.Error("forced resume with the shutter closed")
	}
}
