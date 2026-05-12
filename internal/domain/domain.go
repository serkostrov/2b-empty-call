package domain

import (
	"encoding/json"
	"time"
)

type AudioFile struct {
	Filename    string
	ContentType string
	Bytes       []byte
}

type ASRResult struct {
	Text string
	Raw  any
}

type SummaryResult struct {
	Summary string
	Raw     any
}

type ProcessResult struct {
	Transcription string
	Summary       string
	RawASR        any
	RawGigaChat   any
	Duration      time.Duration
}

type HealthResponse struct {
	Status      string    `json:"status"`
	Service     string    `json:"service"`
	Version     string    `json:"version"`
	Environment string    `json:"environment"`
	Time        time.Time `json:"time"`
}

type ErrorResponse struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

type ProcessingJob struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organization_id"`
	CallID         string          `json:"call_id"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	ErrorCode      *string         `json:"error_code"`
	ErrorMessage   *string         `json:"error_message"`
	ErrorDetails   json.RawMessage `json:"error_details"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type Call struct {
	ID                 string  `json:"id"`
	OrganizationID     string  `json:"organization_id"`
	UserID             string  `json:"user_id"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	AudioFileID        *string `json:"audio_file_id"`
	AnalysisTemplateID *string `json:"analysis_template_id"`
}

type CallFile struct {
	ID               string          `json:"id"`
	OrganizationID   string          `json:"organization_id"`
	CallID           string          `json:"call_id"`
	FileType         string          `json:"file_type"`
	StorageBucket    string          `json:"storage_bucket"`
	StoragePath      string          `json:"storage_path"`
	OriginalFilename *string         `json:"original_filename"`
	MimeType         *string         `json:"mime_type"`
	SizeBytes        *int64          `json:"size_bytes"`
	DurationSeconds  *int            `json:"duration_seconds"`
	MetadataJSON     json.RawMessage `json:"metadata_json"`
	CreatedAt        time.Time       `json:"created_at"`
}

type AnalysisTemplate struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	PromptSystem       string          `json:"prompt_system"`
	PromptUserTemplate string          `json:"prompt_user_template"`
	SchemaJSON         json.RawMessage `json:"schema_json"`
	Version            int             `json:"version"`
	IsDefault          bool            `json:"is_default"`
	IsActive           bool            `json:"is_active"`
}

const (
	JobTypeAnalyzeCall      = "analyze_call"
	JobTypeRegenerateReport = "regenerate_report"

	JobStatusQueued    = "queued"
	JobStatusRunning   = "running"
	JobStatusSuccess   = "success"
	JobStatusError     = "error"
	JobStatusCancelled = "cancelled"

	CallStatusQueued           = "queued"
	CallStatusPreparingAudio   = "preparing_audio"
	CallStatusSentToASR        = "sent_to_asr"
	CallStatusRecognizing      = "recognizing"
	CallStatusRecognized       = "recognized"
	CallStatusAnalyzing        = "analyzing"
	CallStatusGeneratingReport = "generating_report"
	CallStatusReportReady      = "report_ready"
	CallStatusError            = "error"

	FileTypeOriginalAudio = "original_audio"
	// Alternate file_type values used by some apps when inserting call_files (Lovable, etc.).
	FileTypeUpload     = "upload"
	FileTypeSource     = "source"
	FileTypeRecording  = "recording"
	FileTypeAudio      = "audio"
	FileTypeRawASRJSON       = "raw_asr_json"
	FileTypeRawLLMJSON       = "raw_llm_json"
	FileTypeTranscriptionTXT = "transcription_txt"
	FileTypeSummaryTXT       = "summary_txt"
	FileTypeFullAnalysisJSON = "full_analysis_json"

	ReportTypeTranscriptionTXT = "transcription_txt"
	ReportTypeSummaryTXT       = "summary_txt"
	ReportTypeFullAnalysisJSON = "full_analysis_json"
)
