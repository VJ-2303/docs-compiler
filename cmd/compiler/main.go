package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vj/docx/internal/broker"
	"github.com/vj/docx/internal/compiler"
	"github.com/vj/docx/internal/storage"
)

func main() {
	var natsURL, s3Endpoint, s3AccessKey, s3SecretKey, s3Bucket string
	var useSSL bool

	flag.StringVar(&natsURL, "nats-url", "nats://localhost:4222", "NATS JetStream broker URL")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "localhost:9000", "MinIO S3 endpoint")
	flag.StringVar(&s3AccessKey, "s3-access-key", "minioadmin", "MinIO access key")
	flag.StringVar(&s3SecretKey, "s3-secret-key", "minioadmin", "MinIO secret key")
	flag.StringVar(&s3Bucket, "s3-bucket", "docs-rendered", "MinIO target bucket")
	flag.BoolVar(&useSSL, "s3-ssl", false, "Use SSL for S3 connection")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine, err := compiler.New()
	if err != nil {
		logger.Error("failed to initialize compiler engine", "error", err)
		os.Exit(1)
	}

	s3Client, err := storage.New(ctx, storage.Config{
		Endpoint:  s3Endpoint,
		AccessKey: s3AccessKey,
		SecretKey: s3SecretKey,
		Bucket:    s3Bucket,
		UseSSL:    useSSL,
	}, logger)
	if err != nil {
		logger.Error("failed to connect to s3 storage", "error", err)
		os.Exit(1)
	}

	consumer, err := broker.NewCompilerConsumer(ctx, natsURL, engine, s3Client, logger)
	if err != nil {
		logger.Error("failed to initialize compiler consumer", "error", err)
		os.Exit(1)
	}
	defer consumer.Close()

	consumeCtx, err := consumer.Start(ctx)
	if err != nil {
		logger.Error("failed to start consumer loop", "error", err)
		os.Exit(1)
	}

	logger.Info("compiler service running; awaiting jobs", "bucket", s3Bucket)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	logger.Info("shutdown signal received; draining in-flight jobs", "signal", sig.String())

	consumeCtx.Stop()
	time.Sleep(2 * time.Second)

	logger.Info("compiler service stopped cleanly")
}
