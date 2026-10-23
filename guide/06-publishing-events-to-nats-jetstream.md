# Chapter 6: Publishing Events to NATS JetStream

Welcome to Chapter 6. In Chapter 5, we built the HTTP transport layer of our API Service using Go 1.22+ `net/http.ServeMux`, structured logging with `log/slog`, panic recovery middleware, and input validation. When a user submits a Markdown document to `POST /v1/documents`, the API Service saves a `PENDING` record in PostgreSQL and returns `202 Accepted` in ~15ms.

However, saving a `PENDING` record to the database is only half of the ingestion journey. We must now alert our background workers that a new compilation job is ready.

In this chapter, we will build the event publishing subsystem in `internal/broker/publisher.go`. Using **NATS JetStream**—the lightweight, high-performance, pure-Go distributed streaming system—we will connect to the broker, ensure our durable stream topology exists, and publish persistent `docs.submitted` events that survive server restarts.

---

## 1. Objectives & Overview

In this chapter, you will:
- Understand the NATS JetStream mental model: Connections, Streams, Subjects, and Consumers.
- Learn why NATS JetStream provides massive operational simplicity over complex distributed message brokers.
- Understand message persistence on disk and why the JetStream `FileStorage` engine guarantees durability.
- Build a thread-safe, resilient event publisher in `internal/broker/publisher.go`.
- Understand the "dual-write" challenge (database commit vs event publish) and implement an effective failure-compensation strategy.
- Wire NATS publishing directly into the `POST /v1/documents` handler.
- Verify message arrival in NATS JetStream using HTTP monitoring endpoints and CLI utilities.

### Event Ingestion & Publishing Architecture

```
[ HTTP Client ] ── 1. POST /v1/documents (JSON) ──► [ API Service (:8080) ]
                                                            │
                                  2. Insert PENDING         │ 3. Publish "docs.submitted"
                                  (PostgreSQL)              │ (JetStream FileStorage)
                                     │                      ▼
                                     ▼          ┌───────────────────────────┐
                            ┌─────────────────┐ │ NATS JetStream            │
                            │   PostgreSQL    │ │ Stream: "DOCS"            │
                            │ documents table │ │ Subjects: ["docs.*"]      │
                            └─────────────────┘ └─────────────┬─────────────┘
                                                              │
                                                              │ Subject: "docs.submitted"
                                                              ▼
                                                ┌───────────────────────────┐
                                                │ Stream Message Store      │
                                                │ Persisted to disk volume  │
                                                │ in /data                  │
                                                └─────────────┬─────────────┘
                                                              │
                                                              ▼
                                                (Awaiting Compiler Worker in Ch 7-8)
```

---

## 2. Conceptual Deep Dive: The NATS & JetStream Mental Model

If you are new to NATS, its architecture is simpler and more elegant than traditional enterprise brokers. However, you must understand the distinction between **Core NATS** and **JetStream**, as well as the four core primitives that govern how messages flow.

---

### Core NATS vs NATS JetStream

At its foundation, NATS operates in two distinct tiers:

```
+─────────────────────────────────────────────────────────────────────────+
| NATS Server (Single Go Process, port 4222)                              |
|                                                                         |
|  ┌───────────────────────────────────────────────────────────────────┐  |
|  │ 1. Core NATS Engine (At-Most-Once / Fire-and-Forget)              │  |
|  │ - Ultra-fast in-memory pub/sub message router                     │  |
|  │ - No disk storage, no sequence tracking, no persistence           │  |
|  │ - If no subscriber is connected when a message arrives, it drops  │  |
|  └─────────────────────────────────┬─────────────────────────────────┘  |
|                                    │                                    |
|                                    ▼                                    |
|  ┌───────────────────────────────────────────────────────────────────┐  |
|  │ 2. JetStream Persistence Engine (At-Least-Once / Durable)         │  |
|  │ - Enabled via the -js flag in Docker                              │  |
|  │ - Intercepts messages published on configured subjects            │  |
|  │ - Commits messages to append-only disk files (FileStorage)        │  |
|  │ - Tracks message sequence numbers, consumer state, and ACKs       │  |
|  └───────────────────────────────────────────────────────────────────┘  |
+─────────────────────────────────────────────────────────────────────────+
```

1. **Core NATS** is a high-speed, in-memory publish/subscribe router. If an API service publishes a message and no worker is actively connected to receive it, the message is immediately discarded. This is ideal for metrics or chat, but unacceptable for task processing.
2. **JetStream** is the built-in persistence and streaming engine layered on top of Core NATS. When JetStream is enabled (`-js`), it provides **at-least-once delivery**, persistent disk storage, consumer offset tracking, message deduplication, and automatic retries.

---

### The 4 Core Primitives of JetStream

NATS JetStream is composed of four fundamental building blocks:

```
[ Publisher ] ──► (1. Subject) ──► [ 2. Stream (Disk) ] ──► [ 3. Consumer ] ──► [ Worker ]
  (API Service)   "docs.submitted"   "DOCS" [1, 2, 3...]    "compiler-worker"  (Compiler)
```

#### 1. Subjects (Addressing & Routing)
A **Subject** is a simple, dot-delimited string that acts as the destination address for a message (e.g., `docs.submitted`, `docs.compiled`, `docs.failed`).
- Subjects are hierarchical: `category.resource.action`.
- Subjects cost zero memory to declare: you don't "create" a subject in NATS—you simply publish to it.
- **Wildcard Matching**:
  - `*` (Single token): `docs.*` matches `docs.submitted` and `docs.compiled`, but **not** `docs.submitted.retry`.
  - `>` (Multi-token greedy): `docs.>` matches any number of trailing tokens (`docs.submitted.retry.v2`).

In our publishing pipeline, our stream listens to `docs.*`, allowing it to capture all document lifecycle events.

#### 2. Streams (The Append-Only Commit Log)
A **Stream** is an append-only commit log stored on disk or in memory.
- A stream is configured to listen to one or more subject patterns (e.g., `Subjects: []string{"docs.*"}`).
- When a publisher emits a message to `docs.submitted`, JetStream intercepts the message, assigns it a globally incrementing 64-bit integer sequence number (`seq: 1`, `seq: 2`), and writes it to disk.
- **Storage Engines**:
  - `FileStorage`: Every message is committed to disk in memory-mapped files. This is what we use: if Docker or the NATS container restarts, our jobs remain safe on disk.
  - `MemoryStorage`: High-speed ephemeral streaming; messages are lost if the broker restarts.
- **Retention Policies**:
  - `LimitsPolicy` (Default): Retains messages until configured limits (max age, max messages, or max bytes) are reached.
  - `WorkQueuePolicy`: Acts like a queue—once any worker acknowledges a message, JetStream immediately deletes it from the stream.

#### 3. Consumers (Stateful Views on a Stream)
A **Consumer** is **not** your Go client code; a Consumer is a stateful configuration object managed by the NATS server.
- The consumer tracks which messages in the stream have been delivered, which are currently being processed, and which have been acknowledged (`ACK`).
- **Push vs. Pull Consumers**:
  - *Push Consumer*: The NATS server pushes messages over the network to a client subject as fast as they arrive.
  - *Pull Consumer*: The client explicitly asks NATS for a batch of messages (`Fetch(10)`). In Go microservices, **Pull Consumers are the gold standard** because they give workers complete control over memory, concurrency, and backpressure.
- **Durable vs. Ephemeral**:
  - *Ephemeral*: Created temporarily and deleted when the client disconnects.
  - *Durable*: Given a permanent name (e.g., `Durable: "compiler-worker"`). Even if the Compiler Service restarts, the durable consumer maintains its cursor position on the stream log.
- **Delivery Guarantees**:
  - `AckPolicy: Explicit`: The worker must call `msg.Ack()`. If the worker crashes or takes longer than `AckWait` (e.g., 30s), JetStream automatically re-delivers the message.
  - `MaxDeliver`: The maximum number of delivery attempts before JetStream stops retrying, isolating "poison-pill" messages.

#### 4. Messages, Acks, and Deduplication
When publishing to JetStream, publishing is **synchronous and confirmed**:
```go
ack, err := js.Publish(ctx, "docs.submitted", data, jetstream.WithMsgID(docID))
```
- The broker writes the message to disk, increments the stream sequence, and returns a `PubAck` struct containing `ack.Stream` (`DOCS`) and `ack.Sequence` (`1`).
- If disk write fails or the context times out, `Publish()` returns an error.
- **Message Deduplication (`jetstream.WithMsgID`)**:
  - JetStream maintains a rolling deduplication window (default: 2 minutes).
  - If a network blip causes your API service to publish the same document UUID twice, JetStream detects the duplicate `Msg-Id`, ignores the second insert, and returns the original acknowledgement without storing a duplicate job.

---

### Cheat-Sheet: The NATS JetStream Concepts

| Concept | What It Does | In Our Markdown Pipeline |
| :--- | :--- | :--- |
| **Subject** | Hierarchical message routing address | `docs.submitted`, `docs.compiled`, `docs.failed` |
| **Stream** | Append-only persistent disk log for subjects | Stream `DOCS` listening on `docs.*` |
| **Storage Engine** | Where messages live on the broker | `jetstream.FileStorage` (persisted to Docker volume) |
| **Consumer** | Server-side read cursor tracking delivered messages | Durable pull consumer `compiler-worker` |
| **Ack Policy** | How consumers confirm job completion | `AckExplicit`: worker calls `msg.Ack()` after S3 upload |
| **Deduplication** | Rejection of duplicate messages within a time window | `jetstream.WithMsgID(doc.ID.String())` |

---

### Thread Safety in Go: Why NATS is Ergonomic
In many message broker client libraries, network sockets or channels cannot be shared safely across concurrent goroutines without complex mutex wrappers.

In contrast, the NATS Go client (`*nats.Conn` and `jetstream.JetStream`) is **completely thread-safe by design**. The driver manages internal lock-free connection ring buffers and pipelined socket writes. Any number of concurrent HTTP handler goroutines can invoke `p.js.Publish(ctx, ...)` simultaneously without mutexes or race conditions.

---

### The Dual-Write Problem & Failure Compensation
When our API Service receives a document submission, it must update state across two separate distributed systems:
1. Write a `PENDING` row to **PostgreSQL**.
2. Publish a `docs.submitted` event to **NATS JetStream**.

Because these systems do not share a distributed two-phase commit transaction (which would harm latency and availability), a failure can occur between step 1 and step 2:

```
[ Client ] ──► 1. Save to PostgreSQL (SUCCESS)
                     │
                     ▼
               2. Publish to NATS (FAILED - Network partition / broker reboot)
                     │
                     ▼
             PostgreSQL row is stuck in PENDING forever! (Orphaned state)
```

To prevent orphaned documents without adding the overhead of complex Change Data Capture (CDC) outbox pollers, we employ **Synchronous Publish with Database Compensation (Fail Fast)**:
- We first insert the row into PostgreSQL.
- We immediately attempt to publish the event to NATS JetStream.
- If NATS rejects the message or times out, we catch the error, execute a compensating update to mark the document status as `FAILED`, and return an HTTP `500 Internal Server Error` to the user.
- The client knows immediately that ingestion failed and can retry without leaving forgotten `PENDING` records in the database.

---

## 3. Step-by-Step Implementation

We will organize our message broker code in `internal/broker`:
- `internal/broker/events.go`: The strongly-typed event schemas.
- `internal/broker/publisher.go`: NATS connection lifecycle, stream declaration, and event publishing.
- Updates to `internal/api`: Injecting the publisher and triggering events on document ingestion.

---

### Step 3.1: Defining the Event Schema

Create `internal/broker/events.go`.

This file defines the contract between the API Service (producer) and the Compiler Service (consumer).

```go
// internal/broker/events.go
package broker

import (
	"time"

	"github.com/google/uuid"
)

// DocumentSubmittedEvent represents the payload dispatched when a document is ingested.
type DocumentSubmittedEvent struct {
	DocumentID    uuid.UUID `json:"document_id"`
	Title         string    `json:"title"`
	SourceContent string    `json:"source_content"`
	SubmittedAt   time.Time `json:"submitted_at"`
}
```

**Line-by-line explanation:**
- `DocumentID uuid.UUID`: Uniquely identifies the document in PostgreSQL so background workers can tag rendered HTML artifacts.
- `SourceContent string`: The raw CommonMark text to compile. Passing this in the event means the Compiler Service **never needs to query PostgreSQL**, strictly maintaining the Database Boundary Rule.
- `SubmittedAt time.Time`: UTC timestamp recording when the submission was accepted.

---

### Step 3.2: Connecting to NATS and Initializing the Publisher

Create `internal/broker/publisher.go`. We build this in two focused parts adhering to our 30–60 line rule.

#### Part A: Struct Definition and Stream Declaration

```go
// internal/broker/publisher.go
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
	// StreamName is the JetStream stream storing publishing pipeline events.
	StreamName = "DOCS"
	// SubjectDocSubmitted is the subject for incoming document compilation jobs.
	SubjectDocSubmitted = "docs.submitted"
)

// Publisher manages the NATS connection and JetStream publishing context.
type Publisher struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	logger *slog.Logger
}

// NewPublisher establishes a connection to NATS, initializes JetStream, and ensures stream existence.
func NewPublisher(natsURL string, logger *slog.Logger) (*Publisher, error) {
	// Connect to NATS broker
	nc, err := nats.Connect(natsURL, nats.Name("docpub-api-publisher"))
	if err != nil {
		return nil, fmt.Errorf("connecting to nats at %s: %w", natsURL, err)
	}

	// Create modern JetStream context
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating jetstream context: %w", err)
	}

	// Declare durable stream capturing all docs.* subjects
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        StreamName,
		Description: "Markdown Publishing Pipeline Document Stream",
		Subjects:    []string{"docs.*"},
		Storage:     jetstream.FileStorage, // Persist messages to disk
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
```

**Line-by-line explanation:**
- `nats.Connect(natsURL, ...)`: Establishes a persistent TCP connection to NATS. It automatically handles background reconnects with exponential backoff if the broker restarts.
- `jetstream.New(nc)`: Uses the modern JetStream API from `nats.go` v1.35+.
- `js.CreateOrUpdateStream(...)`: Idempotently creates the stream if it does not exist, or updates its configuration safely.
- `Subjects: []string{"docs.*"}`: Tells the stream to capture any message published to subjects starting with `docs.` (e.g. `docs.submitted`, `docs.compiled`, `docs.failed`).
- `Storage: jetstream.FileStorage`: Ensures messages written to this stream are committed to disk on our persistent Docker volume.

---

### Step 3.3: Publishing Persistent Events

Add the `PublishDocumentSubmitted` and `Close` methods to `internal/broker/publisher.go`:

```go
// PublishDocumentSubmitted encodes and dispatches a persistent docs.submitted event.
func (p *Publisher) PublishDocumentSubmitted(ctx context.Context, event DocumentSubmittedEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling document submitted event: %w", err)
	}

	// Publish message to JetStream subject with context timeout support
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

// Close gracefully flushes pending buffers and closes the NATS connection.
func (p *Publisher) Close() error {
	p.logger.Info("draining and closing nats connection")
	return p.nc.Drain()
}
```

**Line-by-line explanation:**
- `jetstream.WithMsgID(...)`: Deduplication header! NATS JetStream uses this ID to prevent duplicate message ingestion if network retries occur.
- `p.js.Publish(ctx, ...)`: Synchronously publishes to JetStream and awaits the broker's commit acknowledgement (`ack`). If the broker cannot write to disk, an error is returned.
- `ack.Sequence`: JetStream returns the exact sequence number assigned to this message in the stream log.
- `p.nc.Drain()`: Graceful shutdown. Waits for in-flight outbound buffers to flush to the socket before tearing down the connection.

---

### Step 3.4: Injecting the Publisher into `Application`

Update `internal/api/api.go` to include the `Publisher` as an explicit dependency.

```go
// internal/api/api.go
package api

import (
	"log/slog"

	"github.com/vj/docx/internal/broker"
	"github.com/vj/docx/internal/store"
)

// Config holds runtime configuration options for the API service.
type Config struct {
	Port    int
	Env     string
	NatsURL string
}

// Application holds the operational dependencies for our HTTP handlers and middleware.
type Application struct {
	Config    Config
	Store     *store.DocumentStore
	Publisher *broker.Publisher
	Logger    *slog.Logger
}

// New constructs an Application instance with injected dependencies.
func New(cfg Config, store *store.DocumentStore, pub *broker.Publisher, logger *slog.Logger) *Application {
	return &Application{
		Config:    cfg,
		Store:     store,
		Publisher: pub,
		Logger:    logger,
	}
}
```

**Line-by-line explanation:**
- `Publisher *broker.Publisher`: Added directly to the `Application` struct. Handlers access it via `app.Publisher`.
- `New(...)`: Requires the caller to supply an active publisher instance, ensuring the API server cannot boot with missing message dependencies.

---

### Step 3.5: Publishing Events in `createDocumentHandler`

Update `createDocumentHandler` in `internal/api/handlers.go` to publish the `docs.submitted` event right after the database row is created.

```go
// createDocumentHandler handles POST /v1/documents.
func (app *Application) createDocumentHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title         string `json:"title"`
		SourceContent string `json:"source_content"`
	}

	err := app.readJSON(w, r, &input)
	if err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	// Validate user input
	v := validator.New()
	v.Check(validator.NotBlank(input.Title), "title", "must be provided")
	v.Check(validator.MaxChars(input.Title, 255), "title", "must not exceed 255 characters")
	v.Check(validator.NotBlank(input.SourceContent), "source_content", "must be provided")

	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	// 1. Persist document to database with PENDING status
	doc := &store.Document{
		Title:         input.Title,
		SourceContent: input.SourceContent,
		Status:        store.StatusPending,
	}

	err = app.Store.Insert(r.Context(), doc)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	// 2. Publish docs.submitted event to NATS JetStream
	event := broker.DocumentSubmittedEvent{
		DocumentID:    doc.ID,
		Title:         doc.Title,
		SourceContent: doc.SourceContent,
		SubmittedAt:   doc.CreatedAt,
	}

	err = app.Publisher.PublishDocumentSubmitted(r.Context(), event)
	if err != nil {
		app.logError(r, fmt.Errorf("publishing event: %w", err))

		// Compensation: mark status as FAILED so document does not linger in PENDING forever
		_ = app.Store.UpdateStatus(r.Context(), doc.ID, store.StatusFailed, nil, 0)

		app.serverErrorResponse(w, r, errors.New("failed to enqueue document for processing"))
		return
	}

	// 3. Return 202 Accepted to client
	err = app.writeJSON(w, http.StatusAccepted, envelope{"document": doc}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
```

**Line-by-line explanation:**
- `event := broker.DocumentSubmittedEvent{...}`: Assembles the event using the generated UUID and timestamp returned from `store.Insert()`.
- `app.Publisher.PublishDocumentSubmitted(r.Context(), event)`: Dispatches the event to NATS JetStream.
- **Compensation logic**: If publishing fails (broker unavailable), we immediately call `app.Store.UpdateStatus(..., store.StatusFailed, ...)`. This prevents orphan records and informs the user via `500 Server Error` that queuing failed.

---

### Step 3.6: Wiring NATS Lifecycle in `cmd/api/main.go`

Update `cmd/api/main.go` to parse the NATS URL flag, establish the publisher, and drain connections cleanly on shutdown.

```go
// cmd/api/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/vj/docx/internal/api"
	"github.com/vj/docx/internal/broker"
	"github.com/vj/docx/internal/database"
	"github.com/vj/docx/internal/store"
)

func main() {
	var cfg api.Config
	var dsn string

	flag.IntVar(&cfg.Port, "port", 8080, "API server port")
	flag.StringVar(&cfg.Env, "env", "development", "Environment (development|staging|production)")
	flag.StringVar(&dsn, "db-dsn", "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable", "PostgreSQL DSN")
	flag.StringVar(&cfg.NatsURL, "nats-url", "nats://localhost:4222", "NATS JetStream broker URL")
	flag.Parse()

	// Initialize structured logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Connect to PostgreSQL database pool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbPool, err := database.New(ctx, database.Config{
		DSN:             dsn,
		MaxConns:        25,
		MinConns:        5,
		MaxConnLifetime: 15 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	})
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer dbPool.Close()

	logger.Info("database connection pool established")

	// Initialize NATS JetStream publisher
	publisher, err := broker.NewPublisher(cfg.NatsURL, logger)
	if err != nil {
		logger.Error("failed to connect to nats jetstream", "error", err)
		os.Exit(1)
	}
	defer publisher.Close()

	logger.Info("nats jetstream publisher initialized and stream declared")

	// Instantiate document store and API application container
	docStore := store.NewDocumentStore(dbPool)
	app := api.New(cfg, docStore, publisher, logger)

	// Configure HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      app.Routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	logger.Info("starting server", "addr", srv.Addr, "env", cfg.Env)

	err = srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
```

**Line-by-line explanation:**
- `flag.StringVar(&cfg.NatsURL, "nats-url", ...)`: Configurable via command-line flags or environment variables in Docker.
- `defer publisher.Close()`: Ensures all buffered outbound messages are cleanly flushed (`nc.Drain()`) before the process exits.
- `logger.Info(...)`: Structured log confirmation that the `DOCS` stream is active on the broker.

---

## 4. Terminal Commands & Infrastructure Execution

### Step 4.1: Download the NATS Go Driver
Install the official NATS Go client:

```bash
go get github.com/nats-io/nats.go
go mod tidy
```

### Step 4.2: Verify Infrastructure Containers
Ensure NATS JetStream and PostgreSQL are running and healthy:

```bash
make ps
```

Confirm that both `docpub-postgres` and `docpub-nats` report `Up (healthy)`.

---

## 5. Verification Playbook

Follow these checks to verify that event publishing works end-to-end.

### Check 1: Verify NATS Ports & Health Endpoint
Confirm that NATS is listening on port 4222 (client protocol) and port 8222 (HTTP monitor) on Arch Linux:

```bash
ss -tulpn | grep -E '4222|8222'
```

**Expected output:**
```
tcp  LISTEN 0  128  0.0.0.0:4222   0.0.0.0:*
tcp  LISTEN 0  128  0.0.0.0:8222   0.0.0.0:*
```

Test the NATS HTTP health endpoint:
```bash
curl -i http://localhost:8222/healthz
```

**Expected output:**
```http
HTTP/1.1 200 OK
Content-Type: application/json
Date: Fri, 23 Oct 2026 17:01:00 GMT
Content-Length: 17

{"status":"ok"}
```

---

### Check 2: Boot the API Service
Start the API service:

```bash
go run ./cmd/api
```

**Expected terminal output (JSON logs):**
```json
{"time":"2026-10-23T17:02:00.100Z","level":"INFO","msg":"database connection pool established"}
{"time":"2026-10-23T17:02:00.120Z","level":"INFO","msg":"nats jetstream publisher initialized and stream declared"}
{"time":"2026-10-23T17:02:00.121Z","level":"INFO","msg":"starting server","addr":":8080","env":"development"}
```

---

### Check 3: Submit a Markdown Document
In a second terminal window, submit a document via `POST /v1/documents`:

```bash
curl -i -X POST http://localhost:8080/v1/documents \
  -H "Content-Type: application/json" \
  -d '{
    "title": "NATS JetStream Pipeline",
    "source_content": "# High Performance Streaming\n\nTesting pure Go message streaming with NATS JetStream."
  }'
```

**Expected HTTP response:**
```http
HTTP/1.1 202 Accepted
Content-Type: application/json
Date: Fri, 23 Oct 2026 17:03:00 GMT
Content-Length: 268

{
  "document": {
    "id": "a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d",
    "title": "NATS JetStream Pipeline",
    "status": "PENDING",
    "source_content": "# High Performance Streaming\n\nTesting pure Go message streaming with NATS JetStream.",
    "render_duration_ms": 0,
    "created_at": "2026-10-23T17:03:00.234567Z",
    "updated_at": "2026-10-23T17:03:00.234567Z"
  }
}
```

---

### Check 4: Inspect Server Structured Logs
Examine the API server terminal. You should see two structured log events:
1. `published document submitted event` (from the NATS publisher with stream name and sequence number).
2. `request processed` with status `202` (from `logRequest` middleware).

```json
{"time":"2026-10-23T17:03:00.240Z","level":"INFO","msg":"published document submitted event","document_id":"a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d","subject":"docs.submitted","stream":"DOCS","seq":1}
{"time":"2026-10-23T17:03:00.241Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":202,"duration_ms":11}
```

---

### Check 5: Inspect JetStream Stream State via HTTP Monitoring API
Query NATS JetStream's monitoring endpoint to inspect the `DOCS` stream:

```bash
curl -s http://localhost:8222/jsz?streams=1 | grep -E '"(name|messages|bytes|first_seq|last_seq)"'
```

**Expected output:**
```json
        "name": "DOCS",
        "messages": 1,
        "first_seq": 1,
        "last_seq": 1
```

Notice that the `DOCS` stream shows **`"messages": 1`**! The message was safely committed to persistent disk storage and is waiting for our Compiler Service worker to consume it in Chapter 8.

---

## 6. Common Gotchas & Troubleshooting

1. **JetStream Not Enabled on Server (`nats: JetStream not enabled`):**
   - *Symptom*: Calling `jetstream.New(nc)` or creating a stream fails with `503 JetStream not enabled for account`.
   - *Cause*: NATS was booted without the `-js` command-line flag.
   - *Fix*: Check `deploy/docker/docker-compose.yml` to verify that `command: ["-js", "-sd", "/data", "-m", "8222"]` is present on the `nats` service.

2. **Stream Subject Mismatch:**
   - *Symptom*: Publishing succeeds, but the message count on the stream remains 0.
   - *Cause*: The published subject does not match the subjects configured on the stream (e.g. stream configured for `doc.*` while publisher sends to `docs.submitted`).
   - *Fix*: Verify that `Subjects: []string{"docs.*"}` matches the published subject hierarchy.

3. **Broker Connection Refused on Startup:**
   - *Symptom*: API server fails to start with `connecting to nats at nats://localhost:4222: dial tcp 127.0.0.1:4222: connect: connection refused`.
   - *Cause*: NATS container is still initializing or stopped.
   - *Fix*: Check container status with `make ps`. Wait until the container transitions to `Up (healthy)`.

---

## 7. Summary & Next Steps

### Key Takeaways
- NATS JetStream provides high-performance, disk-persisted event streaming in a tiny pure-Go container requiring under 20MB of memory.
- Streams capture messages published to subjects (such as `docs.submitted`), persisting them to disk via `FileStorage`.
- The NATS Go driver is natively thread-safe, removing the need for synchronization mutexes across concurrent HTTP handlers.
- Synchronous publish with database compensation ensures that if queuing fails, the record is flagged as `FAILED` rather than lingering in `PENDING` indefinitely.
- The `SourceContent` is passed inside the event payload, enforcing the Database Boundary Rule: workers do not need database credentials.

### Self-Check Questions Before Advancing
1. What is the fundamental difference between Core NATS pub/sub and NATS JetStream?
2. If a stream listens to `docs.*`, will it capture a message published to `docs.submitted`? Will it capture `docs.submitted.retry`?
3. What is the difference between a Push Consumer and a Pull Consumer, and why are Pull Consumers preferred for background workers?
4. Why is `Storage: jetstream.FileStorage` chosen over `jetstream.MemoryStorage` for our publishing pipeline?
5. How does `jetstream.WithMsgID()` help prevent duplicate message processing?
6. Why does passing `SourceContent` in `DocumentSubmittedEvent` preserve the Database Boundary Rule?

---

This completes **Phase 2: The API Service (HTTP & Event Ingestion)**.

In **[Chapter 7: MinIO Storage and Markdown Engine](07-minio-storage-and-markdown-engine.md)**, we enter **Phase 3: The Compiler Worker & MinIO Storage**. We will build the compilation core: configuring the MinIO S3 client, parsing CommonMark with `goldmark`, generating sanitized HTML, and applying standalone CSS stylesheets.
