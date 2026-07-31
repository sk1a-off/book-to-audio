package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxManualReviewReasonLength = 2_000
	maxFragmentAudioPCMBytes    = 16 << 20
)

// FragmentManualReviewDecision is the immutable outcome of a manual review.
type FragmentManualReviewDecision string

const (
	FragmentManualReviewDecisionApproved FragmentManualReviewDecision = "approved"
)

// FragmentManualReviewResource records the warning evidence visible when a
// reviewer manually approved the current audio.
type FragmentManualReviewResource struct {
	ID          string                       `json:"id"`
	FragmentID  string                       `json:"fragment_id"`
	Attempt     int                          `json:"attempt"`
	Decision    FragmentManualReviewDecision `json:"decision"`
	WarningCode string                       `json:"warning_code"`
	STTText     string                       `json:"stt_text"`
	Reason      string                       `json:"reason"`
	CreatedAt   time.Time                    `json:"created_at"`
}

// ApproveFragmentRequest optionally records the reviewer's explanation.
type ApproveFragmentRequest struct {
	Reason string `json:"reason,omitempty"`
}

// ApproveFragmentResponse returns the accepted fragment, the recomputed job,
// and the immutable review appended by this request.
type ApproveFragmentResponse struct {
	Fragment FragmentResource             `json:"fragment"`
	Job      JobResource                  `json:"job"`
	Review   FragmentManualReviewResource `json:"review"`
}

// FragmentManualReviewsResponse contains the ordered approval audit trail.
type FragmentManualReviewsResponse struct {
	FragmentID string                         `json:"fragment_id"`
	Reviews    []FragmentManualReviewResource `json:"reviews"`
}

type fragmentAudioSnapshot struct {
	PCM         []byte
	SampleRate  int
	Channels    int
	SampleWidth int
	DurationMS  int
}

type fragmentApproval struct {
	Fragment FragmentResource
	Job      JobResource
	Review   FragmentManualReviewResource
}

// getFragmentAudio handles GET /v1/fragment/{fragmentID}/audio.wav.
func (s *Server) getFragmentAudio(
	writer http.ResponseWriter,
	request *http.Request,
) {
	fragmentID := request.PathValue("fragmentID")
	audio, ok, err := s.store.fragmentAudio(request.Context(), fragmentID)
	switch {
	case err != nil:
		s.internalStoreError(writer, request, "get fragment audio", err)
		return
	case !ok:
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
			"fragment not found",
		)
		return
	}

	wav, err := encodeFragmentAudioWAV(audio)
	if err != nil {
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"FRAGMENT_AUDIO_NOT_PLAYABLE",
			"fragment has no playable audio",
		)
		return
	}

	escapedID := url.PathEscape(fragmentID)
	writer.Header().Set("Content-Type", "audio/wav")
	writer.Header().Set(
		"Content-Disposition",
		`inline; filename="fragment.wav"; filename*=UTF-8''fragment-`+
			escapedID+`.wav`,
	)
	writer.Header().Set("Content-Length", strconv.Itoa(len(wav)))
	writer.Header().Set(
		"X-Audio-Sample-Rate",
		strconv.Itoa(audio.SampleRate),
	)
	writer.Header().Set("X-Audio-Channels", strconv.Itoa(audio.Channels))
	writer.Header().Set(
		"X-Audio-Sample-Width",
		strconv.Itoa(audio.SampleWidth),
	)
	writer.Header().Set(
		"X-Audio-Duration-Milliseconds",
		strconv.Itoa(audio.DurationMS),
	)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(wav)
}

// approveFragment handles POST /v1/fragment/{fragmentID}/approve.
func (s *Server) approveFragment(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input ApproveFragmentRequest
	if !decodeJSON(writer, request, &input, true) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if len([]rune(input.Reason)) > maxManualReviewReasonLength {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REVIEW_REASON_TOO_LONG",
			"reason is too long",
		)
		return
	}

	fragmentID := request.PathValue("fragmentID")
	approval, err := s.store.approveFragment(
		request.Context(),
		fragmentID,
		s.newID(),
		input.Reason,
		s.now().UTC(),
	)
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
			"fragment not found",
		)
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"FRAGMENT_NOT_APPROVABLE",
			err.Error(),
		)
	case err != nil:
		s.internalStoreError(writer, request, "approve fragment", err)
	default:
		writer.Header().Set(
			"Location",
			"/v1/fragment/"+url.PathEscape(fragmentID)+"/reviews",
		)
		writeJSON(writer, http.StatusCreated, ApproveFragmentResponse{
			Fragment: approval.Fragment,
			Job:      approval.Job,
			Review:   approval.Review,
		})
	}
}

// listFragmentManualReviews handles
// GET /v1/fragment/{fragmentID}/reviews.
func (s *Server) listFragmentManualReviews(
	writer http.ResponseWriter,
	request *http.Request,
) {
	fragmentID := request.PathValue("fragmentID")
	reviews, ok, err := s.store.fragmentManualReviews(
		request.Context(),
		fragmentID,
	)
	switch {
	case err != nil:
		s.internalStoreError(writer, request, "list fragment reviews", err)
	case !ok:
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
			"fragment not found",
		)
	default:
		writeJSON(writer, http.StatusOK, FragmentManualReviewsResponse{
			FragmentID: fragmentID,
			Reviews:    reviews,
		})
	}
}

func encodeFragmentAudioWAV(
	audio fragmentAudioSnapshot,
) ([]byte, error) {
	if len(audio.PCM) == 0 || len(audio.PCM) > maxFragmentAudioPCMBytes {
		return nil, errors.New("PCM payload is empty or exceeds the limit")
	}
	if audio.SampleRate <= 0 || uint64(audio.SampleRate) > math.MaxUint32 {
		return nil, errors.New("invalid sample rate")
	}
	if audio.Channels <= 0 || audio.Channels > math.MaxUint16 {
		return nil, errors.New("invalid channel count")
	}
	if audio.SampleWidth != 2 {
		return nil, errors.New("unsupported sample width")
	}
	if audio.DurationMS < 0 {
		return nil, errors.New("invalid audio duration")
	}
	blockAlign := uint64(audio.Channels) * uint64(audio.SampleWidth)
	if blockAlign > math.MaxUint16 {
		return nil, errors.New("WAV block alignment overflows uint16")
	}

	return encodePCMAsWAV(
		audio.PCM,
		audio.SampleRate,
		audio.Channels,
		audio.SampleWidth,
	)
}
