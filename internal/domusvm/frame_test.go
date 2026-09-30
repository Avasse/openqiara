package domusvm

import (
	"encoding/hex"
	"testing"
)

// TestDWSInitSequenceMatchesVendor reproduces the 3-frame bytecode init
// sequence captured live from fbxhome on 2026-05-16, when pairing a
// HOMELABDWS sensor on a real camera (node_id=18, gwdst=3, gwsrc=25).
//
// The vendor frames are taken directly from /var/log/fbxhome.log on the
// camera. The bytecode .bin file and manifest are copies of the cam's
// /lib/firmwares/bytecode/932f56223f735a6b.bin and
// /etc/hl/update_manifest.json. If our generator matches byte-for-byte,
// we are wire-compatible.
//
// DWS has no dynamic configuration (HlDws::get_conf_from_node is a
// no-op), so dynamicConf is nil.
func TestDWSInitSequenceMatchesVendor(t *testing.T) {
	bytecode, err := LoadBytecode("testdata", "932f56223f735a6b", 1024)
	if err != nil {
		// Bytecode files are proprietary and not committed.
		// See testdata/README.md for how to populate them.
		t.Skipf("testdata bytecode not present, skipping: %v", err)
	}

	ops := BuildInitSequence(bytecode, nil)
	frames := SplitOps(ops)

	expected := []string{
		// trame 1 (cnt:6) — 0x80 start + 0x88 erase + 0x87 chunk0 + 0x87 chunk1 (with 8-byte hash inserted at offset 0)
		"018088870000000040932f56223f735a6b09000400127064b001b2109c1070001001901110f31f039963912110f31f039963911170131004b443b2107062905310438003523020235287400000004000220300018c30b9833854154340030030b98338041001004340030030b98338847400000080436233b9b338847404000080436b837400000000436ac0c16391",
		// trame 2 (cnt:7) — 0x87 chunk2 + 0x87 chunk3
		"01878000000040802d117003b40352102040201270139c021040201270179c12100400107050c0ef201070c7c01020002c1070837404000080010003c03f201070c0c0602b107087c00000002c93c05a151b100a39a1d88a808a74000000009a38a3d88b7400000080ba600b102b406a562b410a39a1d80f20",
		// trame 3 (cnt:8) — 0x87 timeout write (0x800000d8 = 0x2580 ms) + 0x82 boot (entry=8)
		"0187d8000080048025000082000008000000",
	}

	if len(frames) != len(expected) {
		t.Fatalf("frame count: got %d, want %d", len(frames), len(expected))
	}

	for i, want := range expected {
		got := hex.EncodeToString(frames[i].Encode())
		if got != want {
			t.Errorf("frame %d mismatch:\n  got:  %s\n  want: %s", i+1, got, want)
		}
	}
}

// TestLoadManifestRoundtrip exercises the manifest parser against the
// camera's update_manifest.json (a copy under testdata).
func TestLoadManifestRoundtrip(t *testing.T) {
	m, err := LoadManifest("testdata/update_manifest.json")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entry := m.Lookup("48654bf742f5070e")
	if entry == nil {
		t.Fatal("expected to find DWS entry by fw hash")
	}
	if entry.BytecodeHash != "932f56223f735a6b" {
		t.Errorf("BytecodeHash: got %q, want %q", entry.BytecodeHash, "932f56223f735a6b")
	}
	if entry.Model != "HOMELABDWS00ACFD" {
		t.Errorf("Model: got %q, want %q", entry.Model, "HOMELABDWS00ACFD")
	}
}

// TestBytecodeHash: the hash on the wire is the FNV-1a of the bytecode
// padded to the VM size, not the file name. Expected bytes as fbxhome sent
// them (logs of 2026-05-15 and 2026-09-29 for the siren, 2026-05-16 pcap
// for the DWS).
func TestBytecodeHash(t *testing.T) {
	for _, c := range []struct {
		file   string
		vmSize int
		want   string
	}{
		{"a0a4789e3dd0293c", 2048, "3c29d03d9e78a4a0"},
		{"932f56223f735a6b", 1024, "932f56223f735a6b"},
	} {
		bytecode, err := LoadBytecode("testdata", c.file, c.vmSize)
		if err != nil {
			t.Skipf("testdata bytecode not present, skipping: %v", err)
		}
		if got := hex.EncodeToString(bytecode[:8]); got != c.want {
			t.Errorf("%s: hash %s, fbxhome sent %s", c.file, got, c.want)
		}
	}
}
