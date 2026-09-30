package domusvm

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

// Manifest mirrors /etc/hl/update_manifest.json on a Qiara camera.
// It maps a sensor firmware FNV hash to the bytecode file that fbxhome
// should push to that sensor on init.
type Manifest struct {
	Supported []ManifestEntry `json:"supported"`
}

// ManifestEntry is one row of Manifest.Supported. Hashes are kept as
// hex strings (16 chars, big-endian u64) — the same form fbxhome writes
// and reads them.
type ManifestEntry struct {
	HashImage    string `json:"hash_image"`    // FNV of the sensor firmware
	Model        string `json:"model"`         // e.g. HOMELABSRN00BCFD
	BytecodeHash string `json:"bytecode_hash"` // filename (no .bin) of the bytecode to push
	VMSize       int    `json:"vm_size"`
}

// LoadManifest parses an update_manifest.json file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Lookup returns the bytecode entry matching the sensor firmware hash,
// or nil if none is registered.
func (m *Manifest) Lookup(fwHash string) *ManifestEntry {
	for i := range m.Supported {
		if m.Supported[i].HashImage == fwHash {
			return &m.Supported[i]
		}
	}
	return nil
}

// LoadBytecode reads a bytecode file and returns what goes on the wire:
// the file without its 4-byte size header, its 8-byte placeholder filled
// with the FNV-1a 64 of the bytecode zero-padded to vmSize-8 bytes, little
// endian (fbxhome get_actual_bc_buffer FUN_0008d848, hash FUN_00030304).
// The sensor stores that hash and reports it in its status.
//
// The on-disk layout is:
//
//	[0..4]   u32 LE size of the bytecode (file_size - 4)
//	[4..12]  zero placeholder
//	[12..]   raw bytecode
//
// The file name is not that hash in a fixed byte order: it reads as the
// value for the siren's a0a4789e3dd0293c.bin, as its bytes for the DWS's
// 932f56223f735a6b.bin.
func LoadBytecode(dir, bytecodeHash string, vmSize int) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, bytecodeHash+".bin"))
	if err != nil {
		return nil, fmt.Errorf("read bytecode: %w", err)
	}
	if len(raw) < 12 || len(raw)-4 > vmSize {
		return nil, fmt.Errorf("bytecode %s: %d bytes for a %d-byte VM", bytecodeHash, len(raw), vmSize)
	}
	padded := make([]byte, vmSize-8)
	copy(padded, raw[12:])
	h := fnv.New64a()
	h.Write(padded)
	out := append([]byte(nil), raw[4:]...)
	binary.LittleEndian.PutUint64(out, h.Sum64())
	return out, nil
}

// Frames returns the VM write frames that install the bytecode of the
// sensor firmware fwHash, as fbxhome sends them: the radio engine's
// bytecode source. A siren pushed while armed also gets a dynamic config
// from fbxhome (HlSrn get_conf_from_node); it is not built here.
func (m *Manifest) Frames(dir string, fwHash []byte) ([][]byte, error) {
	entry := m.Lookup(hex.EncodeToString(fwHash))
	if entry == nil {
		return nil, fmt.Errorf("domusvm: firmware %x not in the manifest", fwHash)
	}
	bytecode, err := LoadBytecode(dir, entry.BytecodeHash, entry.VMSize)
	if err != nil {
		return nil, err
	}
	var frames [][]byte
	for _, f := range SplitOps(BuildInitSequence(bytecode, nil)) {
		frames = append(frames, f.Encode())
	}
	return frames, nil
}
