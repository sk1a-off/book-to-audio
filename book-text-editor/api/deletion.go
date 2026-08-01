package api

import (
	"errors"
	"net/http"
	"strings"
)

func (s *Server) deleteJobEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
) {
	jobID := strings.TrimSpace(request.PathValue("jobID"))
	deleted, err := s.store.deleteJobForUser(request.Context(), jobID)
	switch {
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"JOB_DELETE_NOT_ALLOWED",
			"active generation or LLM rewrite must finish before the job can be deleted",
		)
	case err != nil:
		s.internalStoreError(writer, request, "delete generation job", err)
	case !deleted:
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"JOB_NOT_FOUND",
			"job not found",
		)
	default:
		s.logger.Info(
			"generation job deleted",
			"request_id", requestID(request.Context()),
			"job_id", jobID,
		)
		writer.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) deleteVoiceEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
) {
	voiceID := strings.TrimSpace(request.PathValue("voiceID"))
	deleted, err := s.store.deleteVoice(request.Context(), voiceID)
	switch {
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"VOICE_DELETE_NOT_ALLOWED",
			"voice is referenced by a generation job; delete every job using it first",
		)
	case err != nil:
		s.internalStoreError(writer, request, "delete voice", err)
	case !deleted:
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"VOICE_NOT_FOUND",
			"voice not found",
		)
	default:
		s.logger.Info(
			"voice deleted",
			"request_id", requestID(request.Context()),
			"voice_id", voiceID,
		)
		writer.WriteHeader(http.StatusNoContent)
	}
}
