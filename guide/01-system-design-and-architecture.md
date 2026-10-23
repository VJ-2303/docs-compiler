# Chapter 1: System Design and Microservice Architecture

Welcome. In this guide, we are going to build a production-grade, event-driven microservices system in Go from scratch on Arch Linux.

Rather than building an overwhelming, multi-tiered monolith or a complex image-processing cluster, we will build a practical, digestible **Markdown Document Publishing Pipeline**. This project gives you hands-on experience with every core microservice pattern without unnecessary complexity:

1. **Synchronous REST API** (`net/http`) for accepting user document submissions and checking status.
2. **Synchronous gRPC** (`google.golang.org/grpc`) for high-performance, type-safe inter-service calls.
3. **Asynchronous Message Queue** (`RabbitMQ`) for offloading document compilation to the background.
4. **Relational Data Boundary** (`PostgreSQL`) for metadata, ownership, and state machines.
5. **Object Storage** (`MinIO / S3`) for storing rendered HTML documents.
6. **Container & Kubernetes Infrastructure** (`Docker Compose` to `k3d` / `k3s`).

Throughout this entire guide, we write pure, standard-library-first Go. Zero bloated frameworks (no Gin, no Fiber), zero magic ORMs (no GORM). Everything is explicit, testable, and production-ready.

---

## 1. Objectives & Overview

In this chapter, you will:
- Understand the 2-service architecture and why 2 services is the ideal learning boundary.
- Learn why we decouple synchronous HTTP ingestion from asynchronous document compilation.
- Understand the Database Boundary Rule (why services never share database tables).
- Set up an idiomatic Go monorepo project structure.
- Verify your Arch Linux host environment and installed tools.

---

## 2. Conceptual Deep Dive (The "Why")

### The High-Level Architecture

Here is how data flows through our system:

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
                               |      Bucket: docs      |
                               +------------------------+
                                           ^
                                           |
       gRPC Internal Query                 |
       (Fetch render stats & artifact URL) |
       API Service ------------------------+
```

### Why Decouple with RabbitMQ?
Imagine a naive system where a user uploads a 10,000-line Markdown document containing tables, math formulas, and code blocks, and the HTTP server compiles it synchronously before responding:
- **CPU Spikes**: If 100 users submit large documents simultaneously, the API server’s CPU spikes to 100%, causing incoming requests to queue up and time out.
- **Lost Data**: If the server crashes or restarts during compilation, the client receives a broken connection error and the work is lost.

By using **RabbitMQ**:
1. The **API Service** validates the payload, saves a `PENDING` record in PostgreSQL, publishes a message to RabbitMQ, and immediately responds with HTTP `202 Accepted` and a `document_id` in under **15 milliseconds**.
2. The **Compiler Service** pulls jobs from RabbitMQ at its own controlled pace (using a concurrency limit called `prefetch_count`).
3. If the Compiler crashes, RabbitMQ detects the severed connection and safely redelivers the message to another worker.

### Why gRPC for Inter-Service Communication?
Why not use HTTP REST between the API Service and Compiler Service?
- **Strict Contracts**: gRPC uses Protocol Buffers (`.proto`). Both services share the exact same generated Go structs. You get compile-time type safety; if a field type changes, the code fails to compile instead of failing silently at runtime.
- **Speed & Payload Size**: Protocol Buffers serialize into compact binary data over HTTP/2, significantly faster than encoding and decoding JSON strings.

### The Database Boundary Rule
Notice that **only the API Service connects to PostgreSQL**. The Compiler Service has no database credentials.
- If multiple services read and write to the same database, you don't have microservices — you have a **distributed monolith**. Any change to a table schema requires deploying both services at the exact same instant.
- In our system, the Compiler gets what it needs in the RabbitMQ event (`document_id`, `markdown_content`), uploads the rendered output to MinIO, and communicates status back via events or gRPC.

---

## 3. Step-by-Step Implementation: Project Layout

Let's establish our clean monorepo folder structure. A monorepo keeps our shared Protobuf definitions and Docker configurations in one repository while maintaining strictly isolated Go binaries.

Here is the directory structure we are creating:

```
new-ms/
├── cmd/
│   ├── api/                  # Main entrypoint for the public HTTP API
│   │   └── main.go
│   └── compiler/             # Main entrypoint for the background worker & gRPC server
│       └── main.go
│
├── internal/                 # Internal Go packages (not importable by external projects)
│   ├── broker/               # RabbitMQ publisher and consumer logic
│   ├── compiler/             # Goldmark Markdown-to-HTML rendering engine
│   ├── database/             # PostgreSQL connection pooling (pgxpool)
│   ├── store/                # SQL queries for documents metadata
│   └── storage/              # MinIO S3 client for saving HTML artifacts
│
├── pkg/                      # Public/generated Go packages
│   └── proto/                # Compiled Protobuf and gRPC stubs
│       └── compiler/
│           └── v1/
│
├── proto/                    # Source Protocol Buffer schemas
│   └── compiler/
│       └── v1/
│           └── compiler.proto
│
├── migrations/               # SQL migration files
│   └── 000001_create_documents_table.up.sql
│
├── deploy/
│   ├── docker/               # Dockerfiles and docker-compose.yml
│   │   ├── Dockerfile.api
│   │   ├── Dockerfile.compiler
│   │   └── docker-compose.yml
│   └── k8s/                  # Kubernetes manifests (k3d)
│
├── guide/                    # This tutorial book
│   ├── 01-system-design-and-architecture.md
│   └── ...
│
├── Makefile                  # Build, run, and migration shortcuts
├── go.mod                    # Root Go module definition
└── go.sum
```

---

## 4. Terminal Commands & Infrastructure Execution

Let's initialize our Go module and create the directory skeleton on your Arch Linux machine.

### Step 4.1: Initialize the Go Module
Run this command in the project root:

```bash
go mod init github.com/vj/new-ms
```

**Explanation of command:**
- `go mod init`: Creates a new `go.mod` file, marking this directory as the root of a Go module.
- `github.com/vj/new-ms`: The module import path that will prefix all internal package imports (e.g. `github.com/vj/new-ms/internal/database`).

**Sample terminal output:**
```
go: creating new go.mod: module github.com/vj/new-ms
```

### Step 4.2: Create the Directory Tree
Run the following `mkdir` command to scaffold the directories:

```bash
mkdir -p cmd/api cmd/compiler \
  internal/broker internal/compiler internal/database internal/store internal/storage \
  pkg/proto/compiler/v1 \
  proto/compiler/v1 \
  migrations \
  deploy/docker deploy/k8s
```

**Explanation of flags:**
- `mkdir -p`: Creates parent directories as needed without throwing an error if the directory already exists.

---

## 5. Verification Playbook

Let's verify that your Go toolchain and directory layout are in place.

### Check 1: Verify `go.mod`
Run:
```bash
cat go.mod
```
**Expected output:**
```
module github.com/vj/new-ms

go 1.27
```

### Check 2: Verify Installed Host Tools
Run this one-liner to confirm your Arch Linux environment has the required container and compilation tools:

```bash
docker --version && k3d --version && kubectl version --client && protoc --version
```

**Expected output:**
- Docker version `29.x`
- k3d version `v5.8.x`
- Client Version `v1.37.x`
- libprotoc `36.x`

---

## 6. Common Gotchas & Troubleshooting

1. **Permission Denied on Docker:**
   - *Symptom*: `docker: Got permission denied while trying to connect to the Docker daemon socket`.
   - *Cause*: On Arch Linux, your user is not in the `docker` group or the daemon is not running.
   - *Fix*:
     ```bash
     sudo systemctl enable --now docker
     sudo usermod -aG docker $USER
     newgrp docker
     ```

2. **Go Module Path Mismatch:**
   - *Symptom*: `package github.com/vj/new-ms/internal/... is not in GOROOT`.
   - *Cause*: Working outside the initialized module directory or running commands from a subfolder.
   - *Fix*: Always execute `go build` and `go run` commands from the repository root `/home/vj/Code/new-ms`.

---

## 7. Summary & Repository Tree Diff

### Key Takeaways
- We chose a clean **2-service architecture**: API Service (`cmd/api`) and Compiler Service (`cmd/compiler`).
- **Sync for user interaction** (HTTP REST), **Async for heavy work** (RabbitMQ), **Contract-first RPC for inter-service queries** (gRPC).
- **Postgres belongs exclusively to the API Service**; the Compiler Service outputs rendered HTML documents into **MinIO S3**.

### Self-Check Questions Before Advancing
1. Why does the Compiler Service **not** need a direct connection to PostgreSQL?
2. What are the two main advantages of using gRPC over HTTP/JSON for inter-service communication?
3. What is the role of `prefetch_count` in RabbitMQ when processing heavy tasks?

---

In **[Chapter 2: Local Infrastructure with Docker Compose](02-local-infrastructure-docker-compose.md)**, we will write our `deploy/docker/docker-compose.yml` to spin up PostgreSQL, MinIO, and RabbitMQ with persistent storage and healthchecks, and verify them using the Arch Linux terminal.
