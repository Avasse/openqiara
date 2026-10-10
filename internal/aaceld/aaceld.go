//go:build cgo

// Package aaceld encodes PCM to AAC-ELD with libfdk-aac, the library the
// camera's hls already uses (/usr/lib/libfdk-aac.so.2). HomeKit streams
// camera audio in AAC-ELD: iOS refuses the Opus the accessory offers
// (2026-10-10, raw SelectedRTPStreamConfiguration). No pure-Go encoder
// exists.
//
// The library is opened at run time (dlopen): without it, openqiarad
// starts all the same, without sound. The few declarations below follow
// fdk-aac's aacenc_lib.h (2.0.x).
package aaceld

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>

typedef struct {
	int numBufs;
	void **bufs;
	int *bufferIdentifiers;
	int *bufSizes;
	int *bufElSizes;
} BufDesc;
typedef struct { int numInSamples; int numAncBytes; } InArgs;
typedef struct { int numOutBytes; int numInSamples; int numAncBytes; int bitResState; } OutArgs;

static int (*pOpen)(void **, unsigned, unsigned);
static int (*pSet)(void *, unsigned, unsigned);
static int (*pEncode)(void *, BufDesc *, BufDesc *, InArgs *, OutArgs *);
static int (*pClose)(void **);

static int load(const char *name) {
	void *lib = dlopen(name, RTLD_NOW);
	if (!lib) return -1;
	pOpen = dlsym(lib, "aacEncOpen");
	pSet = dlsym(lib, "aacEncoder_SetParam");
	pEncode = dlsym(lib, "aacEncEncode");
	pClose = dlsym(lib, "aacEncClose");
	return pOpen && pSet && pEncode && pClose ? 0 : -2;
}

enum {
	AACENC_AOT = 0x0100, AACENC_BITRATE = 0x0101, AACENC_SAMPLERATE = 0x0103,
	AACENC_GRANULE_LENGTH = 0x0105, AACENC_CHANNELMODE = 0x0106,
	AACENC_TRANSMUX = 0x0300,
	AOT_ER_AAC_ELD = 39, MODE_1 = 1, TT_MP4_RAW = 0,
	IN_AUDIO_DATA = 0, OUT_BITSTREAM_DATA = 3,
};

// enc_open opens a mono AAC-ELD encoder, raw access units of frame
// samples. Returns 0 or the failing step.
static int enc_open(void **h, unsigned rate, unsigned bitrate, unsigned frame) {
	if (pOpen(h, 0, 1)) return 1;
	if (pSet(*h, AACENC_AOT, AOT_ER_AAC_ELD)) return 2;
	if (pSet(*h, AACENC_SAMPLERATE, rate)) return 3;
	if (pSet(*h, AACENC_CHANNELMODE, MODE_1)) return 4;
	if (pSet(*h, AACENC_BITRATE, bitrate)) return 5;
	if (pSet(*h, AACENC_TRANSMUX, TT_MP4_RAW)) return 6;
	if (pSet(*h, AACENC_GRANULE_LENGTH, frame)) return 7;
	if (pEncode(*h, NULL, NULL, NULL, NULL)) return 8;
	return 0;
}

// enc_frame encodes n samples; returns the access unit's size (0 while the
// encoder fills up) or -error.
static int enc_frame(void *h, short *pcm, int n, unsigned char *out, int outSize) {
	void *in = pcm, *o = out;
	int inID = IN_AUDIO_DATA, outID = OUT_BITSTREAM_DATA;
	int inSize = n * 2, inEl = 2, outEl = 1;
	BufDesc inD = {1, &in, &inID, &inSize, &inEl};
	BufDesc outD = {1, &o, &outID, &outSize, &outEl};
	InArgs ia = {n, 0};
	OutArgs oa = {0};
	int err = pEncode(h, &inD, &outD, &ia, &oa);
	return err ? -err : oa.numOutBytes;
}

static void enc_close(void **h) { pClose(h); }
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

var (
	loadOnce sync.Once
	loadErr  error
)

// libraries are tried in order: the camera's, then a desktop install
// (tests).
var libraries = []string{"libfdk-aac.so.2", "libfdk-aac.dylib", "/opt/homebrew/lib/libfdk-aac.dylib"}

func load() error {
	loadOnce.Do(func() {
		loadErr = errors.New("libfdk-aac not found")
		for _, name := range libraries {
			cs := C.CString(name)
			r := C.load(cs)
			C.free(unsafe.Pointer(cs))
			if r == 0 {
				loadErr = nil
				return
			}
		}
	})
	return loadErr
}

// Encoder turns 16-bit mono PCM into AAC-ELD access units. Not safe for
// concurrent use.
type Encoder struct {
	h   unsafe.Pointer
	out []byte
}

// New opens an encoder for mono PCM at sampleRate, aiming at bitrate
// (bit/s).
func New(sampleRate, bitrate int) (*Encoder, error) {
	if err := load(); err != nil {
		return nil, err
	}
	e := &Encoder{out: make([]byte, 1024)}
	if r := C.enc_open(&e.h, C.uint(sampleRate), C.uint(bitrate), FrameSamples); r != 0 {
		if e.h != nil {
			C.enc_close(&e.h)
		}
		return nil, fmt.Errorf("aac-eld encoder setup failed (step %d)", r)
	}
	return e, nil
}

// Encode encodes FrameSamples samples. The access unit it returns is
// valid until the next call; it is empty while the encoder fills up.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != FrameSamples {
		return nil, fmt.Errorf("aac-eld: %d samples, want %d", len(pcm), FrameSamples)
	}
	n := C.enc_frame(e.h, (*C.short)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&e.out[0])), C.int(len(e.out)))
	if n < 0 {
		return nil, fmt.Errorf("aac-eld: encode error %#x", int(-n))
	}
	return e.out[:n], nil
}

// Close frees the encoder.
func (e *Encoder) Close() {
	if e.h != nil {
		C.enc_close(&e.h)
	}
}
