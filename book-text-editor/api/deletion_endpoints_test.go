package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestDeleteCompletedJobThenVoiceEndpoints(t *testing.T) {
	fixture := newEndpointTestFixture(t)
	fixture.stt.SetMismatchFirst(false)
	handler := fixture.server.Handler()
	book, voice := endpointTestUploadInputs(t, handler)

	generationResponse := endpointTestRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/generate/book/"+book.ID+"/voice/"+voice.ID,
		strings.NewReader(`{"automatic_warning_retries":0}`),
		"application/json",
	)
	if generationResponse.Code != http.StatusAccepted {
		t.Fatalf("generate status=%d body=%s", generationResponse.Code, generationResponse.Body)
	}
	var generation GenerationResponse
	endpointTestDecodeJSON(t, generationResponse, &generation)
	endpointTestWaitForJob(t, handler, generation.JobID, JobStatusCompleted)

	voiceInUse := endpointTestRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/voices/"+voice.ID,
		nil,
		"",
	)
	if voiceInUse.Code != http.StatusConflict ||
		!strings.Contains(voiceInUse.Body.String(), "VOICE_DELETE_NOT_ALLOWED") {
		t.Fatalf("delete referenced voice status=%d body=%s", voiceInUse.Code, voiceInUse.Body)
	}

	deletedJob := endpointTestRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/job/"+generation.JobID,
		nil,
		"",
	)
	if deletedJob.Code != http.StatusNoContent || deletedJob.Body.Len() != 0 {
		t.Fatalf("delete job status=%d body=%s", deletedJob.Code, deletedJob.Body)
	}
	missingJob := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/job/"+generation.JobID,
		nil,
		"",
	)
	if missingJob.Code != http.StatusNotFound {
		t.Fatalf("get deleted job status=%d body=%s", missingJob.Code, missingJob.Body)
	}
	repeatedJobDelete := endpointTestRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/job/"+generation.JobID,
		nil,
		"",
	)
	if repeatedJobDelete.Code != http.StatusNotFound {
		t.Fatalf("delete missing job status=%d body=%s", repeatedJobDelete.Code, repeatedJobDelete.Body)
	}

	deletedVoice := endpointTestRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/voices/"+voice.ID,
		nil,
		"",
	)
	if deletedVoice.Code != http.StatusNoContent || deletedVoice.Body.Len() != 0 {
		t.Fatalf("delete voice status=%d body=%s", deletedVoice.Code, deletedVoice.Body)
	}
	missingVoice := endpointTestRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/voices/"+voice.ID,
		nil,
		"",
	)
	if missingVoice.Code != http.StatusNotFound {
		t.Fatalf("get deleted voice status=%d body=%s", missingVoice.Code, missingVoice.Body)
	}
}
