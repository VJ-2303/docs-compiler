package storage_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/vj/docx/internal/storage"
)

func TestStorage_UploadHTML(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := storage.Config{
		Endpoint:  "localhost:9000",
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Bucket:    "docs-rendered",
		UseSSL:    false,
	}

	client, err := storage.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("failed to initialize s3 storage client: %v", err)
	}

	key := "artifacts/test-verify.html"
	content := []byte("<!DOCTYPE html><html><body><h1>Verification Successful</h1></body></html>")

	err = client.UploadHTML(ctx, key, content)
	if err != nil {
		t.Fatalf("UploadHTML failed: %v", err)
	}
}
