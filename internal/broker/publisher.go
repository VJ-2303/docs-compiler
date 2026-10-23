package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	StreamName          = "DOCS"
	SubjectDocSubmitted = "docs.submitted"
)

type Publisher struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	logger *slog.Logger
}

func NewPublisher(natsURL string, logger *slog.Logger) (*Publisher, error) {
	nc, err := nats.Connect(natsURL, nats.Name("docpub-api-publisher"))
	if err != nil {
		return nil, fmt.Errorf("connecting to nats at %s: %w", natsURL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating jetstream context: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        StreamName,
		Description: "Markdown Publishing Pipeline Document Stream",
		Subjects:    []string{"docs.*"},
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
	})

	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("declaring jetstream stream %s: %w", StreamName, err)
	}
	return &Publisher{
		nc:     nc,
		js:     js,
		logger: logger,
	}, nil
}

func (p *Publisher) PublishDocumentSubmitted(ctx context.Context, event DocumentSubmittedEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling document submitted event: %w", err)
	}
	ack, err := p.js.Publish(ctx, SubjectDocSubmitted, body, jetstream.WithMsgID(event.DocumentID.String()))
	if err != nil {
		return fmt.Errorf("publishing message to %s: %w", SubjectDocSubmitted, err)
	}

	p.logger.Info("published document submitted event",
		"document_id", event.DocumentID,
		"subject", SubjectDocSubmitted,
		"stream", ack.Stream,
		"seq", ack.Sequence,
	)
	return nil
}

func (p *Publisher) Close() error {
	p.logger.Info("draining and closing nats connection")
	return p.nc.Drain()
}
