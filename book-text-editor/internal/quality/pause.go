package quality

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

const maxAdaptivePause = 5 * time.Second

// BoundaryPauseMetrics describes how much silence was already present around a
// PCM boundary and how much silence the joiner added to reach the requested
// minimum. Existing model-generated silence is never removed.
type BoundaryPauseMetrics struct {
	TargetSeconds    float64 `json:"target_seconds"`
	ExistingSeconds  float64 `json:"existing_seconds"`
	AddedSeconds     float64 `json:"added_seconds"`
	ResultingSeconds float64 `json:"resulting_seconds"`
}

// AppendPCM16WithMinimumPause joins two frame-aligned PCM16 streams and, when
// needed, inserts zero-valued frames so their combined boundary silence reaches
// target. The returned buffer never aliases either input.
func AppendPCM16WithMinimumPause(
	left []byte,
	right []byte,
	sampleRate int,
	channels int,
	target time.Duration,
	silenceThresholdDBFS float64,
) ([]byte, BoundaryPauseMetrics, error) {
	if len(left) == 0 || len(right) == 0 {
		return nil, BoundaryPauseMetrics{}, errors.New("both PCM payloads are required")
	}
	if sampleRate <= 0 || channels <= 0 {
		return nil, BoundaryPauseMetrics{}, errors.New("sample rate and channels must be positive")
	}
	if target < 0 || target > maxAdaptivePause {
		return nil, BoundaryPauseMetrics{}, fmt.Errorf(
			"pause target must be between 0 and %s",
			maxAdaptivePause,
		)
	}
	if math.IsNaN(silenceThresholdDBFS) || math.IsInf(silenceThresholdDBFS, 0) ||
		silenceThresholdDBFS >= 0 {
		return nil, BoundaryPauseMetrics{}, errors.New(
			"silence threshold must be a finite negative dBFS value",
		)
	}
	if channels > int(^uint(0)>>1)/2 {
		return nil, BoundaryPauseMetrics{}, errors.New("channel count is too large")
	}
	frameBytes := channels * 2
	if len(left)%frameBytes != 0 || len(right)%frameBytes != 0 {
		return nil, BoundaryPauseMetrics{}, fmt.Errorf(
			"PCM payload is not aligned to %d-byte frames",
			frameBytes,
		)
	}

	threshold := pcm16FullScale * math.Pow(10, silenceThresholdDBFS/20)
	trailingFrames := countSilentFrames(left, frameBytes, channels, threshold, true)
	leadingFrames := countSilentFrames(right, frameBytes, channels, threshold, false)
	existingFrames := trailingFrames + leadingFrames
	targetFrames := durationToFramesCeil(target, sampleRate)
	addedFrames := 0
	if existingFrames < targetFrames {
		addedFrames = targetFrames - existingFrames
	}
	if addedFrames > (int(^uint(0)>>1)-len(left)-len(right))/frameBytes {
		return nil, BoundaryPauseMetrics{}, errors.New("joined PCM payload is too large")
	}
	addedBytes := addedFrames * frameBytes
	joined := make([]byte, len(left)+addedBytes+len(right))
	copy(joined, left)
	copy(joined[len(left)+addedBytes:], right)

	secondsPerFrame := 1 / float64(sampleRate)
	return joined, BoundaryPauseMetrics{
		TargetSeconds:    float64(targetFrames) * secondsPerFrame,
		ExistingSeconds:  float64(existingFrames) * secondsPerFrame,
		AddedSeconds:     float64(addedFrames) * secondsPerFrame,
		ResultingSeconds: float64(existingFrames+addedFrames) * secondsPerFrame,
	}, nil
}

func durationToFramesCeil(duration time.Duration, sampleRate int) int {
	if duration == 0 {
		return 0
	}
	frames := (int64(duration)*int64(sampleRate) + int64(time.Second) - 1) /
		int64(time.Second)
	return int(frames)
}

func countSilentFrames(
	pcm []byte,
	frameBytes int,
	channels int,
	threshold float64,
	fromEnd bool,
) int {
	frames := len(pcm) / frameBytes
	for offset := 0; offset < frames; offset++ {
		frame := offset
		if fromEnd {
			frame = frames - offset - 1
		}
		if !pcm16FrameSilent(pcm[frame*frameBytes:(frame+1)*frameBytes], channels, threshold) {
			return offset
		}
	}
	return frames
}

func pcm16FrameSilent(frame []byte, channels int, threshold float64) bool {
	for channel := range channels {
		offset := channel * 2
		sample := int32(int16(binary.LittleEndian.Uint16(frame[offset : offset+2])))
		if sample < 0 {
			sample = -sample
		}
		if float64(sample) > threshold {
			return false
		}
	}
	return true
}
