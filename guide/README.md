# Building Microservices in Go: The Markdown Publishing Pipeline

> **Master Guide & System Reference**  
> A pragmatic, production-grade guide to building an event-driven microservices system in Go on Arch Linux. Written strictly following the teaching methodology of **Alex Edwards** (*Let's Go* and *Let's Go Further*).

---

## 1. System Architecture & Reference

We are building a decoupled **Event-Driven Markdown Publishing Pipeline**.

Users submit raw Markdown documents via a public REST API. The compilation process (parsing CommonMark, code syntax highlighting, HTML sanitization, embedding CSS styling, and uploading the rendered document artifact to S3) is decoupled from the HTTP request cycle using an asynchronous message broker: **NATS JetStream**.

```
                      +-------------------+
                      |    HTTP Client    |
                      |   (curl / Web)    |
                      +---------+---------+
                                |
             REST / HTTP JSON   | POST /v1/documents
                                v
               +---------------------------------+
               |          API Service            |
               |        (cmd/api, :8080)         |
               +-------+-----------------+-------+
                       |                 |
     Store Metadata    |                 | Publish "docs.submitted"
     (PostgreSQL)      v                 v
            +------------------+   +----------------------+
            |    PostgreSQL    |   |    NATS JetStream    |
            | (Documents Store)|   |     Stream: DOCS     |
            +------------------+   +----------+-----------+
                       ^                      |
                       |                      | Consume "docs.submitted"
        Update Status  |                      v
       (via API worker)|         +------------------------+
       consumes event  |         |    Compiler Service    |
       "docs.compiled" +---------+      (cmd/compiler)    |
                                 +-----------+------------+
                                             |
                                 1. Parse Markdown (goldmark)
                                 2. Render styled HTML
                                 3. Upload to MinIO (Bucket: docs-rendered)
                                 4. Publish "docs.compiled" / ACK
                                             |
                                             v
                                 +------------------------+
                                 |     MinIO (S3 API)     |
                                 |  Bucket: docs-rendered |
                                 +------------------------+
```

---

## 2. The Complete End-to-End Flow (In Plain English)

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

### Phase 1: Fast Ingestion (Synchronous HTTP ~ 15ms)
1. **User Submits Markdown**:
   - Sends HTTP `POST /v1/documents` with `{ "title": "My Post", "content": "# ..." }`.
2. **API Service Validates & Saves**:
   - Validates JSON format and content length.
   - Saves a new row to PostgreSQL with status `PENDING` and gets back a generated UUID (`document_id`).
3. **API Service Dispatches Event**:
   - Publishes a persistent message to NATS JetStream subject `docs.submitted` on stream `DOCS`.
   - Payload: `{ "document_id": "...", "title": "...", "source_content": "# ..." }`.
4. **Immediate Response to User**:
   - Responds with HTTP `202 Accepted` and the `document_id`.
   - **Total latency: ~15ms**. The user does not wait for compilation.

### Phase 2: Asynchronous Background Compilation (Worker Loop)
5. **Compiler Service Pulls the Job**:
   - The Compiler Service continuously listens to subject `docs.submitted` using a durable JetStream consumer.
   - When the message arrives, an idle worker goroutine picks it up.
6. **Compiles Markdown to Styled HTML**:
   - The worker runs `goldmark` to convert CommonMark text to sanitized HTML.
   - It wraps the HTML in a standalone document template with responsive CSS styling and syntax highlighting.
7. **Uploads Artifact to MinIO (S3)**:
   - The worker streams the rendered HTML into MinIO bucket `docs-rendered` under the key `artifacts/{document_id}.html`.
8. **Publishes Completion Event & ACKs**:
   - The worker publishes a `docs.compiled` event to NATS JetStream containing the `document_id`, `s3_key`, and `render_duration_ms`.
   - The worker sends an `ACK` to NATS JetStream, safely acknowledging the job.

### Phase 3: State Reconciliation & Retrieval
9. **API Service Updates Database**:
   - The API Service background listener consumes `docs.compiled` from NATS JetStream.
   - It updates PostgreSQL: sets status `COMPLETED`, saves `s3_key`, and records duration.
10. **User Polls Status**:
    - The user calls `GET /v1/documents/{document_id}`.
    - The API service queries PostgreSQL directly and returns:
      ```json
      {
        "id": "7b8e1a2f-9c44-42b2-8c14-25e227092984",
        "title": "Getting Started with Microservices",
        "status": "COMPLETED",
        "download_url": "http://localhost:9000/docs-rendered/artifacts/7b8e1a2f-9c44-42b2-8c14-25e227092984.html",
        "render_duration_ms": 42
      }
      ```
    - The user opens the link and views their rendered, styled document.

---

## 3. Service Catalog & Port Mappings

| Service | Binary Path | Primary Role | Inbound Protocols & Ports | Outbound Dependencies |
| :--- | :--- | :--- | :--- | :--- |
| **API Service** | `cmd/api` | Public gateway, validation, document state manager | HTTP/1.1 REST (`:8080`) | PostgreSQL (`:5432`), NATS JetStream (`:4222`) |
| **Compiler Service** | `cmd/compiler` | Background Markdown compiler & artifact generator | NATS consumer | NATS JetStream (`:4222`), MinIO S3 (`:9000`) |
| **PostgreSQL** | Docker | Document metadata & state store | TCP (`:5432`) | Persistent Docker Volume `docpub-pgdata` |
| **NATS JetStream** | Docker | High-performance Go message stream & task queue | Client TCP (`:4222`), HTTP Monitor (`:8222`) | Persistent Docker Volume `docpub-natsdata` |
| **MinIO** | Docker | S3-compatible blob storage for rendered HTML | S3 API (`:9000`), Console Web UI (`:9001`) | Persistent Docker Volume `docpub-miniodata` |

---

## 4. Data Storage & Messaging Specifications

### A. PostgreSQL Schema (`docpub`)
The API service owns the `documents` table exclusively:

```sql
CREATE TABLE IF NOT EXISTS documents (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    title VARCHAR(255) NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'PENDING',
    source_content TEXT NOT NULL,
    s3_key VARCHAR(512),
    render_duration_ms INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_document_status CHECK (status IN ('PENDING', 'COMPILING', 'COMPLETED', 'FAILED'))
);
```

### B. MinIO Object Storage
- **Bucket**: `docs-rendered`
- **Object Key Scheme**: `artifacts/{document_id}.html`
- **Content Type**: `text/html; charset=utf-8`

### C. NATS JetStream Messaging Specifications
- **Stream Name**: `DOCS`
- **Storage Type**: `File` (persisted to disk on volume)
- **Subjects**:
  - `docs.submitted`: Ingestion events published by API Service; consumed by Compiler Service.
  - `docs.compiled`: Successful compilation events published by Compiler Service; consumed by API Service.
  - `docs.failed`: Fatal compilation failures; consumed by API Service to flag `FAILED` status.
- **Consumer Configuration**:
  - **Durable Name**: `compiler-worker`
  - **Deliver Policy**: `All` (or `New`)
  - **Ack Policy**: `Explicit` (worker must call `msg.Ack()` after successful upload)
  - **Max Deliver**: `3` (retries before routing to dead-letter subject `docs.dead`)

---

## 5. Environment Variables Matrix

| Variable | Default Value | Used By | Description |
| :--- | :--- | :--- | :--- |
| `API_PORT` | `8080` | `cmd/api` | HTTP listen port |
| `API_DB_DSN` | `postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable` | `cmd/api` | PostgreSQL connection string |
| `NATS_URL` | `nats://localhost:4222` | `cmd/api`, `cmd/compiler` | NATS JetStream broker address |
| `S3_ENDPOINT` | `localhost:9000` | `cmd/compiler` | MinIO API endpoint |
| `S3_ACCESS_KEY` | `minioadmin` | `cmd/compiler` | MinIO root access key |
| `S3_SECRET_KEY` | `minioadmin` | `cmd/compiler` | MinIO root secret key |
| `S3_BUCKET` | `docs-rendered` | `cmd/compiler` | Target bucket for HTML documents |

---

## 6. Pedagogical Rules (The Alex Edwards Standard)

1. **Standard Library First (Zero-Magic)**:
   - We use Go's standard library (`net/http`, `context`, `database/sql`, `os/signal`, `sync`) and focused, idiomatic drivers. No web frameworks (Gin, Fiber) and no magic ORMs (GORM).
2. **The "Why" Before the "How"**:
   - Every pattern, terminal command, and data structure is prefaced with the problem it solves and its architectural trade-offs.
3. **No Massive Code Dumps (30–60 Line Rule)**:
   - Code is introduced in small, functional units.
4. **Clean Code from Day One**:
   - Explicit dependency injection via an `application` struct, structured logging with `slog`, and clean error wrapping with `%w`.
5. **Database Boundary Rule**:
   - One database per service. The Compiler Service never connects to PostgreSQL.

---

## 7. Master 11-Chapter Modular Curriculum

```
guide/
├── README.md                                         # Master Reference Manual & System Specs
│
├── [Phase 1: Architecture & Foundation]
│   ├── 01-system-design-and-architecture.md          # 2-service topology, boundaries, sync vs async (DONE)
│   ├── 02-local-infrastructure-docker-compose.md     # Postgres, MinIO, NATS JetStream with healthchecks (DONE)
│   ├── 03-database-migrations-and-pooling.md         # golang-migrate, raw SQL migrations, pgxpool (DONE)
│   └── 04-the-documents-store.md                     # Raw SQL CRUD queries, repository pattern (DONE)
│
├── [Phase 2: The API Service (HTTP & Event Ingestion)]
│   ├── 05-the-api-server-and-handlers.md             # net/http ServeMux, middleware, slog, validation, POST & GET (DONE)
│   └── 06-publishing-events-to-nats-jetstream.md     # NATS connection, JetStream context, stream declaration, persistent docs.submitted (DONE)
│
├── [Phase 3: The Compiler Worker & MinIO Storage]
│   ├── 07-minio-storage-and-markdown-engine.md       # S3 client, goldmark CommonMark parsing, HTML styling (DONE)
│   └── 08-queue-consumer-and-event-feedback.md       # JetStream consumer loop, pull consumers, ACK/NAK, docs.compiled feedback
│
└── [Phase 4: Containerization & Cloud-Native Kubernetes]
    ├── 09-multi-stage-dockerfiles.md                 # Scratch multi-stage builds, composing API + Compiler + Infra
    ├── 10-kubernetes-on-arch-with-k3d.md             # k3d cluster on Arch, Deployments, Services, ConfigMaps, Ingress
    └── 11-resilience-graceful-shutdown-and-probes.md # SIGINT/SIGTERM draining, /livez & /readyz health probes
```

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
