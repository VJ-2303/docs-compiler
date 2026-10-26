package broker

import (
	"time"

	"github.com/google/uuid"
)

type DocumentSubmittedEvent struct {
	DocumentID    uuid.UUID `json:"document_id"`
	SourceContent string    `json:"source_content"`
	Title         string    `json:"title"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

type DocumentCompiledEvent struct {
	DocumentID       uuid.UUID `json:"document_id"`
	S3Key            string    `json:"s3_key"`
	RenderDurationMS int       `json:"render_duration_ms"`
	CompiledAt       time.Time `json:"compiled_at"`
}

type DocumentFailedEvent struct {
	DocumentID   uuid.UUID `json:"document_id"`
	ErrorMessage string    `json:"error_message"`
	FailedAt     time.Time `json:"failed_at"`
}
