package quality

import (
	"encoding/binary"
	"slices"
	"testing"
	"time"
)

func TestAppendPCM16WithMinimumPauseAddsOnlyMissingSilence(t *testing.T) {
	t.Parallel()
	const sampleRate = 1_000
	left := append(pcm16Frames(5, 2_000, 1), pcm16Frames(5, 0, 1)...)
	right := append(pcm16Frames(3, 0, 1), pcm16Frames(5, -2_000, 1)...)
	leftBefore := slices.Clone(left)
	rightBefore := slices.Clone(right)

	joined, metrics, err := AppendPCM16WithMinimumPause(
		left,
		right,
		sampleRate,
		1,
		10*time.Millisecond,
		-50,
	)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.ExistingSeconds != 0.008 || metrics.AddedSeconds != 0.002 ||
		metrics.ResultingSeconds != 0.010 || metrics.TargetSeconds != 0.010 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if got, want := len(joined), len(left)+len(right)+4; got != want {
		t.Fatalf("joined bytes = %d, want %d", got, want)
	}
	if !slices.Equal(left, leftBefore) || !slices.Equal(right, rightBefore) {
		t.Fatal("joiner mutated an input buffer")
	}
}

func TestAppendPCM16WithMinimumPauseDoesNotAddWhenExistingIsEnough(t *testing.T) {
	t.Parallel()
	left := append(pcm16Frames(2, 2_000, 1), pcm16Frames(7, 0, 1)...)
	right := append(pcm16Frames(5, 0, 1), pcm16Frames(2, 2_000, 1)...)

	joined, metrics, err := AppendPCM16WithMinimumPause(
		left,
		right,
		1_000,
		1,
		10*time.Millisecond,
		-50,
	)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.ExistingSeconds != 0.012 || metrics.AddedSeconds != 0 ||
		metrics.ResultingSeconds != 0.012 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if got, want := len(joined), len(left)+len(right); got != want {
		t.Fatalf("joined bytes = %d, want %d", got, want)
	}
}

func TestAppendPCM16WithMinimumPausePreservesStereoFrameAlignment(t *testing.T) {
	t.Parallel()
	left := pcm16Frames(1, 1_000, 2)
	right := pcm16Frames(1, -1_000, 2)
	joined, metrics, err := AppendPCM16WithMinimumPause(
		left,
		right,
		1_000,
		2,
		3*time.Millisecond,
		-50,
	)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.AddedSeconds != 0.003 || len(joined) != 5*4 || len(joined)%4 != 0 {
		t.Fatalf("joined bytes=%d metrics=%+v", len(joined), metrics)
	}
}

func TestAppendPCM16WithMinimumPauseRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	valid := pcm16Frames(1, 1_000, 1)
	for name, test := range map[string]struct {
		left       []byte
		right      []byte
		sampleRate int
		channels   int
		target     time.Duration
		threshold  float64
	}{
		"empty":             {nil, valid, 1_000, 1, 0, -50},
		"format":            {valid, valid, 0, 1, 0, -50},
		"unaligned":         {append(valid, 0), valid, 1_000, 1, 0, -50},
		"negative target":   {valid, valid, 1_000, 1, -time.Millisecond, -50},
		"excessive target":  {valid, valid, 1_000, 1, 5*time.Second + 1, -50},
		"invalid threshold": {valid, valid, 1_000, 1, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := AppendPCM16WithMinimumPause(
				test.left,
				test.right,
				test.sampleRate,
				test.channels,
				test.target,
				test.threshold,
			); err == nil {
				t.Fatal("error = nil")
			}
		})
	}
}

func pcm16Frames(frames int, value int16, channels int) []byte {
	result := make([]byte, frames*channels*2)
	for offset := 0; offset < len(result); offset += 2 {
		binary.LittleEndian.PutUint16(result[offset:offset+2], uint16(value))
	}
	return result
}
