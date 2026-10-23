package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRecordNotFound = errors.New("record not found")

type DocumentStatus string

const (
	StatusPending   DocumentStatus = "PENDING"
	StatusCompiling DocumentStatus = "COMPILING"
	StatusCompleted DocumentStatus = "COMPLETED"
	StatusFailed    DocumentStatus = "FAILED"
)

type Document struct {
	ID               uuid.UUID      `json:"id"`
	Title            string         `json:"title"`
	Status           DocumentStatus `json:"status"`
	SourceContent    string         `json:"source_content"`
	S3Key            *string        `json:"s3_key,omitempty"`
	RenderDurationMS int            `json:"render_duration_ms"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

type DocumentStore struct {
	pool *pgxpool.Pool
}

func NewDocumentStore(pool *pgxpool.Pool) *DocumentStore {
	return &DocumentStore{pool: pool}
}

func (s *DocumentStore) Insert(ctx context.Context, doc *Document) error {
	query := `
		INSERT INTO documents (title, status, source_content)
		VALUES ($1, $2, $3)
		RETURNING id, created_at, updated_at
	`

	if doc.Status == "" {
		doc.Status = StatusPending
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	err := s.pool.QueryRow(
		ctx,
		query,
		doc.Title,
		doc.Status,
		doc.SourceContent,
	).Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)

	if err != nil {
		return fmt.Errorf("inserting document: %w", err)
	}

	return nil
}

func (s *DocumentStore) GetByID(ctx context.Context, id uuid.UUID) (*Document, error) {
	query := `
		SELECT id, title, status, source_content, s3_key, render_duration_ms, created_at, updated_at
		FROM documents
		WHERE id = $1
	`

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var doc Document
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&doc.ID,
		&doc.Title,
		&doc.Status,
		&doc.SourceContent,
		&doc.S3Key,
		&doc.RenderDurationMS,
		&doc.CreatedAt,
		&doc.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, fmt.Errorf("fetching document by id: %w", err)
	}

	return &doc, nil
}

func (s *DocumentStore) UpdateStatus(ctx context.Context, id uuid.UUID, status DocumentStatus, s3Key *string, durationMS int) error {
	query := `
		UPDATE documents
		SET status = $2, s3_key = $3, render_duration_ms = $4, updated_at = NOW()
		WHERE id = $1
	`

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	result, err := s.pool.Exec(ctx, query, id, status, s3Key, durationMS)
	if err != nil {
		return fmt.Errorf("updating document status: %w", err)
	}

	if result.RowsAffected() == 0 {
		return ErrRecordNotFound
	}

	return nil
}
