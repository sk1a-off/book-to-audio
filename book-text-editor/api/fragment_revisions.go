package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxRevisionReasonLength = 2_000

// FragmentTextRevisionSource describes how a text revision was created.
type FragmentTextRevisionSource string

const (
	FragmentTextRevisionSourceOriginal FragmentTextRevisionSource = "original"
	FragmentTextRevisionSourceManual   FragmentTextRevisionSource = "manual"
	FragmentTextRevisionSourceAI       FragmentTextRevisionSource = "ai"
	FragmentTextRevisionSourceRestore  FragmentTextRevisionSource = "restore"
)

// FragmentTextRevisionResource is an immutable version of fragment text.
type FragmentTextRevisionResource struct {
	ID               string                     `json:"id"`
	FragmentID       string                     `json:"fragment_id"`
	RevisionNumber   int                        `json:"revision_number"`
	Source           FragmentTextRevisionSource `json:"source"`
	Text             string                     `json:"text"`
	ParentRevisionID *string                    `json:"parent_revision_id"`
	ModelID          *string                    `json:"model_id"`
	ModelRevision    *string                    `json:"model_revision"`
	Prompt           *string                    `json:"prompt"`
	Reason           *string                    `json:"reason"`
	CreatedAt        time.Time                  `json:"created_at"`
}

// FragmentRevisionsResponse contains the complete ordered text history.
type FragmentRevisionsResponse struct {
	FragmentID        string                         `json:"fragment_id"`
	CurrentRevisionID string                         `json:"current_revision_id"`
	Revisions         []FragmentTextRevisionResource `json:"revisions"`
}

// RestoreFragmentRevisionRequest optionally records why a version was restored.
type RestoreFragmentRevisionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// RestoreFragmentRevisionResponse returns both the new current fragment and
// the immutable restore revision that was appended.
type RestoreFragmentRevisionResponse struct {
	Fragment FragmentResource             `json:"fragment"`
	Revision FragmentTextRevisionResource `json:"revision"`
}

type fragmentRevisionHistory struct {
	CurrentRevisionID string
	Revisions         []FragmentTextRevisionResource
}

type fragmentRevisionMutation struct {
	Fragment FragmentResource
	Revision FragmentTextRevisionResource
}

func (s *Server) listFragmentRevisions(
	writer http.ResponseWriter,
	request *http.Request,
) {
	fragmentID := request.PathValue("fragmentID")
	history, ok, err := s.store.fragmentRevisions(
		request.Context(),
		fragmentID,
	)
	switch {
	case err != nil:
		s.internalStoreError(writer, request, "list fragment revisions", err)
	case !ok:
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"FRAGMENT_NOT_FOUND",
			"fragment not found",
		)
	default:
		writeJSON(writer, http.StatusOK, FragmentRevisionsResponse{
			FragmentID:        fragmentID,
			CurrentRevisionID: history.CurrentRevisionID,
			Revisions:         history.Revisions,
		})
	}
}

func (s *Server) restoreFragmentRevision(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input RestoreFragmentRevisionRequest
	if !decodeJSON(writer, request, &input, true) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if len([]rune(input.Reason)) > maxRevisionReasonLength {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REVISION_REASON_TOO_LONG",
			"reason is too long",
		)
		return
	}

	fragmentID := request.PathValue("fragmentID")
	selectedRevisionID := request.PathValue("revisionID")
	if selectedRevisionID == "" {
		writeProblem(
			writer,
			request,
			http.StatusUnprocessableEntity,
			"REVISION_ID_REQUIRED",
			"revision id is required",
		)
		return
	}

	mutation, err := s.store.restoreFragmentRevision(
		request.Context(),
		fragmentID,
		selectedRevisionID,
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
	case errors.Is(err, errRevisionNotFound):
		writeProblem(
			writer,
			request,
			http.StatusNotFound,
			"REVISION_NOT_FOUND",
			"fragment revision not found",
		)
	case errors.Is(err, errConflict):
		writeProblem(
			writer,
			request,
			http.StatusConflict,
			"FRAGMENT_BUSY",
			err.Error(),
		)
	case err != nil:
		s.internalStoreError(writer, request, "restore fragment revision", err)
	default:
		writer.Header().Set(
			"Location",
			"/v1/fragment/"+url.PathEscape(fragmentID)+
				"/revisions",
		)
		writeJSON(writer, http.StatusCreated, RestoreFragmentRevisionResponse{
			Fragment: mutation.Fragment,
			Revision: mutation.Revision,
		})
	}
}
