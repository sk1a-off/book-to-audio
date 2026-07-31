package api

import (
	"context"
	"testing"
)

func TestChapterAudioCompleteWithWarningIsDownloadable(t *testing.T) {
	t.Parallel()
	fragments := []FragmentViewResource{
		{
			FragmentResource: FragmentResource{ID: "f1", ChapterNumber: 1, Ordinal: 1, Status: FragmentStatusReady},
			ChapterTitle:     "Глава", AudioAvailable: true, DurationMS: 10,
		},
		{
			FragmentResource: FragmentResource{ID: "f2", ChapterNumber: 1, Ordinal: 2, Status: FragmentStatusWarning, WarningCode: "transcript_mismatch"},
			ChapterTitle:     "Глава", AudioAvailable: true, DurationMS: 12,
		},
	}
	chapters, err := chapterAudioResources("job-1", fragments)
	if err != nil {
		t.Fatalf("chapterAudioResources() error=%v", err)
	}
	if len(chapters) != 1 || !chapters[0].AudioComplete || !chapters[0].Ready ||
		chapters[0].FragmentsVoiced != 2 || chapters[0].ReviewRequired != 1 ||
		chapters[0].AudioURL == "" || chapters[0].ReviewURL == "" {
		t.Fatalf("chapter=%+v", chapters)
	}
}

func TestMemoryArchiveAllowsWarningsWhenEveryFragmentHasAudio(t *testing.T) {
	t.Parallel()
	store, _ := fragmentWorkflowMemoryFixture()
	second := store.fragments["fragment-2"]
	second.Resource.Status = FragmentStatusWarning
	second.Resource.WarningCode = "transcript_mismatch"
	second.Resource.STTText = "другая расшифровка"
	second.AudioPCM = []byte{0, 0, 1, 0}
	second.SampleRate = 24_000
	second.Channels = 1
	second.SampleWidth = 2
	second.DurationMS = 1
	store.jobs["job-1"].Resource.Status = JobStatusCompletedWithWarnings
	store.jobs["job-1"].Resource.FragmentsPending = 0
	store.jobs["job-1"].Resource.FragmentsWarnings = 1

	snapshot, err := store.archiveSnapshot(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("archiveSnapshot() error=%v", err)
	}
	if len(snapshot.Fragments) != 2 || snapshot.Fragments[1].Resource.Status != FragmentStatusWarning {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestMemoryChapterDownloadAllowsSelectedWarningAudioWhileJobRuns(t *testing.T) {
	t.Parallel()
	store, _ := fragmentWorkflowMemoryFixture()
	for _, id := range []string{"fragment-1", "fragment-2"} {
		fragment := store.fragments[id]
		fragment.Resource.Status = FragmentStatusWarning
		fragment.Resource.WarningCode = "transcript_mismatch"
		fragment.Resource.STTText = "приблизительная расшифровка"
		fragment.AudioPCM = []byte{0, 0, 1, 0}
		fragment.SampleRate = 24_000
		fragment.Channels = 1
		fragment.SampleWidth = 2
		fragment.DurationMS = 1
	}
	store.jobs["job-1"].Resource.Status = JobStatusRunning

	snapshot, err := store.downloadableChapterSnapshot(
		context.Background(),
		"job-1",
		1,
	)
	if err != nil {
		t.Fatalf("downloadableChapterSnapshot() error=%v", err)
	}
	if len(snapshot.Fragments) != 2 {
		t.Fatalf("snapshot fragments=%d, want 2", len(snapshot.Fragments))
	}
}
