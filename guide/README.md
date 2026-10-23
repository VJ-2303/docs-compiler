# Building Microservices in Go: The Markdown Publishing Pipeline

> **Master Guide & System Reference**  
> A pragmatic, production-grade guide to building event-driven microservices in Go on Arch Linux. Written strictly following the teaching methodology of **Alex Edwards** (*Let's Go* and *Let's Go Further*).

---

## 1. System Architecture & Reference

We are building a decoupled **Markdown Document Publishing Pipeline**. Users submit raw Markdown documents via a public REST API. The compilation process (parsing CommonMark, code syntax highlighting, HTML sanitization, embedding styling, and uploading the rendered document artifact to S3) is decoupled from the HTTP request cycle using an asynchronous message broker.

```
                      +-------------------+
                      |    HTTP Client    |
                      |   (curl / Web)    |
                      +---------+---------+
                                |
             REST / HTTP JSON   | POST /v1/documents (Markdown payload)
                                v
               +---------------------------------+
               |          API Service            |
               |       (cmd/api, :8080)          |
               +-------+-----------------+-------+
                       |                 |
     Store Metadata    |                 | Publish "doc.submitted"
     (PostgreSQL)      v                 v
            +------------------+   +------------------+
            |    PostgreSQL    |   |     RabbitMQ     |
            | (Documents Store)|   |  Exchange: docs  |
            +------------------+   +--------+---------+
                                            |
                              Consume Queue | "doc.compile"
                                            v
                               +------------------------+
                               |    Compiler Service    |
                               | (cmd/compiler, :50051) |
                               +-----------+------------+
                                           |
                               1. Parse Markdown & syntax
                               2. Render styled HTML
                               3. Upload to MinIO (S3)
                               4. Update status / ACK
                                           |
                                           v
                               +------------------------+
                               |     MinIO (S3 API)     |
                               |  Bucket: docs-rendered |
                               +------------------------+
                                           ^
                                           |
       gRPC Internal Query                 |
       (Fetch render stats & artifact URL) |
       API Service ------------------------+
```

---

## 2. Service Catalog & Port Mappings

| Service | Binary Path | Primary Role | Inbound Protocols & Ports | Outbound Dependencies |
| :--- | :--- | :--- | :--- | :--- |
| **API Service** | `cmd/api` | Public gateway, validation, document state manager | HTTP/1.1 REST (`:8080`) | PostgreSQL (`:5432`), RabbitMQ (`:5672`), Compiler gRPC (`:50051`) |
| **Compiler Service** | `cmd/compiler` | Background Markdown compiler & artifact generator | gRPC (`:50051`), AMQP consumer | RabbitMQ (`:5672`), MinIO S3 (`:9000`) |
| **PostgreSQL** | Docker | Document metadata & state store | TCP (`:5432`) | Persistent Docker Volume `pgdata` |
| **RabbitMQ** | Docker | Asynchronous task queue & DLQ | AMQP (`:5672`), Management HTTP (`:15672`) | Persistent Docker Volume `rabbitmqdata` |
| **MinIO** | Docker | S3-compatible blob storage for rendered HTML | S3 API (`:9000`), Console Web UI (`:9001`) | Persistent Docker Volume `miniodata` |

---

## 3. Data Storage & Messaging Specifications

### A. PostgreSQL Schema (`docpub`)
The API service owns the `documents` table exclusively:

```sql
CREATE TABLE IF NOT EXISTS documents (
    id UUID PRIMARY KEY,
    title VARCHAR(255) NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'PENDING',
    source_content TEXT NOT NULL,
    s3_key VARCHAR(512),
    render_duration_ms INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**State Transitions:**
- `PENDING`: Initial state upon HTTP submission.
- `COMPILING`: Worker picked up the job from RabbitMQ.
- `COMPLETED`: Worker successfully uploaded HTML artifact to MinIO.
- `FAILED`: Parsing failed or poison-pill message routed to DLQ.

### B. MinIO Object Storage
- **Bucket**: `docs-rendered`
- **Object Key Scheme**: `artifacts/{document_id}.html`
- **Content Type**: `text/html; charset=utf-8`

### C. RabbitMQ AMQP 0-9-1 Messaging
- **Exchange**: `docs.direct` (Type: `direct`, Durable: `true`)
- **Queue**: `docs.compile` (Durable: `true`, bound with routing key `doc.submitted`)
- **Dead Letter Exchange (DLX)**: `docs.dlx` (Type: `direct`)
- **Dead Letter Queue (DLQ)**: `docs.compile.dlq` (Captures failed or rejected unparseable documents)
- **Message Payload**:
  ```json
  {
    "document_id": "7b8e1a2f-9c44-42b2-8c14-25e227092984",
    "title": "Getting Started with Microservices",
    "content": "# Markdown Content...",
    "submitted_at": "2026-10-22T14:00:00Z"
  }
  ```

### D. gRPC Contract (`DocumentCompilerService`)
Defined in `proto/compiler/v1/compiler.proto`:
- `GetCompilerStatus(Empty) -> CompilerStatusResponse` (Worker health & load)
- `GetArtifactURL(ArtifactRequest) -> ArtifactResponse` (Retrieves signed S3 download URL)
- `CompilePreview(PreviewRequest) -> PreviewResponse` (Synchronous on-demand preview)

---

## 4. Environment Variables Matrix

| Variable | Default Value | Used By | Description |
| :--- | :--- | :--- | :--- |
| `API_PORT` | `8080` | `cmd/api` | HTTP listen port |
| `API_DB_DSN` | `postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable` | `cmd/api` | PostgreSQL connection string |
| `RABBITMQ_URL` | `amqp://guest:guest@localhost:5672/` | `cmd/api`, `cmd/compiler` | AMQP 0-9-1 broker address |
| `COMPILER_GRPC_ADDR`| `localhost:50051` | `cmd/api` | Target address for Compiler gRPC |
| `COMPILER_GRPC_PORT`| `50051` | `cmd/compiler` | gRPC listen port |
| `S3_ENDPOINT` | `localhost:9000` | `cmd/compiler` | MinIO API endpoint |
| `S3_ACCESS_KEY` | `minioadmin` | `cmd/compiler` | MinIO root access key |
| `S3_SECRET_KEY` | `minioadmin` | `cmd/compiler` | MinIO root secret key |
| `S3_BUCKET` | `docs-rendered` | `cmd/compiler` | Target bucket for HTML documents |

---

## 5. Pedagogical Rules (The Alex Edwards Standard)

All chapters in this guide strictly adhere to these rules:

1. **Standard Library First (Zero-Magic)**:
   - We use Go's standard library (`net/http`, `context`, `database/sql`, `os/signal`, `sync`) and focused, idiomatic drivers. No web frameworks (Gin, Fiber) and no magic ORMs (GORM).
2. **The "Why" Before the "How"**:
   - Every pattern, terminal command, and data structure is prefaced with the problem it solves and its architectural trade-offs.
3. **No Massive Code Dumps (30–60 Line Rule)**:
   - Code is introduced in small, functional units. Never dump 300 lines of code at once.
4. **Clean Code from Day One**:
   - Explicit dependency injection via an `application` struct, structured logging with `slog`, and clean error wrapping with `%w`.
5. **Database Boundary Rule**:
   - One database per service. The Compiler Service never connects to PostgreSQL.
6. **Command Explanations**:
   - Every CLI command explains all flags (e.g., `-d`, `-p`, `-v`, `-e`).

---

## 6. The 16-Chapter Modular Curriculum

To prevent cognitive overload, the curriculum is divided into **16 focused, bite-sized chapters**. Each chapter accomplishes one distinct task without full-code dumps:

```
guide/
├── README.md                                         # This Master Specification & Reference
│
├── [Phase 1: Architecture & Foundation]
│   ├── 01-system-design-and-architecture.md          # 2-service topology, boundaries, sync vs async
│   ├── 02-local-infrastructure-docker-compose.md     # Postgres, MinIO, RabbitMQ with healthchecks
│   ├── 03-database-migrations-and-pooling.md         # golang-migrate, raw SQL migrations, pgxpool
│   └── 04-the-documents-store.md                     # Raw SQL CRUD queries, repository pattern
│
├── [Phase 2: Contract-First Inter-Service RPC (gRPC)]
│   ├── 05-defining-the-grpc-contract.md              # Protocol Buffers (proto3), schema, protoc generation
│   ├── 06-implementing-the-grpc-server.md            # Compiler gRPC service implementation
│   └── 07-connecting-the-grpc-client.md              # API service client connection, timeouts, retries
│
├── [Phase 3: The API Service (HTTP & Event Ingestion)]
│   ├── 08-setting-up-the-api-server.md               # net/http ServeMux, custom middleware, slog
│   ├── 09-document-submission-handlers.md            # POST /v1/documents, JSON decoding, validation
│   └── 10-publishing-amqp-events.md                  # RabbitMQ channel, exchange binding, message delivery
│
├── [Phase 4: The Compiler Service (Async Worker & S3 Storage)]
│   ├── 11-minio-object-storage-client.md             # S3 bucket initialization, streaming file uploads
│   ├── 12-pure-go-markdown-engine.md                 # Goldmark CommonMark parsing, CSS styling, sanitization
│   └── 13-asynchronous-queue-consumer.md             # AMQP consumer loop, QoS prefetch, ACK/NACK & DLQ
│
└── [Phase 5: Containerization & Cloud-Native Kubernetes]
    ├── 14-multi-stage-dockerfiles.md                 # Scratch/distroless multi-stage builds, compose stack
    ├── 15-kubernetes-on-arch-with-k3d.md             # k3d cluster, Deployments, ClusterIP Services, Ingress
    └── 16-resilience-graceful-shutdown-and-probes.md # SIGINT/SIGTERM, connection draining, /livez & /readyz
```

---

## 7. Granular Chapter Breakdown

### Phase 1: Architecture & Foundation
- **[Chapter 1: System Design and Microservice Architecture](01-system-design-and-architecture.md)**
  - *Focus*: Domain decomposition, 2-service boundaries, sync vs async trade-offs, monorepo directory layout.
- **[Chapter 2: Local Infrastructure with Docker Compose](02-local-infrastructure-docker-compose.md)**
  - *Focus*: Spinning up PostgreSQL, MinIO, and RabbitMQ with Docker Compose, named volumes, port mappings, and native healthchecks.
- **[Chapter 3: Database Migrations and Connection Pooling](03-database-migrations-and-pooling.md)**
  - *Focus*: Writing idempotent SQL migration files (`000001_create_documents.up.sql`), configuring `pgxpool.Pool` with connection limits and idle timeouts.
- **[Chapter 4: The Documents Store](04-the-documents-store.md)**
  - *Focus*: Implementing raw SQL data-access methods (`Insert`, `GetByID`, `UpdateStatus`) using clean Go structs and context timeouts.

### Phase 2: Contract-First Inter-Service RPC (gRPC)
- **[Chapter 5: Defining the gRPC Contract](05-defining-the-grpc-contract.md)**
  - *Focus*: Writing `proto/compiler/v1/compiler.proto`, understanding Protocol Buffers types, generating Go stubs with `protoc`.
- **[Chapter 6: Implementing the gRPC Server](06-implementing-the-grpc-server.md)**
  - *Focus*: Building the `DocumentCompilerService` server struct in `cmd/compiler`, implementing RPC handler methods, and starting the gRPC TCP listener.
- **[Chapter 7: Connecting the gRPC Client](07-connecting-the-grpc-client.md)**
  - *Focus*: Wiring the API service to dial the Compiler Service over gRPC with client-side connection pooling, keepalive, and context deadlines.

### Phase 3: The API Service (HTTP & Event Ingestion)
- **[Chapter 8: Setting Up the API Server](08-setting-up-the-api-server.md)**
  - *Focus*: Initializing `http.NewServeMux` (Go 1.22+ routing patterns), panic recovery middleware, logging middleware with structured `slog`.
- **[Chapter 9: Document Submission Handlers](09-document-submission-handlers.md)**
  - *Focus*: Handlers for `POST /v1/documents` and `GET /v1/documents/{id}`, JSON helpers (enveloping, size limits), request validation.
- **[Chapter 10: Publishing AMQP Events](10-publishing-amqp-events.md)**
  - *Focus*: RabbitMQ AMQP connection lifecycle, exchange declaration, publishing persistent `doc.submitted` events with correlation IDs.

### Phase 4: The Compiler Service (Async Worker & S3 Storage)
- **[Chapter 11: MinIO Object Storage Client](11-minio-object-storage-client.md)**
  - *Focus*: Initializing `minio.Client`, bucket existence verification, uploading rendered HTML blobs with metadata headers.
- **[Chapter 12: Pure-Go Markdown Engine](12-pure-go-markdown-engine.md)**
  - *Focus*: Integrating `goldmark`, Markdown parsing, code block styling with embedded CSS, HTML sanitization.
- **[Chapter 13: Asynchronous Queue Consumer](13-asynchronous-queue-consumer.md)**
  - *Focus*: AMQP consumer loop, worker concurrency with `prefetch_count`, reliable message acknowledgements (`ack`/`nack`), Dead Letter Queue routing.

### Phase 5: Containerization & Cloud-Native Kubernetes
- **[Chapter 14: Multi-Stage Dockerfiles](14-multi-stage-dockerfiles.md)**
  - *Focus*: Writing production multi-stage Dockerfiles (`golang:alpine` builder -> `scratch` runner), `CGO_ENABLED=0`, non-root user security, composing the full stack.
- **[Chapter 15: Kubernetes on Arch Linux with k3d](15-kubernetes-on-arch-with-k3d.md)**
  - *Focus*: Creating local k3d cluster on Arch Linux, writing manifests for Deployments, ClusterIP Services, ConfigMaps, Secrets, and Ingress routing.
- **[Chapter 16: Resilience, Graceful Shutdown, and Production Probes](16-resilience-graceful-shutdown-and-probes.md)**
  - *Focus*: Capturing `SIGINT` / `SIGTERM`, server drain loops, `/livez` and `/readyz` health endpoints, production checklist.

---

## 8. Arch Linux Host Environment Quick Reference

Verify your local toolchain:

```bash
# Check compiler and container runtimes
go version
docker --version
docker compose version
k3d --version
kubectl version --client
protoc --version
```

Start and enable Docker daemon if not active:
```bash
sudo systemctl enable --now docker
```
Ensure your user can run Docker without `sudo`:
```bash
sudo usermod -aG docker $USER
newgrp docker
```
