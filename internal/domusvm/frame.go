package domusvm

// Per-frame flush limit, from fbxhome's FUN_000938a0: a frame is flushed
// once its write counter (uVar6: header and data bytes of each op) exceeds
// 100, the op that crossed it included. fbxhome also caps read responses
// at 30 bytes, but the init sequence has no read op.
const maxWriteBytes = 100

// Frame is a managed VM-write frame: the leading 0x01 marker followed
// by a sequence of encoded opcodes. A frame is the unit charmux sends
// over the wire.
type Frame struct {
	Ops []Op
}

// Encode serializes the frame to wire bytes.
func (f Frame) Encode() []byte {
	out := []byte{0x01}
	for _, op := range f.Ops {
		out = op.Encode(out)
	}
	return out
}

// SplitOps groups a stream of opcodes into frames with fbxhome's flush
// rule. Each returned Frame is ready to be encoded and sent.
func SplitOps(ops []Op) []Frame {
	var frames []Frame
	var current []Op
	size := 0
	for _, op := range ops {
		current = append(current, op)
		size += op.SizeContribution()
		if size > maxWriteBytes {
			frames = append(frames, Frame{Ops: current})
			current, size = nil, 0
		}
	}
	if len(current) > 0 {
		frames = append(frames, Frame{Ops: current})
	}
	return frames
}
