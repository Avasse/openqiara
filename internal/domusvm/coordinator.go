package domusvm

import "encoding/binary"

// VM address constants used by the bytecode init sequence. These match
// the addresses fbxhome writes to via OpWrite. They are part of the VM
// ABI and shared across all sensor models.
const (
	// vmAddrConfig is where sensor-specific dynamic configuration is
	// written (e.g. HlSrn sigfox identifiers when ARMED). The high bit
	// signals "VM config register" rather than "program memory".
	vmAddrConfig uint32 = 0x800000c0

	// vmAddrTimeout is where the VM execution timeout is written, as a
	// little-endian u32 in milliseconds.
	vmAddrTimeout uint32 = 0x800000d8

	// vmDefaultTimeoutMS is the timeout fbxhome always sets (9600 ms).
	vmDefaultTimeoutMS uint32 = 0x2580

	// vmChunkSize is the size of each bytecode write. fbxhome uses 64
	// bytes (0x40), which keeps two writes per frame under the 100-byte
	// flush limit.
	vmChunkSize = 64

	// vmBootEntry is the entry point used by fbxhome to start the VM.
	vmBootEntry uint32 = 0x00000008
)

// BuildInitSequence returns the full opcode stream fbxhome sends to
// initialize and start a sensor's VM with the given bytecode.
//
// The sequence is:
//  1. OpStartInit  — reset the VM
//  2. OpErase      — clear program memory
//  3. OpWrite × N  — push bytecode in 64-byte chunks at offsets 0, 64, ...
//  4. OpWrite       — push dynamicConf at vmAddrConfig (optional, nil to skip)
//  5. OpWrite       — push the VM timeout at vmAddrTimeout
//  6. OpBoot       — start the VM at vmBootEntry
//
// Callers split the result into frames with SplitOps before sending.
//
// dynamicConf is the per-sensor configuration produced by the sensor's
// get_conf_from_node equivalent (e.g. HlSrn sigfox config when ARMED).
// Pass nil when the sensor has no dynamic config to push.
func BuildInitSequence(bytecode []byte, dynamicConf []byte) []Op {
	ops := make([]Op, 0, len(bytecode)/vmChunkSize+5)
	ops = append(ops, OpStartInit{}, OpErase{})

	for offset := 0; offset < len(bytecode); offset += vmChunkSize {
		end := offset + vmChunkSize
		if end > len(bytecode) {
			end = len(bytecode)
		}
		ops = append(ops, OpWrite{
			Addr: uint32(offset),
			Data: bytecode[offset:end],
		})
	}

	if len(dynamicConf) > 0 {
		ops = append(ops, OpWrite{Addr: vmAddrConfig, Data: dynamicConf})
	}

	timeout := make([]byte, 4)
	binary.LittleEndian.PutUint32(timeout, vmDefaultTimeoutMS)
	ops = append(ops, OpWrite{Addr: vmAddrTimeout, Data: timeout})

	ops = append(ops, OpBoot{Mode: 0, Entry: vmBootEntry})
	return ops
}
