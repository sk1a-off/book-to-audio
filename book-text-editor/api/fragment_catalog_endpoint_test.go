package api

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

type blockingCatalogTTS struct {
	started chan struct{}
	release chan struct{}
}

func (f *blockingCatalogTTS) Generate(
	ctx context.Context,
	request TTSRequest,
) (TTSResult, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return TTSResult{}, ctx.Err()
	case <-f.release:
	}

	pcm := make([]byte, 16*2)
	for index := 0; index < 16; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:index*2+2], uint16(index))
	}
	return TTSResult{
		RequestID:   request.RequestID,
		AudioPCM:    pcm,
		SampleRate:  24_000,
		Channels:    1,
		SampleWidth: 2,
		DurationMS:  1,
	}, nil
}

func TestFragmentCatalogIsAvailableWhileGenerationRuns(t *testing.T) {
	tts := &blockingCatalogTTS{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	ids := &endpointTestIDGenerator{}
	server, err := NewServer(Dependencies{
		TTS: tts,
		STT: &endpointTestSTT{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
		ID:            ids.New,
		WorkerTimeout: time.Second,
		QueueSize:     4,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer server.Close()

	handler := server.Handler()
	book, voice := endpointTestUploadInputs(t, handler)
	response := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		nil,
		"",
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("generate status = %d, body = %s", response.Code, response.Body)
	}
	var generation GenerationResponse
	endpointTestDecodeJSON(t, response, &generation)

	select {
	case <-tts.started:
	case <-time.After(time.Second):
		t.Fatal("TTS did not start")
	}

	catalogResponse := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID+"/chapters",
		nil,
		"",
	)
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body = %s", catalogResponse.Code, catalogResponse.Body)
	}
	var catalog JobChaptersResponse
	endpointTestDecodeJSON(t, catalogResponse, &catalog)
	if len(catalog.Chapters) != 1 || len(catalog.Fragments) != 2 {
		t.Fatalf("catalog = %+v", catalog)
	}
	if catalog.Chapters[0].Ready || catalog.Chapters[0].AudioURL != "" {
		t.Fatalf("running chapter unexpectedly downloadable: %+v", catalog.Chapters[0])
	}
	if catalog.Fragments[0].Status != FragmentStatusGenerating {
		t.Fatalf("first fragment status = %q", catalog.Fragments[0].Status)
	}

	close(tts.release)
	endpointTestWaitForJob(t, handler, generation.JobID, JobStatusCompleted)
}
