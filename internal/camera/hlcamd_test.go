package camera

import (
	"context"
	"path/filepath"
	"testing"
)

// TestResumerShutterClosed: while the shutter is closed, a viewer's
// request (a stale or missing playlist) resumes nothing: the streams stay
// paused (#50).
func TestResumerShutterClosed(t *testing.T) {
	r := NewHlcamdResumer(PlaylistMtime(filepath.Join(t.TempDir(), "missing.m3u8")), 0, 0, nil)
	r.closed.Store(true)
	if r.ResumeIfStale(context.Background()) {
		t.Error("streams resumed with the shutter closed")
	}
	if err := r.ForceResume(context.Background()); err == nil {
		t.Error("forced resume with the shutter closed")
	}
}
