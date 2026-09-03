package quality

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestAnalyzePCM16(t *testing.T) {
	t.Parallel()
	pcm := pcm16Samples(0, 0, 16_384, -16_384, 32_767, 0)
	metrics, err := AnalyzePCM16(pcm, 2, 1, -50)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.DurationSeconds != 3 {
		t.Errorf("duration = %v, want 3", metrics.DurationSeconds)
	}
	if metrics.PeakDBFS == nil || math.Abs(*metrics.PeakDBFS) > 0.001 {
		t.Errorf("peak dBFS = %v, want approximately 0", metrics.PeakDBFS)
	}
	if metrics.RMSDBFS == nil {
		t.Fatal("RMS dBFS is nil")
	}
	if got, want := metrics.ClippingPercent, 100.0/6.0; math.Abs(got-want) > 0.0001 {
		t.Errorf("clipping = %v, want %v", got, want)
	}
	if metrics.SilenceRatio != 0.5 || metrics.LeadingSilenceSeconds != 1 ||
		metrics.TrailingSilenceSeconds != 0.5 {
		t.Errorf("unexpected silence metrics: %+v", metrics)
	}
	if metrics.LUFSI != nil || metrics.ContainsNaNOrInf {
		t.Errorf("unexpected unsupported/finite metrics: %+v", metrics)
	}
}

func TestEncodePCM16WAV(t *testing.T) {
	t.Parallel()
	pcm := pcm16Samples(-1, 0, 1)
	wav, err := EncodePCM16WAV(pcm, 24_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		binary.LittleEndian.Uint32(wav[24:28]) != 24_000 ||
		binary.LittleEndian.Uint32(wav[40:44]) != uint32(len(pcm)) ||
		string(wav[44:]) != string(pcm) {
		t.Fatalf("invalid WAV: %x", wav)
	}
}

func TestAnalyzePCM16RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		pcm        []byte
		sampleRate int
		channels   int
		threshold  float64
	}{
		{name: "empty", sampleRate: 24_000, channels: 1, threshold: -50},
		{name: "unaligned", pcm: []byte{1}, sampleRate: 24_000, channels: 1, threshold: -50},
		{name: "sample rate", pcm: []byte{0, 0}, channels: 1, threshold: -50},
		{name: "threshold", pcm: []byte{0, 0}, sampleRate: 24_000, channels: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := AnalyzePCM16(test.pcm, test.sampleRate, test.channels, test.threshold); err == nil {
				t.Fatal("AnalyzePCM16() error = nil")
			}
		})
	}
}

func pcm16Samples(samples ...int16) []byte {
	pcm := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:index*2+2], uint16(sample))
	}
	return pcm
}
