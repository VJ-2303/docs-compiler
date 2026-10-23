# Chapter 4: The Documents Store

Welcome to Chapter 4. In the previous chapter, we established our database connection pool and ran our first versioned SQL migration, creating the `documents` table in PostgreSQL.

Now, we will build the data-access layer for the API Service: the **Documents Store**.

In production Go applications, scattering raw SQL queries inside HTTP handlers creates tightly coupled, difficult-to-test code. If a table column changes or an error-handling convention shifts, you are forced to audit every handler file in your project.

Instead, we follow the approach taught by Alex Edwards: encapsulating all database interactions behind a dedicated store struct in `internal/store/documents.go`. Every query is explicit, parameterized to prevent SQL injection, and receives a `context.Context` to support cancellation and timeouts.

---

## 1. Objectives & Overview

In this chapter, you will:
- Learn why encapsulating SQL in a dedicated store layer isolates database driver dependencies from your transport layer (HTTP/gRPC).
- Define clean Go domain models with custom status types and sentinel errors (`ErrRecordNotFound`).
- Implement parameterized SQL queries for inserting, querying, and updating document records.
- Learn how `context.Context` cancellation propagates to PostgreSQL to terminate abandoned server-side queries.
- Write a clean integration test using Go's standard `testing` package to verify our store against the real PostgreSQL container.

---

## 2. Conceptual Deep Dive (The "Why")

### The Data Access Layer Pattern
The role of our store is to act as an intermediary between database rows and Go structs:

```
+-----------------------------------------------------------+
| Transport Layer (HTTP Handlers in cmd/api)                |
| - Decodes JSON requests                                   |
| - Validates user input                                    |
| - Calls store.Insert() or store.GetByID()                 |
+-----------------------------+-----------------------------+
                              |
                     Domain Structs & Errors
                     (models.Document, ErrRecordNotFound)
                              |
                              v
+-----------------------------+-----------------------------+
| Store Layer (internal/store/documents.go)                 |
| - Holds reference to *pgxpool.Pool                        |
| - Executes raw, parameterized SQL queries                 |
| - Translates pgx errors into clean domain errors          |
+-----------------------------+-----------------------------+
                              |
                     pgx Binary Protocol
                              |
                              v
+-----------------------------+-----------------------------+
| PostgreSQL Engine (Docker)                                |
| - documents table                                         |
+-----------------------------------------------------------+
```

### Why Custom Sentinel Errors?
When a query finds no matching row, `pgx` returns `pgx.ErrNoRows`. If our HTTP handlers checked directly for `errors.Is(err, pgx.ErrNoRows)`, our HTTP handlers would become directly coupled to the `pgx` driver.

If we ever switched drivers, or if we wanted to mock our store during unit testing, our HTTP handlers would break.

Instead, our store catches `pgx.ErrNoRows` and translates it into our own sentinel error: `store.ErrRecordNotFound`. The HTTP handler only needs to know that the record wasn't found—it doesn't care which driver discovered that fact.

### Why Parameterized Queries?
Never construct SQL queries using string concatenation or `fmt.Sprintf`:

```go
// DANGEROUS: Vulnerable to SQL Injection
query := fmt.Sprintf("SELECT * FROM documents WHERE title = '%s'", userInput)
```

If `userInput` contains `' OR 1=1; --`, an attacker can compromise your database.

We use **parameterized placeholders** (`$1`, `$2`, `$3` in PostgreSQL):

```go
// SAFE: Arguments sent separately over the binary wire protocol
query := `SELECT id, title FROM documents WHERE title = $1`
row := pool.QueryRow(ctx, query, userInput)
```

PostgreSQL compiles the query plan first, treating user input purely as literal data values.

### Context Cancellation and Server-Side Query Aborts
When an HTTP client aborts an upload or closes their browser tab, Go's HTTP server cancels the request's `r.Context()`.

Because we pass `ctx` into `pool.QueryRow(ctx, ...)` or `pool.Exec(ctx, ...)`, `pgx` detects the cancellation signal, closes the query stream, and sends an interruption packet to PostgreSQL. PostgreSQL immediately halts the query, freeing CPU and locks instead of wasting resources on an abandoned request.

---

## 3. Step-by-Step Implementation

We will create our models and store methods in `internal/store/documents.go`. To adhere to the 30–60 line rule, we build this file in four focused steps.

### Step 3.1: Domain Models and Sentinel Errors
First, let's define our document status type, domain struct, and sentinel error.

```go
// internal/store/documents.go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRecordNotFound is returned when a requested document does not exist.
var ErrRecordNotFound = errors.New("record not found")

// DocumentStatus represents the state of a document in our pipeline.
type DocumentStatus string

const (
	StatusPending   DocumentStatus = "PENDING"
	StatusCompiling DocumentStatus = "COMPILING"
	StatusCompleted DocumentStatus = "COMPLETED"
	StatusFailed    DocumentStatus = "FAILED"
)

// Document represents a single document record in our system.
type Document struct {
	ID               uuid.UUID      `json:"id"`
	Title            string         `json:"title"`
	Status           DocumentStatus `json:"status"`
	SourceContent    string         `json:"source_content"`
	S3Key            *string        `json:"s3_key,omitempty"`
	RenderDurationMS int            `json:"render_duration_ms"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}
```

**Line-by-line explanation:**
- `ErrRecordNotFound`: Exported sentinel error. Handlers can inspect it with `errors.Is(err, store.ErrRecordNotFound)` to return an HTTP `404 Not Found`.
- `DocumentStatus`: A dedicated type preventing invalid string assignments at compile time.
- `S3Key *string`: Stored as a pointer to string because this column is nullable in PostgreSQL (`NULL` when a document is first submitted). If `NULL`, Go parses it as `nil`.

---

### Step 3.2: Store Struct and Constructor
Next, we define the `DocumentStore` struct which wraps our PostgreSQL connection pool.

```go
// DocumentStore manages persistence operations for documents.
type DocumentStore struct {
	pool *pgxpool.Pool
}

// NewDocumentStore creates a new DocumentStore instance.
func NewDocumentStore(pool *pgxpool.Pool) *DocumentStore {
	return &DocumentStore{
		pool: pool,
	}
}
```

**Explanation:**
- `pool *pgxpool.Pool`: Injected explicitly. No global variables.
- Methods on `DocumentStore` borrow connections from this pool automatically when running queries.

---

### Step 3.3: Implementing `Insert()`
Now we implement the method to create a new document record.

```go
// Insert creates a new document in the database and populates generated fields.
func (s *DocumentStore) Insert(ctx context.Context, doc *Document) error {
	query := `
		INSERT INTO documents (title, status, source_content)
		VALUES ($1, $2, $3)
		RETURNING id, created_at, updated_at
	`

	// Ensure default status is set if empty
	if doc.Status == "" {
		doc.Status = StatusPending
	}

	// Set a reasonable context timeout if caller did not provide one
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	err := s.pool.QueryRow(
		ctx,
		query,
		doc.Title,
		doc.Status,
		doc.SourceContent,
	).Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)

	if err != nil {
		return fmt.Errorf("inserting document: %w", err)
	}

	return nil
}
```

**Line-by-line explanation:**
- `RETURNING id, created_at, updated_at`: PostgreSQL generates the UUID and timestamps. The `RETURNING` clause sends them back in the same round-trip.
- `.Scan(&doc.ID, ...)`: Scans the database-generated values directly back into our pointer receiver struct, mutating the caller's struct with the assigned ID.
- `%w` error wrapping: Wraps internal database errors while preserving root cause details for debugging.

---

### Step 3.4: Implementing `GetByID()`
Now we implement fetching a document by its UUID.

```go
// GetByID fetches a single document by its UUID.
func (s *DocumentStore) GetByID(ctx context.Context, id uuid.UUID) (*Document, error) {
	query := `
		SELECT id, title, status, source_content, s3_key, render_duration_ms, created_at, updated_at
		FROM documents
		WHERE id = $1
	`

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var doc Document
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&doc.ID,
		&doc.Title,
		&doc.Status,
		&doc.SourceContent,
		&doc.S3Key,
		&doc.RenderDurationMS,
		&doc.CreatedAt,
		&doc.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, fmt.Errorf("fetching document by id: %w", err)
	}

	return &doc, nil
}
```

**Line-by-line explanation:**
- `errors.Is(err, pgx.ErrNoRows)`: We explicitly intercept `pgx.ErrNoRows` and translate it into our domain `ErrRecordNotFound`.
- If an actual database error occurs (e.g. lost network connection), it is wrapped with context and returned.

---

### Step 3.5: Implementing `UpdateStatus()`
When the Compiler Service compiles our Markdown file and uploads the artifact to MinIO, our system will update the document's status, duration, and S3 key.

```go
// UpdateStatus updates the compilation state and artifact location of a document.
func (s *DocumentStore) UpdateStatus(ctx context.Context, id uuid.UUID, status DocumentStatus, s3Key *string, durationMS int) error {
	query := `
		UPDATE documents
		SET status = $2, s3_key = $3, render_duration_ms = $4, updated_at = NOW()
		WHERE id = $1
	`

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	result, err := s.pool.Exec(ctx, query, id, status, s3Key, durationMS)
	if err != nil {
		return fmt.Errorf("updating document status: %w", err)
	}

	// If no rows were updated, the document ID does not exist
	if result.RowsAffected() == 0 {
		return ErrRecordNotFound
	}

	return nil
}
```

**Line-by-line explanation:**
- `pool.Exec()`: Used when we don't need to read returned row values, only execution metadata.
- `result.RowsAffected()`: If an update query runs against a non-existent UUID, PostgreSQL does not return an error—it simply updates 0 rows. Checking `RowsAffected() == 0` lets us reliably return `ErrRecordNotFound`.

---

## 4. Terminal Commands & Infrastructure Execution

### Step 4.1: Install the UUID Library
Our domain models use `github.com/google/uuid` for RFC 4122 compliant UUID handling.

Install it from your terminal:

```bash
go get github.com/google/uuid
go mod tidy
```

---

### Step 4.2: Writing an Automated Store Test
To verify our store against PostgreSQL without needing to boot the full HTTP server, we write an automated test in `internal/store/documents_test.go`.

Create `internal/store/documents_test.go`:

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/vj/new-ms/internal/database"
	"github.com/vj/new-ms/internal/store"
)

func TestDocumentStore_CRUD(t *testing.T) {
	ctx := context.Background()

	// Connect to local test database running in Docker
	pool, err := database.New(ctx, database.Config{
		DSN:             "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable",
		MaxConns:        5,
		MinConns:        2,
		MaxConnLifetime: 5 * time.Minute,
		MaxConnIdleTime: 1 * time.Minute,
	})
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	defer pool.Close()

	docStore := store.NewDocumentStore(pool)

	// 1. Test Insert
	doc := &store.Document{
		Title:         "Microservices in Go",
		SourceContent: "# Introduction\nBuilding clean systems.",
	}

	err = docStore.Insert(ctx, doc)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	if doc.ID.String() == "" {
		t.Errorf("expected generated UUID, got empty")
	}

	// 2. Test GetByID
	fetched, err := docStore.GetByID(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}

	if fetched.Title != doc.Title {
		t.Errorf("expected title %q, got %q", doc.Title, fetched.Title)
	}
	if fetched.Status != store.StatusPending {
		t.Errorf("expected status PENDING, got %q", fetched.Status)
	}

	// 3. Test UpdateStatus
	s3Key := "artifacts/" + doc.ID.String() + ".html"
	err = docStore.UpdateStatus(ctx, doc.ID, store.StatusCompleted, &s3Key, 142)
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	updated, err := docStore.GetByID(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetByID after update failed: %v", err)
	}

	if updated.Status != store.StatusCompleted {
		t.Errorf("expected status COMPLETED, got %q", updated.Status)
	}
	if updated.S3Key == nil || *updated.S3Key != s3Key {
		t.Errorf("expected S3Key %q, got %v", s3Key, updated.S3Key)
	}
}
```

---

## 5. Verification Playbook

### Check 1: Ensure Infrastructure is Active
Ensure your Docker containers from Chapter 2 are running:

```bash
make ps
```

Confirm that `docpub-postgres` shows status `Up (healthy)`.

---

### Check 2: Run the Store Test Suite
Execute the Go test suite against the store package:

```bash
go test -v ./internal/store/...
```

**Expected output:**
```
=== RUN   TestDocumentStore_CRUD
--- PASS: TestDocumentStore_CRUD (0.02s)
PASS
ok      github.com/vj/new-ms/internal/store     0.031s
```

---

### Check 3: Inspect Inserted Row via `psql`
Verify that the test record actually landed in PostgreSQL:

```bash
docker exec -it docpub-postgres psql -U docpub -d docpub -c "SELECT id, title, status, s3_key, render_duration_ms FROM documents ORDER BY created_at DESC LIMIT 1;"
```

**Expected output:**
```
                  id                  |        title        |  status   |                       s3_key                       | render_duration_ms 
--------------------------------------+---------------------+-----------+----------------------------------------------------+--------------------
 8a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d | Microservices in Go | COMPLETED | artifacts/8a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d.html|                142
(1 row)
```

---

## 6. Common Gotchas & Troubleshooting

1. **`uuid: cannot parse UUID` or Scan Errors:**
   - *Symptom*: `sql: Scan error on column index 0: converting driver.Value type string to a uuid.UUID`.
   - *Cause*: `pgx/v5` natively supports PostgreSQL's `uuid` data type, but Go's standard `database/sql` driver sometimes requires explicit byte slice parsing.
   - *Fix*: Using `github.com/google/uuid` alongside `pgx/v5` automatically registers the binary scanner for UUID types. Ensure `github.com/google/uuid` is imported.

2. **Null Pointer Dereference on `s3_key`:**
   - *Symptom*: Panic runtime error `invalid memory address or nil pointer dereference`.
   - *Cause*: Trying to read `*doc.S3Key` when a document is in `PENDING` state and `s3_key` in the database is `NULL`.
   - *Fix*: Always check `if doc.S3Key != nil` before dereferencing, or use pointer types in JSON serialization (`*string`).

3. **Silent Update Failures:**
   - *Symptom*: Calling `UpdateStatus` on a missing document returns `nil` error, but no changes happened.
   - *Cause*: In SQL, `UPDATE ... WHERE id = $1` matches 0 rows if the ID doesn't exist, which is not treated by PostgreSQL as an error.
   - *Fix*: Always inspect `result.RowsAffected() == 0` and return `ErrRecordNotFound`.

---

## 7. Summary & Next Steps

### Key Takeaways
- The data-access layer cleanly decouples HTTP and gRPC handlers from database drivers and raw SQL queries.
- Sentinel errors (`ErrRecordNotFound`) let outer layers respond with proper HTTP status codes (`404 Not Found`) without referencing `pgx.ErrNoRows`.
- Parameterized placeholders (`$1`, `$2`) eliminate SQL injection vulnerabilities by separating query structure from user input.
- `context.Context` cancellation ensures abandoned client requests do not leave runaway queries running inside PostgreSQL.

### Self-Check Questions Before Advancing
1. What is the difference between `pool.QueryRow()` and `pool.Exec()`?
2. Why is `S3Key` modeled as `*string` rather than `string` in the `Document` struct?
3. How does checking `result.RowsAffected() == 0` prevent silent update bugs?

---

This completes **Phase 1: Architecture & Foundation**.

In **[Chapter 5: Defining the gRPC Contract](05-defining-the-grpc-contract.md)**, we enter **Phase 2: Contract-First Inter-Service RPC**. We will write our Protocol Buffers schema (`compiler.proto`), compile it into Go stubs using `protoc`, and define the RPC boundary for our Compiler Service.
