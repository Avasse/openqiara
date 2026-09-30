// Package domusvm implements the VM-write protocol used by fbxhome to
// transfer bytecode and configuration to Domus sensors (HlSrn, HlDws,
// HlPir, HlKpd) over charmux managed frames.
//
// This is the low-level wire format only: opcodes, frame assembly, ACK
// parsing, bytecode loading. It has no knowledge of sensor state or
// alarm rules — those belong to higher-level packages.
package domusvm

import (
	"encoding/binary"
	"fmt"
)

// Op is a single VM-write opcode appended to a managed frame.
//
// The opcode set is derived from fbxhome's FUN_000938a0 serializer. Each
// concrete type encodes itself onto the wire and reports its size
// contribution to the frame-flush accounting (see Frame).
type Op interface {
	// Encode appends the wire bytes of this opcode to dst.
	Encode(dst []byte) []byte

	// SizeContribution returns the number of bytes this opcode adds to
	// the per-frame size counter (uVar6 in the RE). It matches the
	// opcode header size plus any inline payload, and is what the
	// flush rule (>100 strict) is checked against.
	SizeContribution() int
}

// OpStartInit (0x80) initializes the VM on the target. No payload.
type OpStartInit struct{}

func (OpStartInit) Encode(dst []byte) []byte { return append(dst, 0x80) }
func (OpStartInit) SizeContribution() int    { return 1 }

// OpErase (0x88) erases the VM program memory. No payload.
type OpErase struct{}

func (OpErase) Encode(dst []byte) []byte { return append(dst, 0x88) }
func (OpErase) SizeContribution() int    { return 1 }

// OpWrite (0x87) writes Data at Addr in the target's address space.
//
// Addresses with the high bit set (0x80000000+) target VM configuration
// registers (e.g. 0x800000c0 for HlSrn sigfox config, 0x800000d8 for
// the VM timeout register). Lower addresses target program memory.
//
// Data length must fit in a uint8 (0..255).
type OpWrite struct {
	Addr uint32
	Data []byte
}

func (o OpWrite) Encode(dst []byte) []byte {
	if len(o.Data) > 0xff {
		panic(fmt.Sprintf("domusvm: OpWrite data too large (%d bytes, max 255)", len(o.Data)))
	}
	dst = append(dst, 0x87)
	dst = binary.LittleEndian.AppendUint32(dst, o.Addr)
	dst = append(dst, byte(len(o.Data)))
	return append(dst, o.Data...)
}

func (o OpWrite) SizeContribution() int { return 5 + len(o.Data) }

// OpBoot (0x82) starts the VM at Entry with the given Mode.
type OpBoot struct {
	Mode  uint16
	Entry uint32
}

func (o OpBoot) Encode(dst []byte) []byte {
	dst = append(dst, 0x82)
	dst = binary.LittleEndian.AppendUint16(dst, o.Mode)
	return binary.LittleEndian.AppendUint32(dst, o.Entry)
}

func (OpBoot) SizeContribution() int { return 5 }
