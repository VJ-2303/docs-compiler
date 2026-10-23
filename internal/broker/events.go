package broker

import (
	"time"
	"uuid"
)

type DocumentSubmittedEvent struct {
	DocumentID    uuid.UUID `json:"document_id"`
	SourceContent string    `json:"source_content"`
	SubmittedAt   time.Time `json:"submitted_at"`
}
