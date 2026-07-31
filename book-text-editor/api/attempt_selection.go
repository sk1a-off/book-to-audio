package api

import (
	"math"
	"slices"
)

const attemptScoreEpsilon = 0.0001

// chooseBestFragmentResult compares the current persisted audio with a newly
// generated candidate. The active fragment always keeps the best Whisper match
// seen so far; every candidate is still recorded in attempts for auditability.
func chooseBestFragmentResult(
	expected string,
	current fragmentResult,
	candidate fragmentResult,
) (fragmentResult, bool) {
	candidate.WarningCode = effectiveFragmentWarning(expected, candidate)
	if len(current.AudioPCM) == 0 {
		return cloneFragmentResult(candidate), true
	}
	current.WarningCode = effectiveFragmentWarning(expected, current)

	currentScore := fragmentAttemptQuality(expected, current)
	candidateScore := fragmentAttemptQuality(expected, candidate)
	switch {
	case candidateScore > currentScore+attemptScoreEpsilon:
		return cloneFragmentResult(candidate), true
	case currentScore > candidateScore+attemptScoreEpsilon:
		return cloneFragmentResult(current), false
	case candidate.WarningCode == "" && current.WarningCode != "":
		return cloneFragmentResult(candidate), true
	case current.WarningCode == "" && candidate.WarningCode != "":
		return cloneFragmentResult(current), false
	case len(candidate.WorkerNotes) < len(current.WorkerNotes):
		return cloneFragmentResult(candidate), true
	default:
		// Stable tie-breaking avoids replacing good audio merely because a retry
		// finished later.
		return cloneFragmentResult(current), false
	}
}

func fragmentAttemptQuality(expected string, result fragmentResult) float64 {
	if len(result.AudioPCM) == 0 {
		return 0
	}
	score := transcriptSimilarityScore(expected, result.STTText)
	if result.WarningCode == "audio_warning" {
		score -= 0.08
	}
	if len(result.WorkerNotes) > 0 {
		score -= math.Min(0.08, float64(len(result.WorkerNotes))*0.02)
	}
	if result.WarningCode == "" {
		score += 0.01
	}
	return clampUnit(score)
}

func effectiveFragmentWarning(expected string, result fragmentResult) string {
	if result.WarningCode != "" {
		return result.WarningCode
	}
	if len(result.AudioPCM) == 0 {
		return ""
	}
	if !transcriptMatches(expected, result.STTText) {
		return "transcript_mismatch"
	}
	if len(result.WorkerNotes) > 0 {
		return "audio_warning"
	}
	return ""
}

func fragmentStatusForResult(result fragmentResult) FragmentStatus {
	if len(result.AudioPCM) == 0 {
		return FragmentStatusFailed
	}
	if result.WarningCode == "" {
		return FragmentStatusReady
	}
	return FragmentStatusWarning
}

func cloneFragmentResult(result fragmentResult) fragmentResult {
	result.AudioPCM = slices.Clone(result.AudioPCM)
	result.WorkerNotes = slices.Clone(result.WorkerNotes)
	return result
}

func fragmentResultFromStored(
	resource FragmentResource,
	audio []byte,
	sampleRate, channels, sampleWidth, durationMS int,
	sttLanguage string,
	workerNotes []string,
) fragmentResult {
	return fragmentResult{
		AudioPCM:    slices.Clone(audio),
		SampleRate:  sampleRate,
		Channels:    channels,
		SampleWidth: sampleWidth,
		DurationMS:  durationMS,
		STTText:     resource.STTText,
		STTLanguage: sttLanguage,
		WarningCode: resource.WarningCode,
		WorkerNotes: slices.Clone(workerNotes),
	}
}
