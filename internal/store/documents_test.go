package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/vj/docx/internal/database"
	"github.com/vj/docx/internal/store"
)

func TestDocumentStore_CRUD(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, database.Config{
		DSN:             "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable",
		MaxConns:        5,
		MinConns:        2,
		MaxConnLifetime: 5 * time.Minute,
		MaxConnIdleTime: 1 * time.Minute,
	})
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	defer pool.Close()

	docStore := store.NewDocumentStore(pool)

	doc := &store.Document{
		Title:         "Microservices in Go",
		SourceContent: "# Introduction\nBuilding clean systems.",
	}

	err = docStore.Insert(ctx, doc)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	if doc.ID.String() == "" {
		t.Errorf("expected generated UUID, got empty")
	}

	fetched, err := docStore.GetByID(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}

	if fetched.Title != doc.Title {
		t.Errorf("expected title %q, got %q", doc.Title, fetched.Title)
	}
	if fetched.Status != store.StatusPending {
		t.Errorf("expected status PENDING, got %q", fetched.Status)
	}

	s3Key := "artifacts/" + doc.ID.String() + ".html"
	err = docStore.UpdateStatus(ctx, doc.ID, store.StatusCompleted, &s3Key, 142)
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	updated, err := docStore.GetByID(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetByID after update failed: %v", err)
	}

	if updated.Status != store.StatusCompleted {
		t.Errorf("expected status COMPLETED, got %q", updated.Status)
	}
	if updated.S3Key == nil || *updated.S3Key != s3Key {
		t.Errorf("expected S3Key %q, got %v", s3Key, updated.S3Key)
	}
}
