package aaceld

import "testing"

func TestEncode(t *testing.T) {
	e, err := New(16000, 24000)
	if err != nil {
		t.Skip(err) // libfdk-aac absent: the camera has it
	}
	defer e.Close()
	pcm := make([]int16, FrameSamples)
	total := 0
	for f := range 50 {
		for i := range pcm {
			pcm[i] = int16((f*FrameSamples + i) * 37 % 8000)
		}
		au, err := e.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		total += len(au)
	}
	// 24 kbit/s over 50 frames of 30 ms: ~4.5 KB.
	if total < 2000 || total > 8000 {
		t.Fatalf("%d bytes for 1.5 s at 24 kbit/s", total)
	}
	if _, err := e.Encode(pcm[:10]); err == nil {
		t.Fatal("short frame accepted")
	}
}
