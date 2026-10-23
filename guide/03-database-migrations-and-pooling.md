# Chapter 3: Database Migrations and Connection Pooling

Welcome to Chapter 3. Now that our infrastructure containers are running in Docker, we turn our attention to the database foundation of our API Service.

In production applications, altering a database schema by manually opening a terminal and running `ALTER TABLE` or using an ORM that auto-synchronizes tables on startup is dangerous. Auto-syncing ORMs can inadvertently lock tables in production, erase columns, or mask performance regressions.

Instead, we follow the industry-standard approach championed by Alex Edwards: **explicit, versioned SQL migrations** tracked in Git, paired with an optimized **PostgreSQL connection pool** using `pgx/v5`.

---

## 1. Objectives & Overview

In this chapter, you will:
- Understand why schema migrations must be versioned, immutable, and reversible.
- Install and configure `golang-migrate` on Arch Linux.
- Write our first idempotent `up` and `down` SQL migration files.
- Learn how connection pools prevent database exhaustion and latency spikes under load.
- Implement an explicit, production-grade connection pool constructor in Go using `pgxpool.Pool`.
- Verify migration execution and schema integrity directly in PostgreSQL.

---

## 2. Conceptual Deep Dive (The "Why")

### Why Versioned SQL Migrations?
As a system evolves across staging, testing, and production environments, the database schema must evolve in lockstep with the application code.

A migration consists of two paired files:
1. **Up migration (`.up.sql`)**: Applies changes forward (e.g., creating a table, adding a column or index).
2. **Down migration (`.down.sql`)**: Reverses the exact changes made by the corresponding Up migration (e.g., dropping the table or column).

`golang-migrate` tracks which migrations have been applied by maintaining a dedicated metadata table in your database named `schema_migrations`. This table stores a single integer: the current version number, and a boolean indicating whether the last migration succeeded or failed.

```
migrations/
├── 000001_create_documents.up.sql    ──> [Run Up] ──> Applies schema to DB
└── 000001_create_documents.down.sql  ──> [Run Down] ──> Reverts schema cleanly
```

### Why Raw SQL Over an ORM?
Frameworks like GORM try to deduce table structure by inspecting Go structs via reflection (`db.AutoMigrate(&Document{})`). This has several critical drawbacks:
- It hides index creation, constraint naming, and cascade rules.
- It cannot reliably perform column renames or table splits without data loss.
- It prevents DBAs and software engineers from reviewing the exact SQL being executed against production databases.

Raw SQL migrations give you 100% control over data types, default values, foreign keys, and indexes.

---

### How Database Connection Pooling Works
Establishing a new TCP connection to PostgreSQL is computationally expensive:
1. TCP 3-way handshake (`SYN`, `SYN-ACK`, `ACK`).
2. TLS negotiation (in production).
3. PostgreSQL backend process fork and authentication check.
4. Setting up server-side backend memory (`work_mem`, transaction buffers).

If an HTTP server opens a new database connection for every incoming HTTP request, it will rapidly exhaust database memory and collapse under modest traffic.

A **connection pool** solves this by creating a pool of open, authenticated database connections when the application boots. When a request arrives:
1. The handler borrows an active connection from the pool.
2. The handler executes its query.
3. The connection is returned to the pool for reuse.

```
Incoming Requests
    │       │       │
    ▼       ▼       ▼
+-------------------------------+
|       HTTP Handlers           |
+-------------------------------+
    │       │       │  (Borrow connection)
    ▼       ▼       ▼
+-------------------------------+
|       pgxpool.Pool            |  <── Manages active & idle connections
+-------------------------------+
    │       │       │
    ▼       ▼       ▼  (Pre-warmed TCP sockets)
+-------------------------------+
|       PostgreSQL Engine       |
+-------------------------------+
```

### Essential Pool Tuning Parameters

To prevent resource exhaustion, `pgxpool` provides knobs that we must configure explicitly:

- **`MaxConns` (Maximum Open Connections)**: The upper ceiling of simultaneous connections. PostgreSQL defaults to a global limit of 100 connections. If your Go service opens 150, Postgres rejects the excess. Setting `MaxConns: 25` ensures our service leaves ample connections for administrative access and other replicas.
- **`MinConns` (Minimum Idle Connections)**: Connections kept alive even during zero traffic. This eliminates latency spikes for sudden requests because connections are already established.
- **`MaxConnLifetime`**: The maximum lifespan of any single connection (e.g., 1 hour). Periodically retiring connections ensures long-lived connection memory leaks in drivers or proxies are safely reclaimed.
- **`MaxConnIdleTime`**: If a connection remains unused for longer than this duration (e.g., 15 minutes), it is closed and removed from the pool until needed again.

---

## 3. Step-by-Step Implementation

### Step 3.1: Writing the Up Migration
Create the file `migrations/000001_create_documents.up.sql`.

```sql
-- migrations/000001_create_documents.up.sql

-- Enable UUID extension if not already available
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Create documents table
CREATE TABLE IF NOT EXISTS documents (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    title VARCHAR(255) NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'PENDING',
    source_content TEXT NOT NULL,
    s3_key VARCHAR(512),
    render_duration_ms INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    -- Constraint ensuring status values adhere to our state machine
    CONSTRAINT chk_document_status CHECK (status IN ('PENDING', 'COMPILING', 'COMPLETED', 'FAILED'))
);

-- Index for querying documents by status (used for monitoring & queue reconciliation)
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(status);

-- Index for ordering documents by creation time
CREATE INDEX IF NOT EXISTS idx_documents_created_at ON documents(created_at DESC);
```

**Line-by-line explanation:**
- `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`: Enables PostgreSQL's native UUID generation functions.
- `id UUID PRIMARY KEY`: We use UUIDs rather than sequential auto-incrementing integers (`SERIAL` / `BIGINT`). Sequential IDs leak business volume to clients (e.g., user can guess total orders from ID `42`) and create conflicts in distributed systems.
- `TIMESTAMPTZ`: Always use timestamp with time zone. Storing bare `TIMESTAMP` without timezone leads to subtle bugs when servers, databases, and clients reside in different geographic regions.
- `chk_document_status`: Enforces our state machine at the database storage engine layer. No buggy code can insert an arbitrary status string.

---

### Step 3.2: Writing the Down Migration
Create the file `migrations/000001_create_documents.down.sql`.

```sql
-- migrations/000001_create_documents.down.sql

DROP INDEX IF EXISTS idx_documents_created_at;
DROP INDEX IF EXISTS idx_documents_status;
DROP TABLE IF EXISTS documents;
```

**Explanation:**
- Drops the created indexes and the `documents` table in reverse order of creation.
- If we ever need to roll back this migration, running `migrate down` leaves the database in the exact clean state it was in beforehand.

---

### Step 3.3: Writing the Connection Pool Constructor
Now we write our database connection logic in Go.

Create `internal/database/db.go`. We will build this in two small chunks adhering to our 30–60 line rule.

#### Part A: Struct and Constructor Definition

```go
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the connection pool tuning parameters.
type Config struct {
	DSN             string        // PostgreSQL connection string
	MaxConns        int32         // Maximum open connections
	MinConns        int32         // Minimum idle connections kept warm
	MaxConnLifetime time.Duration // Maximum lifetime of a connection
	MaxConnIdleTime time.Duration // Maximum idle time before closing a connection
}

// New creates and verifies a new pgxpool.Pool.
func New(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	// Parse the DSN string into a pgxpool configuration struct
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parsing db dsn: %w", err)
	}

	// Apply our pool limits
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime

	// Initialize the connection pool with a timeout context
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("initializing pgxpool: %w", err)
	}

	// Ping database to confirm connection credentials and TCP reachability
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return pool, nil
}
```

**Line-by-line explanation:**
- `pgxpool.ParseConfig(cfg.DSN)`: Validates the PostgreSQL connection string format (e.g., `postgres://user:pass@host:port/dbname?sslmode=disable`).
- `context.WithTimeout(ctx, 5*time.Second)`: Prevents the application from hanging indefinitely if PostgreSQL is unreachable or network packets are silently dropped.
- `pool.Ping(connectCtx)`: Crucial! `pgxpool.NewWithConfig` creates the pool data structure lazily. The `Ping` call forces an immediate round-trip TCP handshake and authentication check before the function returns. If credentials or host are incorrect, the service fails fast at boot time.

---

## 4. Terminal Commands & Infrastructure Execution

### Step 4.1: Install `golang-migrate` on Arch Linux
On Arch Linux, install `golang-migrate` directly using `pacman`:

```bash
sudo pacman -S --needed golang-migrate
```

Verify installation:
```bash
migrate -version
```
**Expected output:**
```
v4.18.x (or similar modern release)
```

*(Alternative via Go toolchain if you prefer not to use pacman:)*
```bash
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
```

---

### Step 4.2: Update `go.mod` with `pgx/v5`
Download the `pgx/v5` driver package:

```bash
go get github.com/jackc/pgx/v5
```

Run `go mod tidy` to clean up indirect dependencies:
```bash
go mod tidy
```

---

### Step 4.3: Add Migration Recipes to `Makefile`
Open your root `Makefile` and append these targets:

```makefile
DB_DSN ?= "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable"

migrate-up:
	migrate -path migrations -database $(DB_DSN) up

migrate-down:
	migrate -path migrations -database $(DB_DSN) down 1

migrate-force:
	@read -p "Enter version to force: " version; \
	migrate -path migrations -database $(DB_DSN) force $$version
```

**Flag explanation:**
- `-path migrations`: Directory where our `.up.sql` and `.down.sql` files are located.
- `-database $(DB_DSN)`: Target database connection URL.
- `up`: Applies all pending up migrations.
- `down 1`: Rolls back exactly 1 migration step.

---

### Step 4.4: Execute the Migration
Make sure your Docker containers are running (`make up`), then execute:

```bash
make migrate-up
```

**Sample terminal output:**
```
migrate -path migrations -database "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable" up
1/u create_documents (14.2834ms)
```

---

## 5. Verification Playbook

### Check 1: Verify Tables Created in PostgreSQL
Open an interactive `psql` shell inside your Docker container:

```bash
docker exec -it docpub-postgres psql -U docpub -d docpub
```

List all tables:
```sql
\dt
```

**Expected output:**
```
             List of relations
 Schema |       Name        | Type  | Owner  
--------+-------------------+-------+--------
 public | documents         | table | docpub
 public | schema_migrations | table | docpub
(2 rows)
```

Notice that `golang-migrate` automatically created the `schema_migrations` tracking table alongside `documents`.

---

### Check 2: Verify Schema Details and Constraints
Inspect the structure of `documents`:

```sql
\d documents
```

**Expected output:**
```
                                        Table "public.documents"
       Column       |           Type           | Collation | Nullable |             Default              
--------------------+--------------------------+-----------+----------+----------------------------------
 id                 | uuid                     |           | not null | uuid_generate_v4()
 title              | character varying(255)   |           | not null | 
 status             | character varying(50)    |           | not null | 'PENDING'::character varying
 source_content     | text                     |           | not null | 
 s3_key             | character varying(512)   |           |          | 
 render_duration_ms | integer                  |           |          | 0
 created_at         | timestamp with time zone |           | not null | now()
 updated_at         | timestamp with time zone |           | not null | now()
Indexes:
    "documents_pkey" PRIMARY KEY, btree (id)
    "idx_documents_created_at" btree (created_at DESC)
    "idx_documents_status" btree (status)
Check constraints:
    "chk_document_status" CHECK (status::text = ANY (ARRAY['PENDING'::character varying, 'COMPILING'::character varying, 'COMPLETED'::character varying, 'FAILED'::character varying]::text[]))
```

Exit `psql`:
```sql
\q
```

---

### Check 3: Verify the Migration Version
Query the current migration version stored in `schema_migrations`:

```bash
docker exec -it docpub-postgres psql -U docpub -d docpub -c "SELECT * FROM schema_migrations;"
```

**Expected output:**
```
 version | dirty 
---------+-------
       1 | f
(1 row)
```
- `version: 1`: Migration 1 is active.
- `dirty: f`: False (`f`) indicates the migration completed cleanly without errors.

---

### Check 4: Test Rollback and Re-application
Verify that the `down` migration works properly:

```bash
make migrate-down
```
**Sample output:**
```
1/d create_documents (8.1201ms)
```

Now re-apply the migration:
```bash
make migrate-up
```
**Sample output:**
```
1/u create_documents (12.4519ms)
```

---

## 6. Common Gotchas & Troubleshooting

1. **Dirty Database State (`error: Dirty database version 1`):**
   - *Symptom*: When a migration file has a SQL syntax error halfway through, `golang-migrate` stops and flags `dirty = true` in `schema_migrations`. Subsequent runs fail with `error: Dirty database version X`.
   - *Cause*: A partial migration was executed. PostgreSQL does not automatically roll back multi-statement DDL errors if transactions are not handled by the runner.
   - *Fix*:
     1. Open `psql` and manually fix or drop the partial table.
     2. Run `make migrate-force` and enter the clean version (e.g., `0` to re-run from scratch or `1` if already manually completed).
     3. Fix the typo in your `.sql` file and run `make migrate-up`.

2. **Connection Refused during Migration:**
   - *Symptom*: `dial tcp 127.0.0.1:5432: connect: connection refused`.
   - *Cause*: PostgreSQL container is still initializing or stopped.
   - *Fix*: Check container health with `make ps`. Ensure `docpub-postgres` has status `Up (healthy)`.

3. **`pgxpool` Context Deadline Exceeded on Boot:**
   - *Symptom*: `pinging database: context deadline exceeded`.
   - *Cause*: Incorrect credentials in DSN or firewall blocking port 5432.
   - *Fix*: Verify your connection string: `postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable`.

---

## 7. Summary & Next Steps

### Key Takeaways
- Versioned SQL migrations (`golang-migrate`) track schema evolution cleanly in source control without magic ORMs.
- Raw SQL gives exact control over constraints (`chk_document_status`), data types (`UUID`, `TIMESTAMPTZ`), and performance indexes.
- Connection pools (`pgxpool.Pool`) reuse established TCP connections, shielding PostgreSQL from resource exhaustion under high concurrency.
- `pool.Ping()` with a timeout context verifies database reachability and credentials immediately during service startup.

### Self-Check Questions Before Advancing
1. What does the `dirty` column in `schema_migrations` indicate, and how do you recover if a migration fails halfway?
2. Why is storing timestamps as `TIMESTAMPTZ` better than storing them as bare `TIMESTAMP`?
3. Why should an application configure `MaxConns` on its connection pool rather than leaving it unbounded?

---

In **[Chapter 4: The Documents Store](04-the-documents-store.md)**, we will build the data-access layer (`internal/store/documents.go`) implementing raw SQL queries (`Insert`, `GetByID`, `UpdateStatus`) with context deadlines and domain models.
