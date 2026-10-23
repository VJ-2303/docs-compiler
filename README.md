# Markdown Document Publishing Pipeline

A clean, production-grade event-driven microservices system in Go on Arch Linux:
- Synchronous REST API (`net/http`)
- Asynchronous task distribution & event feedback (NATS JetStream)
- Isolated relational state (PostgreSQL with `pgx/v5` & `golang-migrate`)
- Object storage for rendered artifacts (MinIO / S3)
- Pure-Go Markdown compilation (`goldmark`)
- Containerization & Kubernetes deployment (Docker Compose & `k3d`)

Written following the pedagogical principles of **Alex Edwards** (*Let's Go* and *Let's Go Further*): standard library first, zero magic, explicit dependency injection, and clean code.

---

## The Complete Application Flow (Simplified)

```
[ User ] ──── 1. POST /v1/documents ───► [ API Service ] ──── 2. Insert PENDING ───► [ PostgreSQL ]
                                                │
                                        3. Publish "docs.submitted"
                                                │
                                                ▼
                                         [ NATS JetStream ]
                                                │
                                        4. Consume "docs.submitted"
                                                │
                                                ▼
[ User ] ◄─── 8. GET /v1/documents/{id} ◄─── [ Compiler Service ]
               (See COMPLETED status)           │
                                        5. Render styled HTML
                                        6. Upload to MinIO ──► [ MinIO (S3) ]
                                        7. Publish "docs.compiled" ──► [ NATS JetStream ]
                                                                            │
                [ API Service ] ◄──── Consume "docs.compiled" ───────────────┘
                      │
                      └────── Update status to COMPLETED ────► [ PostgreSQL ]
```

### 1. Ingestion Phase (~15ms)
- **User sends Markdown**: `POST /v1/documents` with `{ "title": "...", "content": "# ..." }`.
- **API Service validates & persists**: Stores a `PENDING` record in PostgreSQL and generates a unique `document_id`.
- **API Service publishes to stream**: Dispatches a `docs.submitted` event to NATS JetStream.
- **Immediate response**: User receives `202 Accepted` with the `document_id` in milliseconds without blocking.

### 2. Compilation Phase (Background Worker)
- **Compiler pulls job**: An idle worker in the Compiler Service consumes `docs.submitted` from NATS JetStream.
- **Compiles Markdown**: Parses CommonMark using `goldmark`, generates sanitized HTML, and wraps it in a responsive styled template.
- **Uploads to MinIO (S3)**: Streams the compiled HTML document into the MinIO bucket `docs-rendered` under `artifacts/{id}.html`.
- **Publishes completion event**: Publishes `docs.compiled` (with S3 key and duration) to NATS JetStream, then acknowledges (`ACK`) the message.

### 3. Reconciliation & Retrieval Phase
- **API updates database**: The API Service consumes `docs.compiled` from NATS JetStream and updates PostgreSQL status to `COMPLETED`.
- **User queries status**: User sends `GET /v1/documents/{document_id}`.
- **Instant retrieval**: API queries PostgreSQL and returns status, render duration, and direct MinIO download link.

---

## Master Teaching Guide

The complete step-by-step 11-chapter curriculum lives in [`guide/`](guide/README.md):

1. [Chapter 1: System Design and Microservice Architecture](guide/01-system-design-and-architecture.md) *(Completed)*
2. [Chapter 2: Local Infrastructure with Docker Compose](guide/02-local-infrastructure-docker-compose.md) *(Completed)*
3. [Chapter 3: Database Migrations and Connection Pooling](guide/03-database-migrations-and-pooling.md) *(Completed)*
4. [Chapter 4: The Documents Store](guide/04-the-documents-store.md) *(Completed)*
5. [Chapter 5: The API Server and Handlers](guide/05-the-api-server-and-handlers.md) *(Completed)*
6. [Chapter 6: Publishing Events to NATS JetStream](guide/06-publishing-events-to-nats-jetstream.md) *(Completed)*
7. [Chapter 7: MinIO Storage and Markdown Engine](guide/07-minio-storage-and-markdown-engine.md) *(Completed)*
8. [Chapter 8: Queue Consumer and Event Feedback Loop](guide/08-queue-consumer-and-event-feedback.md) *(Next)*
9. [Chapter 9: Multi-Stage Dockerfiles](guide/09-multi-stage-dockerfiles.md)
10. [Chapter 10: Kubernetes on Arch Linux with k3d](guide/10-kubernetes-on-arch-with-k3d.md)
11. [Chapter 11: Resilience, Graceful Shutdown, and Production Probes](guide/11-resilience-graceful-shutdown-and-probes.md)

Read the [Comprehensive Reference Manual](guide/README.md) for full service catalogs, port mappings, and schema specifications.
