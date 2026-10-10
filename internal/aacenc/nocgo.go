//go:build !cgo

package aacenc

import "errors"

// Without cgo (a cross-build without zig), there is no encoder: HomeKit
// streams without sound.

// Encoder is never built without cgo.
type Encoder struct{}

// New always fails without cgo.
func New(p Profile, sampleRate, bitrate int) (*Encoder, error) {
	return nil, errors.New("built without cgo: no AAC encoder")
}

// Encode is never reached.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) { return nil, errors.New("no encoder") }

// Close does nothing.
func (e *Encoder) Close() {}
