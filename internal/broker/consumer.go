package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vj/docx/internal/compiler"
	"github.com/vj/docx/internal/storage"
)

const (
	ConsumerName       = "compiler-worker"
	SubjectDocCompiled = "docs.compiled"
	SubjectDocFailed   = "docs.failed"
)

type CompilerConsumer struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	consumer jetstream.Consumer
	engine   *compiler.Engine
	storage  *storage.Client
	logger   *slog.Logger
}

func NewCompilerConsumer(
	ctx context.Context,
	natsURL string,
	engine *compiler.Engine,
	storeClient *storage.Client,
	logger *slog.Logger,
) (*CompilerConsumer, error) {
	nc, err := nats.Connect(natsURL, nats.Name("docpub-compiler-worker"))
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
		return nil, fmt.Errorf("fetching stream %q: %w", StreamName, err)
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       ConsumerName,
		Description:   "Compiler service durable worker consumer",
		FilterSubject: SubjectDocSubmitted,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    3,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating durable consumer %q: %w", ConsumerName, err)
	}

	return &CompilerConsumer{
		nc:       nc,
		js:       js,
		consumer: consumer,
		engine:   engine,
		storage:  storeClient,
		logger:   logger,
	}, nil
}

func (c *CompilerConsumer) ProcessMessage(ctx context.Context, msg jetstream.Msg) {
	var event DocumentSubmittedEvent
	err := json.Unmarshal(msg.Data(), &event)
	if err != nil {
		c.logger.Error("failed to unmarshal submission event", "error", err)
		_ = msg.Term()
		return
	}

	c.logger.Info("processing compilation job",
		"document_id", event.DocumentID,
		"title", event.Title,
	)

	result, err := c.engine.Compile(event.Title, event.SourceContent)
	if err != nil {
		c.logger.Error("compilation failed", "document_id", event.DocumentID, "error", err)
		c.publishFailure(ctx, event, err.Error())
		_ = msg.Ack()
		return
	}

	s3Key := fmt.Sprintf("artifacts/%s.html", event.DocumentID)
	err = c.storage.UploadHTML(ctx, s3Key, result.HTMLContent)
	if err != nil {
		c.logger.Error("s3 upload failed", "document_id", event.DocumentID, "error", err)
		_ = msg.Nak()
		return
	}

	compiledEvent := DocumentCompiledEvent{
		DocumentID:       event.DocumentID,
		S3Key:            s3Key,
		RenderDurationMS: result.DurationMS,
		CompiledAt:       time.Now().UTC(),
	}

	data, err := json.Marshal(compiledEvent)
	if err == nil {
		_, pubErr := c.js.Publish(ctx, SubjectDocCompiled, data, jetstream.WithMsgID(s3Key))
		if pubErr != nil {
			c.logger.Error("failed to publish docs.compiled event", "error", pubErr)
		}
	}

	_ = msg.Ack()

	c.logger.Info("completed compilation job",
		"document_id", event.DocumentID,
		"s3_key", s3Key,
		"duration_ms", result.DurationMS,
	)
}

func (c *CompilerConsumer) publishFailure(ctx context.Context, event DocumentSubmittedEvent, reason string) {
	failEvent := DocumentFailedEvent{
		DocumentID:   event.DocumentID,
		ErrorMessage: reason,
		FailedAt:     time.Now().UTC(),
	}
	if data, err := json.Marshal(failEvent); err == nil {
		_, _ = c.js.Publish(ctx, SubjectDocFailed, data)
	}
}

func (c *CompilerConsumer) Start(ctx context.Context) (jetstream.ConsumeContext, error) {
	c.logger.Info("starting compiler worker consume loop", "consumer", ConsumerName)

	consumeCtx, err := c.consumer.Consume(func(msg jetstream.Msg) {
		c.ProcessMessage(ctx, msg)
	})
	if err != nil {
		return nil, fmt.Errorf("starting consume context: %w", err)
	}

	return consumeCtx, nil
}

func (c *CompilerConsumer) Close() error {
	c.logger.Info("draining compiler nats connection")
	return c.nc.Drain()
}
