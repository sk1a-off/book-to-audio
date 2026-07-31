package api

import "testing"

func TestChooseBestFragmentResultKeepsBestWhisperAttempt(t *testing.T) {
	t.Parallel()
	expected := "Путник медленно вошёл в старый дом и закрыл тяжёлую дверь."
	current := fragmentResult{
		AudioPCM: []byte{1, 0, 2, 0}, SampleRate: 24000, Channels: 1, SampleWidth: 2,
		STTText:     "Путник вошел в старый дом и закрыл дверь",
		WarningCode: "transcript_mismatch",
	}
	worse := fragmentResult{
		AudioPCM: []byte{3, 0, 4, 0}, SampleRate: 24000, Channels: 1, SampleWidth: 2,
		STTText:     "Утром поезд покинул город",
		WarningCode: "transcript_mismatch",
	}
	selected, candidateSelected := chooseBestFragmentResult(expected, current, worse)
	if candidateSelected || selected.STTText != current.STTText || selected.AudioPCM[0] != 1 {
		t.Fatalf("worse candidate was selected: %+v candidate=%t", selected, candidateSelected)
	}

	better := fragmentResult{
		AudioPCM: []byte{5, 0, 6, 0}, SampleRate: 24000, Channels: 1, SampleWidth: 2,
		STTText: expected,
	}
	selected, candidateSelected = chooseBestFragmentResult(expected, current, better)
	if !candidateSelected || selected.STTText != expected || selected.AudioPCM[0] != 5 {
		t.Fatalf("better candidate was not selected: %+v candidate=%t", selected, candidateSelected)
	}
}

func TestAttemptQualityPenalizesWorkerAudioWarningOnTie(t *testing.T) {
	t.Parallel()
	expected := "Точный текст для проверки."
	clean := fragmentResult{AudioPCM: []byte{1, 0}, STTText: expected}
	warning := fragmentResult{
		AudioPCM: []byte{2, 0}, STTText: expected,
		WarningCode: "audio_warning", WorkerNotes: []string{"clipping"},
	}
	if fragmentAttemptQuality(expected, clean) <= fragmentAttemptQuality(expected, warning) {
		t.Fatal("clean attempt did not outrank audio warning")
	}
}
