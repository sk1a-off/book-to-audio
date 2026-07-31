package api

import (
	"bytes"
	"encoding/binary"
	"time"
)

// Shared archive fixtures live in a dedicated file because both the full-book
// exporter tests and the direct chapter-download tests use the same immutable
// snapshot. Keeping them here prevents one test file from accidentally deleting
// helpers required by another.
func chapterArchiveTestSnapshot() archiveSnapshot {
	updatedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	return archiveSnapshot{
		Book: BookResource{ID: "book-1", Title: "Книга"},
		Voice: VoiceResource{
			ID:   "voice-1",
			Name: "Голос",
		},
		Job: JobResource{
			ID:             "job-1",
			BookID:         "book-1",
			VoiceID:        "voice-1",
			Status:         JobStatusCompleted,
			FragmentsCount: 3,
			FragmentsReady: 3,
			UpdatedAt:      updatedAt,
		},
		Fragments: []archiveFragment{
			chapterArchiveTestFragment(
				"fragment-2", 2, 2, "Продолжение", 300,
				chapterArchiveTestPCM(2), updatedAt,
			),
			chapterArchiveTestFragment(
				"fragment-3", 1, 3, "Начало", 250,
				chapterArchiveTestPCM(3), updatedAt,
			),
			chapterArchiveTestFragment(
				"fragment-1", 1, 1, "Начало", 200,
				chapterArchiveTestPCM(1), updatedAt,
			),
		},
	}
}

func chapterArchiveTestPCM(base int16) []byte {
	const samples = 16
	result := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		binary.LittleEndian.PutUint16(
			result[index*2:index*2+2],
			uint16(base+int16(index)),
		)
	}
	return result
}

func chapterArchiveTestFragment(
	id string,
	chapterNumber, ordinal int,
	title string,
	durationMS int,
	pcm []byte,
	updatedAt time.Time,
) archiveFragment {
	return archiveFragment{
		Resource: FragmentResource{
			ID:            id,
			JobID:         "job-1",
			ChapterNumber: chapterNumber,
			Ordinal:       ordinal,
			Text:          "Текст",
			STTText:       "Текст",
			Status:        FragmentStatusReady,
			Attempt:       1,
			UpdatedAt:     updatedAt,
		},
		ChapterTitle: title,
		AudioPCM:     bytes.Clone(pcm),
		SampleRate:   24_000,
		Channels:     1,
		SampleWidth:  2,
		DurationMS:   durationMS,
	}
}
