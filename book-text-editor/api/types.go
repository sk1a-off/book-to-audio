package api

import "time"

type BookResource struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Authors        []string  `json:"authors"`
	Format         string    `json:"format"`
	ChaptersCount  int       `json:"chapters_count"`
	FragmentsCount int       `json:"fragments_count"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

type VoiceResource struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Mode             string    `json:"mode"`
	Format           string    `json:"format"`
	ContentType      string    `json:"content_type"`
	SizeBytes        int64     `json:"size_bytes"`
	HasReferenceText bool      `json:"has_reference_text"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
}

type VoicesResponse struct {
	Voices []VoiceResource `json:"voices"`
}

type JobStatus string

const (
	JobStatusQueued                JobStatus = "queued"
	JobStatusRunning               JobStatus = "running"
	JobStatusCompleted             JobStatus = "completed"
	JobStatusCompletedWithWarnings JobStatus = "completed_with_warnings"
	JobStatusFailed                JobStatus = "failed"
)

type JobResource struct {
	ID                 string             `json:"id"`
	BookID             string             `json:"book_id"`
	VoiceID            string             `json:"voice_id"`
	Status             JobStatus          `json:"status"`
	FragmentsCount     int                `json:"fragments_count"`
	FragmentsPending   int                `json:"fragments_pending"`
	FragmentsReady     int                `json:"fragments_ready"`
	FragmentsWarnings  int                `json:"fragments_warnings"`
	FragmentsFailed    int                `json:"fragments_failed"`
	GenerationSettings GenerationSettings `json:"generation_settings"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

type FragmentStatus string

const (
	FragmentStatusPending    FragmentStatus = "pending"
	FragmentStatusGenerating FragmentStatus = "generating"
	FragmentStatusReady      FragmentStatus = "ready"
	FragmentStatusWarning    FragmentStatus = "warning"
	FragmentStatusFailed     FragmentStatus = "failed"
)

type FragmentResource struct {
	ID            string         `json:"id"`
	JobID         string         `json:"job_id"`
	ChapterNumber int            `json:"chapter_number"`
	Ordinal       int            `json:"ordinal"`
	Text          string         `json:"text"`
	STTText       string         `json:"stt_text,omitempty"`
	Status        FragmentStatus `json:"status"`
	WarningCode   string         `json:"warning_code,omitempty"`
	Error         string         `json:"error,omitempty"`
	Attempt       int            `json:"attempt"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type GenerationResponse struct {
	Book  BookResource  `json:"book"`
	Voice VoiceResource `json:"voice"`
	Job   JobResource   `json:"job"`
	JobID string        `json:"job_id"`
}

type WarningsResponse struct {
	JobID     string             `json:"job_id"`
	Fragments []FragmentResource `json:"fragments"`
}

type EditFragmentRequest struct {
	NewText string `json:"new_text"`
}

type RetryWarningsRequest struct {
	FragmentIDs []string `json:"fragment_ids,omitempty"`
}

type LegacyRetryRequest struct {
	JobID       string   `json:"job_id"`
	FragmentIDs []string `json:"fragment_ids,omitempty"`
}

type RetryResponse struct {
	JobID           string    `json:"job_id"`
	Status          JobStatus `json:"status"`
	FragmentsQueued int       `json:"fragments_queued"`
}

type jobTask struct {
	JobID            string
	FragmentIDs      []string
	IsRetry          bool
	PreviousStatuses map[string]FragmentStatus
	Settings         GenerationSettings
}

type chapterRecord struct {
	Number   int
	Title    string
	Segments []string
}

type voicePayload struct {
	Resource      VoiceResource
	ReferenceText string
	Audio         []byte
}

type fragmentWorkItem struct {
	Resource FragmentResource
	Voice    voicePayload
}

type fragmentResult struct {
	AudioPCM    []byte
	SampleRate  int
	Channels    int
	SampleWidth int
	DurationMS  int
	STTText     string
	STTLanguage string
	WarningCode string
	WorkerNotes []string
}

type archiveSnapshot struct {
	Book      BookResource
	Voice     VoiceResource
	Job       JobResource
	Fragments []archiveFragment
}

type archiveFragment struct {
	Resource     FragmentResource
	ChapterTitle string
	AudioPCM     []byte
	SampleRate   int
	Channels     int
	SampleWidth  int
	DurationMS   int
	WorkerNotes  []string
}
