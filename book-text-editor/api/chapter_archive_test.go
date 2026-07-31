package api

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

func TestChapterAudioResourcesExposeDirectFLACOnlyWhenReady(t *testing.T) {
	fragments := []FragmentViewResource{
		{
			FragmentResource: FragmentResource{
				ID: "f-2", ChapterNumber: 2, Ordinal: 3,
				Status: FragmentStatusGenerating,
			},
			ChapterTitle: "Продолжение",
		},
		{
			FragmentResource: FragmentResource{
				ID: "f-1", ChapterNumber: 1, Ordinal: 1,
				Status: FragmentStatusReady,
			},
			ChapterTitle: "Начало", DurationMS: 250, AudioAvailable: true,
		},
		{
			FragmentResource: FragmentResource{
				ID: "f-1b", ChapterNumber: 1, Ordinal: 2,
				Status: FragmentStatusReady,
			},
			ChapterTitle: "Начало", DurationMS: 350, AudioAvailable: true,
		},
	}

	got, err := chapterAudioResources("job id", fragments)
	if err != nil {
		t.Fatalf("chapterAudioResources() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("chapters = %#v", got)
	}
	if !got[0].Ready || got[0].FragmentsReady != 2 ||
		got[0].AudioURL != "/v1/job/job%20id/chapters/1/audio.flac" {
		t.Fatalf("ready chapter = %#v", got[0])
	}
	if got[1].Ready || got[1].AudioURL != "" || got[1].FragmentsPending != 1 {
		t.Fatalf("pending chapter = %#v", got[1])
	}
}

func TestBuildChapterAudioFLACReturnsNativeStream(t *testing.T) {
	pcm := make([]byte, 16*2)
	for index := 0; index < 16; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:index*2+2], uint16(index))
	}
	snapshot := archiveSnapshot{
		Fragments: []archiveFragment{
			{
				Resource: FragmentResource{
					ID: "fragment-1", ChapterNumber: 1, Ordinal: 1,
					Text: "Текст.", Status: FragmentStatusReady,
				},
				ChapterTitle: "Начало",
				AudioPCM:     pcm, SampleRate: 24_000, Channels: 1, SampleWidth: 2,
			},
		},
	}

	audio, filename, err := buildChapterAudioFLAC(snapshot)
	if err != nil {
		t.Fatalf("buildChapterAudioFLAC() error = %v", err)
	}
	if !strings.HasPrefix(string(audio), "fLaC") {
		t.Fatalf("audio does not start with FLAC marker: %q", audio[:min(4, len(audio))])
	}
	if filename != "character_0001_Начало.flac" {
		t.Fatalf("filename = %q", filename)
	}
}

func TestPositiveChapterNumber(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "+1", "one"} {
		if _, err := positiveChapterNumber(value); err == nil {
			t.Errorf("positiveChapterNumber(%q) returned nil error", value)
		}
	}
	if got, err := positiveChapterNumber("42"); err != nil || !reflect.DeepEqual(got, 42) {
		t.Fatalf("positiveChapterNumber(42) = %d, %v", got, err)
	}
}
