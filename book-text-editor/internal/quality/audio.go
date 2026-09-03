package quality

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const pcm16FullScale = 32768.0

type AudioMetrics struct {
	DurationSeconds        float64  `json:"duration_seconds"`
	PeakDBFS               *float64 `json:"peak_dbfs"`
	RMSDBFS                *float64 `json:"rms_dbfs"`
	LUFSI                  *float64 `json:"lufs_i"`
	ClippingPercent        float64  `json:"clipping_percent"`
	SilenceRatio           float64  `json:"silence_ratio"`
	LeadingSilenceSeconds  float64  `json:"leading_silence_seconds"`
	TrailingSilenceSeconds float64  `json:"trailing_silence_seconds"`
	ContainsNaNOrInf       bool     `json:"contains_nan_or_inf"`
}

func AnalyzePCM16(
	pcm []byte,
	sampleRate int,
	channels int,
	silenceThresholdDBFS float64,
) (AudioMetrics, error) {
	if len(pcm) == 0 {
		return AudioMetrics{}, errors.New("PCM payload is empty")
	}
	if sampleRate <= 0 || channels <= 0 {
		return AudioMetrics{}, errors.New("sample rate and channels must be positive")
	}
	if math.IsNaN(silenceThresholdDBFS) || math.IsInf(silenceThresholdDBFS, 0) ||
		silenceThresholdDBFS >= 0 {
		return AudioMetrics{}, errors.New("silence threshold must be a finite negative dBFS value")
	}
	frameBytes := channels * 2
	if len(pcm)%frameBytes != 0 {
		return AudioMetrics{}, fmt.Errorf("PCM payload is not aligned to %d-byte frames", frameBytes)
	}

	frames := len(pcm) / frameBytes
	samples := len(pcm) / 2
	threshold := pcm16FullScale * math.Pow(10, silenceThresholdDBFS/20)
	var peak int32
	var sumSquares float64
	var clipped int
	silentFrames := 0
	leadingSilentFrames := 0
	trailingSilentFrames := 0
	seenNonSilent := false
	currentTrailing := 0

	for frame := range frames {
		frameSilent := true
		for channel := range channels {
			offset := (frame*channels + channel) * 2
			sample := int32(int16(binary.LittleEndian.Uint16(pcm[offset : offset+2])))
			absolute := sample
			if absolute < 0 {
				absolute = -absolute
			}
			if absolute > peak {
				peak = absolute
			}
			if absolute >= 32767 {
				clipped++
			}
			normalized := float64(sample) / pcm16FullScale
			sumSquares += normalized * normalized
			if float64(absolute) > threshold {
				frameSilent = false
			}
		}
		if frameSilent {
			silentFrames++
			currentTrailing++
			if !seenNonSilent {
				leadingSilentFrames++
			}
		} else {
			seenNonSilent = true
			currentTrailing = 0
		}
	}
	trailingSilentFrames = currentTrailing

	metrics := AudioMetrics{
		DurationSeconds:        float64(frames) / float64(sampleRate),
		ClippingPercent:        float64(clipped) * 100 / float64(samples),
		SilenceRatio:           float64(silentFrames) / float64(frames),
		LeadingSilenceSeconds:  float64(leadingSilentFrames) / float64(sampleRate),
		TrailingSilenceSeconds: float64(trailingSilentFrames) / float64(sampleRate),
		ContainsNaNOrInf:       false,
	}
	if peak > 0 {
		value := 20 * math.Log10(float64(peak)/pcm16FullScale)
		metrics.PeakDBFS = &value
	}
	rms := math.Sqrt(sumSquares / float64(samples))
	if rms > 0 {
		value := 20 * math.Log10(rms)
		metrics.RMSDBFS = &value
	}
	return metrics, nil
}

func EncodePCM16WAV(pcm []byte, sampleRate, channels int) ([]byte, error) {
	if len(pcm) == 0 || sampleRate <= 0 || channels <= 0 {
		return nil, errors.New("invalid PCM format")
	}
	blockAlign := channels * 2
	if len(pcm)%blockAlign != 0 {
		return nil, errors.New("PCM payload is not frame-aligned")
	}
	if uint64(len(pcm))+36 > math.MaxUint32 {
		return nil, errors.New("PCM payload is too large for RIFF/WAV")
	}
	byteRate := uint64(sampleRate) * uint64(blockAlign)
	if byteRate > math.MaxUint32 || blockAlign > math.MaxUint16 || channels > math.MaxUint16 {
		return nil, errors.New("PCM format exceeds RIFF/WAV limits")
	}

	result := make([]byte, 44+len(pcm))
	copy(result[0:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(36+len(pcm)))
	copy(result[8:12], "WAVE")
	copy(result[12:16], "fmt ")
	binary.LittleEndian.PutUint32(result[16:20], 16)
	binary.LittleEndian.PutUint16(result[20:22], 1)
	binary.LittleEndian.PutUint16(result[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(result[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(result[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(result[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(result[34:36], 16)
	copy(result[36:40], "data")
	binary.LittleEndian.PutUint32(result[40:44], uint32(len(pcm)))
	copy(result[44:], pcm)
	return result, nil
}
