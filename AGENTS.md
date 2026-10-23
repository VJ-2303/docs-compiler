# AGENTS.md — Master Source of Truth

> **Purpose**: Definitive architectural specification and pedagogical standard for this repository. Any AI agent or human author generating content, code, documentation, or infrastructure manifests for this project must strictly comply with the principles and rules defined here.

---

## 1. Project Overview & Domain

We are building a clean, production-grade **Markdown Document Publishing Pipeline** in Go on Arch Linux.

### The Problem Domain
Modern documentation platforms, note-taking apps, and publishing systems (like GitHub, Notion, or Ghost) allow users to submit raw Markdown files. Compiling Markdown with syntax highlighting, sanitizing HTML, bundling CSS styles, and generating downloadable document artifacts is computationally decoupled from client requests.

### The Solution Architecture
A focused **2-service** distributed system:

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
                               1. Parse Markdown & highlight
                               2. Render styled HTML document
                               3. Upload to MinIO (Bucket: docs)
                               4. Update status / Notify
                                           |
                                           v
                               +------------------------+
                               |     MinIO (S3 API)     |
                               |      Bucket: docs      |
                               +------------------------+
                                           ^
                                           |
       gRPC Internal Query                 |
       (Fetch artifact URL & render stats) |
       API Service ------------------------+
```

1. **API Service (`cmd/api`, Port `:8080`)**:
   - **Public REST API**: Accepts Markdown submissions, validates size/syntax, writes document metadata to PostgreSQL with status `PENDING`, and returns `202 Accepted` with a Document ID.
   - **gRPC Client**: Communicates directly with the Compiler Service over gRPC (`:50051`) to fetch compilation statistics or request on-demand previews.
   - **Event Publisher**: Publishes `doc.submitted` events to RabbitMQ.
2. **Compiler Service (`cmd/compiler`, Port `:50051`)**:
   - **Queue Consumer**: Subscribes to `doc.compile` queue in RabbitMQ.
   - **Compiler Engine**: Parses Markdown, sanitizes output, renders standalone HTML with embedded styles using pure Go (`yuin/goldmark`), and uploads the compiled artifact to MinIO S3 bucket `docs`.
   - **gRPC Server (`:50051`)**: Exposes an internal RPC contract (`DocumentCompilerService`) for checking compiler health, document render stats, and generating signed artifact URLs.

---

## 2. Pedagogical Philosophy: The Alex Edwards Method

Every chapter in `guide/` and all agent responses must strictly adhere to this teaching methodology:

### Principle 1: Standard Library First (Zero-Magic)
- **No frameworks**: No Gin, Fiber, Echo, or GORM.
- **Why**: Frameworks hide how the web really works. When a framework breaks or leaks memory, beginners have no idea why. Learning Go's `net/http`, `context`, `database/sql`, `os/signal`, and `sync` packages builds lifelong, transferable skills.
- Use focused, battle-tested drivers: `pgx/v5` (PostgreSQL), `rabbitmq/amqp091-go` (RabbitMQ), `minio-go/v7` (S3), and `google.golang.org/grpc`.

### Principle 2: The "Why" Before the "How"
- Never introduce a pattern or command without explaining:
  1. What real-world problem it solves.
  2. The trade-offs involved (memory, CPU, network, operational complexity).
  3. Why we chose this approach over common alternatives.
- Every code block must feature line-by-line comments explaining non-trivial statements.

### Principle 3: Chunked Cognitive Load (Never Overwhelm)
- **2 Services Only**: Keep the architecture approachable for beginners.
- **30–60 Line Rule**: Never dump an entire 300-line service file at once. Break it into bite-sized functional units:
  - Step A: Define the struct and dependencies.
  - Step B: Write the constructor.
  - Step C: Implement the core handler method.
  - Step D: Wire up the router and verify.
- **Explain Every Terminal Flag**: Explain flags for Docker, curl, and Linux commands (`-d`, `-p`, `-v`, `-e`).
- **Arch Linux Grounding**: Provide exact commands for Arch Linux (`pacman`, `systemctl`, `k3d`, `ip`, `ss`).

### Principle 4: Clean Architecture & Best Practices from Day One
- **No throwaway code**: Write production-quality code from the start.
- **Explicit Dependency Injection**: Inject all dependencies (`*pgxpool.Pool`, `*slog.Logger`, `*minio.Client`, `*amqp.Channel`) via fields on an `application` struct. Never use global variables.
- **The Database Boundary Rule**: One database per service. The Compiler Service does **not** connect to PostgreSQL. It receives payload context via AMQP and reports results via events or gRPC.

### Principle 5: Progressive Infrastructure Evolution
Infrastructure is learned in three distinct phases:
1. **Local Bare-Metal/Docker Compose**: Understand ports, environment variables, bridge networks, container logs, volume mounts, and healthchecks.
2. **Service Containerization**: Write multi-stage Dockerfiles utilizing Go's static compilation into minimal, secure `scratch` or `distroless` containers.
3. **Kubernetes with k3d (Arch Linux)**: Translate Docker Compose mental models directly into Kubernetes primitives: Deployments, Pods, ClusterIP Services, ConfigMaps, Secrets, PVCs, and Ingress.

---

## 3. Mandatory Anatomy of Every Guide Chapter

Every chapter markdown file in `guide/` **must** follow this 7-part structure:

```
# Chapter X: [Title]

## 1. Objectives & Overview
- What we will build in this chapter.
- Real-world motivation and architectural role.

## 2. Conceptual Deep Dive (The "Why")
- Architectural diagram / flow diagram.
- How this primitive works under the hood (e.g. AMQP exchange vs queue, S3 multipart vs single PUT).
- Alternative approaches considered and why they were rejected.

## 3. Step-by-Step Implementation
- Code in small, focused snippets (30–60 lines max).
- Deep line-by-line commentary on new concepts.
- Explicit imports and package names.

## 4. Terminal Commands & Infrastructure Execution
- Complete commands to run on Arch Linux.
- Explanation of all flags and parameters.
- Sample terminal output so the student knows what success looks like.

## 5. Verification Playbook
- Concrete, runnable verification tests (curl commands, DB queries, UI inspection).
- Expected HTTP status codes, headers, and responses.

## 6. Common Gotchas & Troubleshooting
- Practical troubleshooting for real errors (connection refused, permission denied, port in use, AMQP channel exceptions).
- Step-by-step resolution commands.

## 7. Summary & Repository Tree Diff
- Key takeaways.
- Current file tree showing newly created and modified files.
- Self-check questions before advancing.
```

---

## 4. Technology Stack & Host Environment

Configured specifically for Arch Linux (x86_64):

| Category | Technology | Version | Purpose |
| :--- | :--- | :--- | :--- |
| **Host OS** | Arch Linux (x86_64) | Rolling | Development host |
| **Language** | Go | `1.22+` (installed: `1.27.1`) | Microservices implementation |
| **Database** | PostgreSQL | `16-alpine` | Document metadata and state store |
| **DB Driver / Migrations** | `pgx/v5` + `golang-migrate` | CLI / SQL files | Raw SQL queries & versioned schema migrations |
| **Object Storage** | MinIO | Latest RELEASE container | S3-compatible blob storage for rendered HTML documents |
| **Message Broker** | RabbitMQ | `3.13-management-alpine` | AMQP 0-9-1 task queues, fanout exchanges, DLQ |
| **RPC Framework** | gRPC + Protocol Buffers | `protoc 36.1`, `protoc-gen-go`, `protoc-gen-go-grpc` | Contract-first internal synchronous RPC |
| **Markdown Compiler** | Pure Go `goldmark` | `v1.7+` | Fast, CommonMark compliant, zero CGo dependencies |
| **Local Containers** | Docker & Docker Compose | Docker `29.x` | Local infrastructure orchestration |
| **Local Kubernetes** | `k3d` (k3s in Docker) + `kubectl` | `k3d v5.8.3`, `k3s v1.31.5`, `kubectl v1.37` | Cloud-native production deployment |

---

## 5. Master 16-Chapter Curriculum

To prevent cognitive overload and avoid massive code dumps, the curriculum is divided into **16 bite-sized chapters**. Each chapter introduces one focused concept and never exceeds 30–60 lines of code per step.

```
guide/
├── README.md                                         # Master Reference Manual & Architecture
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

### Granular Chapter Deliverables

1. **Chapter 1: System Design and Microservice Architecture**
   - *Status*: Complete in `guide/01-system-design-and-architecture.md`.
   - *Deliverable*: 2-service topology, sync vs async analysis, monorepo layout, toolchain verification.
2. **Chapter 2: Local Infrastructure with Docker Compose**
   - *Deliverable*: `deploy/docker/docker-compose.yml` (Postgres, MinIO, RabbitMQ), persistent named volumes, healthchecks, `Makefile`.
3. **Chapter 3: Database Migrations and Connection Pooling**
   - *Deliverable*: `migrations/000001_create_documents.up.sql`, `internal/database/db.go` (pgxpool configuration).
4. **Chapter 4: The Documents Store**
   - *Deliverable*: `internal/store/documents.go` (Insert, GetByID, UpdateStatus with context timeouts).
5. **Chapter 5: Defining the gRPC Contract**
   - *Deliverable*: `proto/compiler/v1/compiler.proto`, compilation script, `pkg/proto/compiler/v1/` stubs.
6. **Chapter 6: Implementing the gRPC Server**
   - *Deliverable*: `internal/compiler/grpc_server.go` (implementing `DocumentCompilerServiceServer`).
7. **Chapter 7: Connecting the gRPC Client**
   - *Deliverable*: `internal/client/compiler.go` (gRPC dialing, client pool, context deadlines).
8. **Chapter 8: Setting Up the API Server**
   - *Deliverable*: `cmd/api/main.go`, `internal/api/routes.go`, logging & panic-recovery middleware.
9. **Chapter 9: Document Submission Handlers**
   - *Deliverable*: `internal/api/handlers.go` (`POST /v1/documents`, `GET /v1/documents/{id}`), JSON helpers.
10. **Chapter 10: Publishing AMQP Events**
    - *Deliverable*: `internal/broker/publisher.go` (AMQP channel, persistent delivery, correlation IDs).
11. **Chapter 11: MinIO Object Storage Client**
    - *Deliverable*: `internal/storage/s3.go` (bucket ensure, PutObject with HTML content-type).
12. **Chapter 12: Pure-Go Markdown Engine**
    - *Deliverable*: `internal/compiler/engine.go` (Goldmark parsing, CSS bundling, sanitization).
13. **Chapter 13: Asynchronous Queue Consumer**
    - *Deliverable*: `internal/broker/consumer.go` (QoS prefetch, ACK/NACK, Dead Letter Exchange).
14. **Chapter 14: Multi-Stage Dockerfiles**
    - *Deliverable*: `deploy/docker/Dockerfile.api`, `deploy/docker/Dockerfile.compiler` (scratch builds).
15. **Chapter 15: Kubernetes on Arch Linux with k3d**
    - *Deliverable*: `k3d` cluster setup, `deploy/k8s/` manifests (Deployments, Services, Ingress).
16. **Chapter 16: Resilience, Graceful Shutdown, and Production Probes**
    - *Deliverable*: Signal trapping (`SIGINT`/`SIGTERM`), connection draining, `/livez` & `/readyz` endpoints.

---

## 6. Strict Instructions for AI Agents

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
