# AGENTS.md — Master Source of Truth

> **Purpose**: Definitive architectural specification and pedagogical standard for this repository. Any AI agent or human author generating content, code, documentation, or infrastructure manifests for this project must strictly comply with the principles and rules defined here.

---

## 1. Project Overview & Domain

We are building a clean, production-grade **Event-Driven Markdown Publishing Pipeline** in Go on Arch Linux.

### The Problem Domain
Modern documentation and publishing systems allow users to submit raw Markdown files. Compiling Markdown (parsing CommonMark, code syntax highlighting, HTML sanitization, embedding styling, and uploading rendered artifacts to cloud object storage) is CPU- and I/O-intensive.

Decoupling ingestion from compilation using an asynchronous event bus ensures the public API responds in milliseconds while background workers process jobs at a controlled, resilient rate.

---

## 2. The Clean 2-Service Event-Driven Architecture

Zero forced abstractions. No artificial synchronous RPCs. A pure, choreography-based distributed architecture:

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
     Store Metadata    |                 | Publish "doc.submitted"
     (PostgreSQL)      v                 v
            +------------------+   +------------------+
            |    PostgreSQL    |   |     RabbitMQ     |
            | (Documents Store)|   |  Exchange: docs  |
            +------------------+   +--------+---------+
                       ^                    |
                       |                    | Consume "doc.submitted"
        Update Status  |                    v
       (via API worker)|       +------------------------+
       consumes event  |       |    Compiler Service    |
       "doc.compiled"  +-------+      (cmd/compiler)    |
                               +-----------+------------+
                                           |
                               1. Parse Markdown (goldmark)
                               2. Render styled HTML
                               3. Upload to MinIO (Bucket: docs-rendered)
                               4. Publish "doc.compiled" / ACK
                                           |
                                           v
                               +------------------------+
                               |     MinIO (S3 API)     |
                               |  Bucket: docs-rendered |
                               +------------------------+
```

### Service Responsibilities & Strict Boundaries

1. **API Service (`cmd/api`, Port `:8080`)**:
   - **Public Ingestion**: Accepts Markdown submissions via REST (`POST /v1/documents`), validates size/syntax, writes `PENDING` records to PostgreSQL, and returns `202 Accepted` with a UUID in ~15ms.
   - **Public Status Polling**: Handles `GET /v1/documents/{id}` to return document state, compilation metrics, and the direct MinIO download link.
   - **Exclusive Database Owner**: Sole owner of PostgreSQL `documents` table.
   - **Event Publisher & Consumer**:
     - Publishes `doc.submitted` to RabbitMQ upon user submission.
     - Subscribes to `doc.compiled` and `doc.failed` from RabbitMQ to update PostgreSQL status (`COMPLETED` or `FAILED`).

2. **Compiler Service (`cmd/compiler`)**:
   - **Pure Autonomous Background Worker**: No public HTTP port.
   - **Zero Database Access**: Does **not** connect to PostgreSQL (strictly enforcing the Database Boundary Rule).
   - **Queue Consumer**: Subscribes to `docs.compile` queue in RabbitMQ with strict worker concurrency (`prefetch_count`).
   - **Engine**: Parses Markdown with `goldmark`, bundles CSS, and streams the compiled HTML artifact directly into MinIO S3 (`artifacts/{id}.html`).
   - **Event Publisher**: Publishes `doc.compiled` (with S3 key and render duration) or `doc.failed` back to RabbitMQ, then acknowledges (`ACK`) the message.
   - **Resilience**: Rejects corrupt messages to the Dead Letter Queue (`docs.compile.dlq`).

---

## 3. Pedagogical Philosophy: The Alex Edwards Method

Every chapter in `guide/` and all agent responses must strictly adhere to this teaching methodology:

### Principle 1: Standard Library First (Zero-Magic)
- **No frameworks**: No Gin, Fiber, Echo, or GORM.
- **Why**: Frameworks hide how the web really works. When a framework breaks or leaks memory, beginners have no idea why. Learning Go's `net/http`, `context`, `database/sql`, `os/signal`, and `sync` packages builds lifelong, transferable skills.
- Use focused, battle-tested drivers: `pgx/v5` (PostgreSQL), `rabbitmq/amqp091-go` (RabbitMQ), `minio-go/v7` (S3).

### Principle 2: The "Why" Before the "How"
- Never introduce a pattern or command without explaining:
  1. What real-world problem it solves.
  2. The trade-offs involved (memory, CPU, network, operational complexity).
  3. Why we chose this approach over common alternatives.
- Every code block must feature line-by-line comments explaining non-trivial statements.

### Principle 3: Chunked Cognitive Load (Never Overwhelm)
- **30–60 Line Rule**: Never dump an entire 300-line service file at once. Break it into bite-sized functional units:
  - Step A: Define the struct and dependencies.
  - Step B: Write the constructor.
  - Step C: Implement the core handler method.
  - Step D: Wire up the router and verify.
- **Explain Every Terminal Flag**: Explain flags for Docker, curl, and Linux commands (`-d`, `-p`, `-v`, `-e`).
- **Arch Linux Grounding**: Provide exact commands for Arch Linux (`pacman`, `systemctl`, `k3d`, `ip`, `ss`).

### Principle 4: Clean Architecture & Best Practices from Day One
- **No throwaway code**: Write production-quality code from the start.
- **Explicit Dependency Injection**: Inject all dependencies (`*store.DocumentStore`, `*slog.Logger`, `*minio.Client`, `*amqp.Channel`) via fields on an `application` struct. Never use global variables.
- **The Database Boundary Rule**: One database per service. The Compiler Service does **not** connect to PostgreSQL. It receives payload context via AMQP and reports results via events.

### Principle 5: Progressive Infrastructure Evolution
Infrastructure is learned in three distinct phases:
1. **Local Bare-Metal/Docker Compose**: Understand ports, environment variables, bridge networks, container logs, volume mounts, and healthchecks.
2. **Service Containerization**: Write multi-stage Dockerfiles utilizing Go's static compilation into minimal, secure `scratch` or `distroless` containers.
3. **Kubernetes with k3d (Arch Linux)**: Translate Docker Compose mental models directly into Kubernetes primitives: Deployments, Pods, ClusterIP Services, ConfigMaps, Secrets, PVCs, and Ingress.

---

## 4. Master 11-Chapter Streamlined Curriculum

Clean, focused, and free of artificial abstractions:

```
guide/
├── README.md                                         # Master Reference Manual & System Specs
│
├── [Phase 1: Architecture & Foundation]
│   ├── 01-system-design-and-architecture.md          # 2-service topology, boundaries, sync vs async (DONE)
│   ├── 02-local-infrastructure-docker-compose.md     # Postgres, MinIO, RabbitMQ with healthchecks (DONE)
│   ├── 03-database-migrations-and-pooling.md         # golang-migrate, raw SQL migrations, pgxpool (DONE)
│   └── 04-the-documents-store.md                     # Raw SQL CRUD queries, repository pattern (DONE)
│
├── [Phase 2: The API Service (HTTP & Event Ingestion)]
│   ├── 05-the-api-server-and-handlers.md             # net/http ServeMux, middleware, slog, validation, POST & GET
│   └── 06-publishing-events-to-rabbitmq.md           # RabbitMQ connection, channel lifecycle, persistent doc.submitted
│
├── [Phase 3: The Compiler Worker & MinIO Storage]
│   ├── 07-minio-storage-and-markdown-engine.md       # S3 client, goldmark CommonMark parsing, HTML styling
│   └── 08-queue-consumer-and-event-feedback.md       # AMQP consumer loop, QoS prefetch, ACK/NACK, DLQ, doc.compiled feedback
│
└── [Phase 4: Containerization & Cloud-Native Kubernetes]
    ├── 09-multi-stage-dockerfiles.md                 # Scratch multi-stage builds, composing API + Compiler + Infra
    ├── 10-kubernetes-on-arch-with-k3d.md             # k3d cluster on Arch, Deployments, Services, ConfigMaps, Ingress
    └── 11-resilience-graceful-shutdown-and-probes.md # SIGINT/SIGTERM draining, /livez & /readyz health probes
```

### Detailed Chapter Deliverables

1. **Chapter 1: System Design and Microservice Architecture** *(Complete)*
   - *Deliverable*: 2-service topology, sync vs async analysis, monorepo layout, toolchain verification.
2. **Chapter 2: Local Infrastructure with Docker Compose** *(Complete)*
   - *Deliverable*: `deploy/docker/docker-compose.yml` (Postgres, MinIO, RabbitMQ), persistent named volumes, healthchecks, `Makefile`.
3. **Chapter 3: Database Migrations and Connection Pooling** *(Complete)*
   - *Deliverable*: `migrations/000001_create_documents.up.sql`, `internal/database/db.go` (pgxpool configuration).
4. **Chapter 4: The Documents Store** *(Complete)*
   - *Deliverable*: `internal/store/documents.go` (Insert, GetByID, UpdateStatus with context timeouts, domain models).
5. **Chapter 5: The API Server and Handlers**
   - *Deliverable*: `cmd/api/main.go`, `internal/api/routes.go`, `internal/api/handlers.go` (`POST /v1/documents`, `GET /v1/documents/{id}`), logging & panic-recovery middleware.
6. **Chapter 6: Publishing Events to RabbitMQ**
   - *Deliverable*: `internal/broker/publisher.go` (AMQP channel, exchange declaration, persistent `doc.submitted` publishing).
7. **Chapter 7: MinIO Storage and Markdown Engine**
   - *Deliverable*: `internal/storage/s3.go` (bucket ensure, PutObject), `internal/compiler/engine.go` (`goldmark` HTML compilation, sanitization, embedded styling).
8. **Chapter 8: Queue Consumer & Event Feedback Loop**
   - *Deliverable*: `cmd/compiler/main.go`, `internal/broker/consumer.go` (worker pool, QoS prefetch, ACK/NACK, DLQ routing, publishing `doc.compiled`), and API service consumer updating PostgreSQL.
9. **Chapter 9: Multi-Stage Dockerfiles & Full Compose Stack**
   - *Deliverable*: `deploy/docker/Dockerfile.api`, `deploy/docker/Dockerfile.compiler` (scratch builds), full local stack orchestration.
10. **Chapter 10: Kubernetes on Arch Linux with k3d**
    - *Deliverable*: `k3d` cluster setup, `deploy/k8s/` manifests (Deployments, ClusterIP Services, ConfigMaps, Secrets, Ingress).
11. **Chapter 11: Resilience, Graceful Shutdown, and Production Probes**
    - *Deliverable*: Signal trapping (`SIGINT`/`SIGTERM`), connection draining, `/livez` & `/readyz` endpoints.

---

## 5. Strict Instructions for AI Agents

1. **You Are an Instructor, Not a Code Dumper**:
   - Explain the "why" before the "how".
   - Keep code chunks to 30–60 lines.
2. **Follow Alex Edwards' Writing Style**:
   - Clear, technical, conversational, and direct.
   - Every terminal command must include sample output and flag explanations.
3. **Pacing and Review**:
   - Present one chapter at a time.
   - End each chapter with a verification playbook and self-check questions.
   - Wait for student confirmation before advancing.
