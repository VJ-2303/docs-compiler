package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vj/docx/internal/store"
)

type ResultsConsumer struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	consumer jetstream.Consumer
	store    *store.DocumentStore
	logger   *slog.Logger
}

func NewResultsConsumer(ctx context.Context, natsURL string, docStore *store.DocumentStore, logger *slog.Logger) (*ResultsConsumer, error) {
	nc, err := nats.Connect(natsURL, nats.Name("docpub-api-results-consumer"))
	if err != nil {
		return nil, fmt.Errorf("connecting to nats: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating jetstream context: %w", err)
	}

	stream, err := js.Stream(ctx, StreamName)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("fetching stream %s: %w", StreamName, err)
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:        "api-results-consumer",
		Description:    "API service consumer updating PostgreSQL document states",
		FilterSubjects: []string{SubjectDocCompiled, SubjectDocFailed},
		AckPolicy:      jetstream.AckExplicitPolicy,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("declaring results consumer: %w", err)
	}

	return &ResultsConsumer{
		nc:       nc,
		js:       js,
		consumer: consumer,
		store:    docStore,
		logger:   logger,
	}, nil
}

func (rc *ResultsConsumer) Start(ctx context.Context) (jetstream.ConsumeContext, error) {
	rc.logger.Info("starting api results consumer")

	return rc.consumer.Consume(func(msg jetstream.Msg) {
		switch msg.Subject() {
		case SubjectDocCompiled:
			var event DocumentCompiledEvent
			if err := json.Unmarshal(msg.Data(), &event); err == nil {
				updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err = rc.store.UpdateStatus(updateCtx, event.DocumentID, store.StatusCompleted, &event.S3Key, event.RenderDurationMS)
				cancel()

				if err != nil {
					rc.logger.Error("failed to update document status to COMPLETED", "error", err)
					_ = msg.Nak()
					return
				}
				rc.logger.Info("reconciled document status to COMPLETED",
					"document_id", event.DocumentID,
					"s3_key", event.S3Key,
				)
			}
		case SubjectDocFailed:
			var event DocumentFailedEvent
			if err := json.Unmarshal(msg.Data(), &event); err == nil {
				updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = rc.store.UpdateStatus(updateCtx, event.DocumentID, store.StatusFailed, nil, 0)
				cancel()
				rc.logger.Warn("reconciled document status to FAILED", "document_id", event.DocumentID)
			}
		}
		_ = msg.Ack()
	})
}

func (rc *ResultsConsumer) Close() error {
	return rc.nc.Drain()
}
