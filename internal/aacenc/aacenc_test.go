package aacenc

import "testing"

func TestEncode(t *testing.T) {
	for _, p := range []Profile{ELD, LC} {
		t.Run(map[Profile]string{ELD: "eld", LC: "lc"}[p], func(t *testing.T) { testEncode(t, p) })
	}
}

func testEncode(t *testing.T, p Profile) {
	e, err := New(p, 16000, 24000)
	if err != nil {
		t.Skip(err) // libfdk-aac absent: the camera has it
	}
	defer e.Close()
	pcm := make([]int16, p.FrameSamples())
	total, frames := 0, 1500*16/p.FrameSamples() // 1.5 s
	for f := range frames {
		for i := range pcm {
			pcm[i] = int16((f*len(pcm) + i) * 37 % 8000)
		}
		au, err := e.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		total += len(au)
	}
	// 24 kbit/s over 1.5 s: ~4.5 KB.
	if total < 2000 || total > 8000 {
		t.Fatalf("%d bytes for 1.5 s at 24 kbit/s", total)
	}
	if _, err := e.Encode(pcm[:10]); err == nil {
		t.Fatal("short frame accepted")
	}
}
