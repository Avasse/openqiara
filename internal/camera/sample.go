package camera

// Sample is one media sample from the camera. Video: a single H.264 NAL
// unit, without the Annex B start code. Audio: raw PCM (s16le, 16 kHz,
// mono), as MulticastPCM reads it.
type Sample struct {
	IsVideo bool
	PTS     int64 // 90 kHz clock
	Data    []byte
}

// splitNALUnits splits an Annex B byte stream into individual NAL units,
// stripping the start codes. Returns each NAL unit's body (the first
// byte is the NAL header).
//
// Annex B format: each NAL unit is preceded by either 0x000001 (3-byte
// start code) or 0x00000001 (4-byte start code). The end of a NAL unit
// is the start code of the next NAL unit, or end of buffer.
func splitNALUnits(data []byte) [][]byte {
	// Find every start code position (returns offset of the first byte
	// AFTER the start code, i.e. the NAL header byte).
	var starts []int
	i := 0
	for i+2 < len(data) {
		if data[i] != 0x00 || data[i+1] != 0x00 {
			i++
			continue
		}
		// data[i..i+1] = 00 00
		if data[i+2] == 0x01 {
			// 3-byte start code 00 00 01 → NAL begins at i+3
			starts = append(starts, i+3)
			i += 3
			continue
		}
		if i+3 < len(data) && data[i+2] == 0x00 && data[i+3] == 0x01 {
			// 4-byte start code 00 00 00 01 → NAL begins at i+4
			starts = append(starts, i+4)
			i += 4
			continue
		}
		i++
	}

	if len(starts) == 0 {
		return nil
	}

	// Build NAL units: each NAL goes from starts[k] to (starts[k+1] - sclen),
	// where sclen is 3 or 4 depending on what precedes starts[k+1].
	var nals [][]byte
	for k, ns := range starts {
		var ne int
		if k+1 < len(starts) {
			next := starts[k+1]
			// Determine how many bytes the next start code consumed
			// (3 or 4) by looking back from `next`.
			ne = next - 3
			if ne >= 1 && data[ne-1] == 0x00 {
				// 4-byte start code (00 00 00 01)
				ne--
			}
		} else {
			ne = len(data)
		}
		if ne > ns && ne <= len(data) {
			nals = append(nals, data[ns:ne])
		}
	}
	return nals
}
