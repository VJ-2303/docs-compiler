package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	useSSL    bool
}

type Client struct {
	minioClient *minio.Client
	bucket      string
	logger      *slog.Logger
}

func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.useSSL,
	})

	if err != nil {
		return nil, fmt.Errorf("initializing minio client: %w", err)
	}

	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("checking bucket %q: %w", cfg.Bucket, err)
	}

	if !exists {
		err = mc.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{})
		if err != nil {
			return nil, fmt.Errorf("creating bucket %q: %w", cfg.Bucket, err)
		}
		logger.Info("created storage bucket", "bucket", cfg.Bucket)
	} else {
		logger.Info("storage bucket verified", "bucket", cfg.Bucket)
	}
	return &Client{
		minioClient: mc,
		bucket:      cfg.Bucket,
		logger:      logger,
	}, nil
}

func (c *Client) UploadHTML(ctx context.Context, key string, content []byte) error {
	reader := bytes.NewReader(content)
	size := int64(len(content))

	opts := minio.PutObjectOptions{
		ContentType: "text/html; charset=utf-8",
	}
	_, err := c.minioClient.PutObject(ctx, c.bucket, key, reader, size, opts)
	if err != nil {
		return fmt.Errorf("uploading object %q to bucket %q: %w", key, c.bucket, err)
	}
	return nil
}

func (c *Client) Bucket() string {
	return c.bucket
}
