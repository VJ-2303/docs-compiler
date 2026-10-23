# Chapter 5: The API Server and Handlers

Welcome to Chapter 5. In Phase 1, we established our local Docker infrastructure, configured PostgreSQL connection pooling with `pgxpool`, ran versioned database migrations, and implemented the data-access layer in `internal/store`.

We now enter **Phase 2: The API Service (HTTP & Event Ingestion)**.

In this chapter, we will build the public HTTP transport layer for the API Service using Go's standard library. By the end of this chapter, your service will expose endpoints to ingest raw Markdown documents (`POST /v1/documents`), poll document processing status (`GET /v1/documents/{id}`), enforce strict payload validation, log structured JSON events with `log/slog`, and gracefully recover from panics.

---

## 1. Objectives & Overview

In this chapter, you will:
- Implement a zero-framework HTTP server using Go 1.22+ enhanced `net/http.ServeMux` routing.
- Construct a robust dependency injection container via an `Application` struct.
- Build production-ready HTTP middleware for panic recovery and structured request logging using `log/slog`.
- Write reusable JSON decoding and encoding helpers with payload size limits to guard against memory exhaustion.
- Create a lightweight, reflection-free validation engine to enforce API input constraints.
- Implement the `POST /v1/documents` and `GET /v1/documents/{id}` handlers.
- Verify the server using `curl` and inspect structured JSON log output.

### HTTP Request Lifecycle Architecture

```
                                 [ HTTP Client (curl / Web) ]
                                               │
                                 POST /v1/documents (JSON)
                                               ▼
                              ┌──────────────────────────────────┐
                              │     net/http Server (:8080)      │
                              └────────────────┬─────────────────┘
                                               │
                                               ▼
                              ┌──────────────────────────────────┐
                              │ Middleware: recoverPanic()       │  Catches runtime exceptions,
                              │                                  │  returns 500 Internal Error
                              └────────────────┬─────────────────┘
                                               │
                                               ▼
                              ┌──────────────────────────────────┐
                              │ Middleware: logRequest()         │  Emits structured slog JSON:
                              │                                  │  method, uri, status, duration
                              └────────────────┬─────────────────┘
                                               │
                                               ▼
                              ┌──────────────────────────────────┐
                              │ Go 1.22+ ServeMux Router         │  Matches method + path:
                              │                                  │  "POST /v1/documents"
                              └────────────────┬─────────────────┘
                                               │
                                               ▼
                              ┌──────────────────────────────────┐
                              │ Handler: createDocumentHandler() │
                              │ 1. readJSON() (max 1MB payload)  │
                              │ 2. Validate input struct         │
                              │ 3. store.Insert() (PostgreSQL)   │
                              │ 4. writeJSON() -> 202 Accepted   │
                              └────────────────┬─────────────────┘
                                               │
                                               ▼
                              ┌──────────────────────────────────┐
                              │ PostgreSQL Store (Phase 1)       │
                              │ Status: PENDING                  │
                              └──────────────────────────────────┘
```

---

## 2. Conceptual Deep Dive (The "Why")

### Why `net/http` and Go 1.22+ ServeMux?
For years, Go developers turned to third-party routing packages such as `chi`, `gorilla/mux`, or heavy frameworks like `gin` because the standard library's `http.ServeMux` only supported exact prefix matches. It could not match HTTP methods (`POST` vs `GET`) or extract dynamic path variables (`{id}`).

Go 1.22 resolved this fundamentally by adding method-matching and wildcard patterns directly to `http.ServeMux`:

```go
mux.HandleFunc("POST /v1/documents", app.createDocumentHandler)
mux.HandleFunc("GET /v1/documents/{id}", app.getDocumentHandler)
```

Inside your handler, extracting a URL wildcard requires a single standard library call:
```go
id := r.PathValue("id")
```

By relying on the standard library:
1. **Zero External Dependencies**: Eliminates supply chain risks, breaking API changes, and maintenance abandonments.
2. **Transferable Skills**: Every Go engineer understands `net/http`. Framework-specific idioms become obsolete; standard interfaces endure.
3. **Optimized Performance**: The standard library multiplexer uses an efficient routing tree with minimal allocations.

### Why Structured Logging with `log/slog`?
In modern distributed systems, plain text logs (`log.Printf("handled request in %v", d)`) are an anti-pattern. Log aggregators (such as Grafana Loki, Datadog, or Elasticsearch) cannot easily parse or index unstructured strings without brittle regular expressions.

Go 1.21 introduced `log/slog` as the standard library structured logger. Emitting logs in JSON format allows downstream observability platforms to index fields natively:

```json
{"time":"2026-10-23T15:00:00Z","level":"INFO","msg":"request completed","method":"POST","uri":"/v1/documents","status":202,"duration_ms":12.4}
```

### The Onion-Ring Middleware Pattern
In Go, HTTP middleware adheres to a simple, functional signature:

```go
func(http.Handler) http.Handler
```

Middleware functions intercept incoming requests before they reach your handler, and can inspect or modify responses before they return to the client. Wrapping handlers in this "onion" structure centralizes cross-cutting concerns:
- **Panic Recovery**: In Go, an unrecovered runtime panic inside an active goroutine terminates the entire operating system process. The recovery middleware intercepts panics with `recover()`, logs the stack trace, and sends a clean `500 Internal Server Error` response, keeping the server alive.
- **Request Logging**: Captures start time, passes execution down the chain, intercepts the response status code, and writes a single structured log entry when the request completes.

### Why Return `202 Accepted` for Ingestion?
Our publishing pipeline is asynchronous. Compiling CommonMark, highlighting code blocks, embedding styling, and uploading rendered artifacts to MinIO takes tens or hundreds of milliseconds.

If our `POST /v1/documents` endpoint waited for compilation to finish, clients would experience high latency, and network timeouts would cause duplicate submissions.

Instead, we follow standard REST conventions for background processing:
1. Validate the Markdown syntax and payload size.
2. Insert a record with status `PENDING` into PostgreSQL.
3. Immediately respond with **`202 Accepted`** containing the document UUID and metadata in ~15ms.
4. (In Chapter 6, we will publish a `doc.submitted` event to RabbitMQ to trigger background processing).

Clients can then poll `GET /v1/documents/{id}` to inspect status updates.

---

## 3. Step-by-Step Implementation

We will organize our API layer across two packages:
- `internal/validator`: A reusable, reflection-free validation utility.
- `internal/api`: Handlers, routing, middleware, and HTTP helpers.
- `cmd/api/main.go`: Application initialization, configuration, and server startup.

```
cmd/api/
└── main.go                  # Application entry point, flag parsing, server lifecycle
internal/
├── api/
│   ├── api.go               # Application struct & dependencies
│   ├── errors.go            # Centralized JSON error responses
│   ├── handlers.go          # POST /v1/documents and GET /v1/documents/{id}
│   ├── helpers.go           # readJSON and writeJSON envelope utilities
│   ├── middleware.go        # panic recovery and request logging
│   └── routes.go            # ServeMux route registration and middleware chain
└── validator/
    └── validator.go         # Lightweight map-based input validation
```

---

### Step 5.1: The Lightweight Validator

Create `internal/validator/validator.go`. This utility holds validation failure messages mapped by field name, avoiding external dependencies or complex reflection.

```go
// internal/validator/validator.go
package validator

import "strings"

// Validator stores validation error messages for specific struct fields.
type Validator struct {
	Errors map[string]string
}

// New constructs an empty Validator ready for assertions.
func New() *Validator {
	return &Validator{Errors: make(map[string]string)}
}

// Valid returns true if no validation errors were recorded.
func (v *Validator) Valid() bool {
	return len(v.Errors) == 0
}

// AddError records an error message for a given field if not already present.
func (v *Validator) AddError(field, message string) {
	if _, exists := v.Errors[field]; !exists {
		v.Errors[field] = message
	}
}

// Check evaluates a boolean condition; if false, it records the error message.
func (v *Validator) Check(ok bool, field, message string) {
	if !ok {
		v.AddError(field, message)
	}
}

// NotBlank verifies that a string contains at least one non-whitespace character.
func NotBlank(value string) bool {
	return strings.TrimSpace(value) != ""
}

// MaxChars verifies that a string does not exceed a maximum character length.
func MaxChars(value string, max int) bool {
	return len([]rune(value)) <= max
}
```

---

### Step 5.2: JSON Envelope & Decoding Helpers

Create `internal/api/helpers.go`.

We enforce a consistent JSON structure by wrapping all API responses inside a parent `envelope` map (`{"document": { ... }}`). We also implement `readJSON()` with defensive limits:
- Limit payload sizes to 1MB using `http.MaxBytesReader` to prevent denial-of-service via memory exhaustion.
- Call `disallowUnknownFields()` on the JSON decoder to reject unexpected properties.

```go
// internal/api/helpers.go
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// envelope defines a generic top-level wrapper for consistent JSON payloads.
type envelope map[string]any

// writeJSON serializes data to JSON, sets the Content-Type header, and writes the response.
func (app *Application) writeJSON(w http.ResponseWriter, status int, data envelope, headers http.Header) error {
	js, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshaling json: %w", err)
	}

	for key, value := range headers {
		w.Header()[key] = value
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(js)
	return err
}

// readJSON decodes a JSON request body into a destination struct, applying defensive limits.
func (app *Application) readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	// Limit request body size to 1MB to prevent memory exhaustion attacks
	maxBytes := int64(1_048_576)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err != nil {
		return fmt.Errorf("decoding request json: %w", err)
	}

	// Ensure the request body contains only a single JSON value
	err = dec.Decode(&struct{}{})
	if !errors.Is(err, io.EOF) {
		return errors.New("body must only contain a single JSON value")
	}

	return nil
}
```

---

### Step 5.3: Centralized Error Responses

Create `internal/api/errors.go`. 

Centralizing error responses ensures clients receive identical JSON structures (`{"error": "message"}` or `{"error": { "field": "reason" }}`) for all failures across every endpoint.

```go
// internal/api/errors.go
package api

import (
	"net/http"
)

// logError writes structured error logs containing request context.
func (app *Application) logError(r *http.Request, err error) {
	app.Logger.Error(
		err.Error(),
		"method", r.Method,
		"uri", r.URL.RequestURI(),
	)
}

// errorResponse writes a structured JSON error envelope to the client.
func (app *Application) errorResponse(w http.ResponseWriter, r *http.Request, status int, message any) {
	env := envelope{"error": message}
	err := app.writeJSON(w, status, env, nil)
	if err != nil {
		app.logError(r, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// serverErrorResponse logs unexpected 500 errors and returns a generic message to the client.
func (app *Application) serverErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.logError(r, err)
	message := "the server encountered a problem and could not process your request"
	app.errorResponse(w, r, http.StatusInternalServerError, message)
}

// notFoundResponse sends a 404 Not Found JSON response.
func (app *Application) notFoundResponse(w http.ResponseWriter, r *http.Request) {
	message := "the requested resource could not be found"
	app.errorResponse(w, r, http.StatusNotFound, message)
}

// badRequestResponse sends a 400 Bad Request JSON response.
func (app *Application) badRequestResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.errorResponse(w, r, http.StatusBadRequest, err.Error())
}

// failedValidationResponse sends a 422 Unprocessable Entity response containing validation errors.
func (app *Application) failedValidationResponse(w http.ResponseWriter, r *http.Request, errors map[string]string) {
	app.errorResponse(w, r, http.StatusUnprocessableEntity, errors)
}
```

---

### Step 5.4: Panic Recovery & Structured Logging Middleware

Create `internal/api/middleware.go`.

We introduce two middleware handlers:
1. `recoverPanic`: Protects against server crashes.
2. `logRequest`: Intercepts the HTTP status code and latency using a custom `responseWriter` wrapper.

```go
// internal/api/middleware.go
package api

import (
	"fmt"
	"net/http"
	"time"
)

// statusResponseWriter intercepts and stores the HTTP status code written by handlers.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// recoverPanic intercepts unhandled runtime panics and returns a 500 Internal Server Error.
func (app *Application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if pv := recover(); pv != nil {
				w.Header().Set("Connection", "close")
				app.serverErrorResponse(w, r, fmt.Errorf("%v", pv))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// logRequest records request duration, method, URL, and status code via structured slog logs.
func (app *Application) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(srw, r)

		app.Logger.Info(
			"request processed",
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", srw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
```

---

### Step 5.5: The Application Dependency Container

Create `internal/api/api.go`.

This struct encapsulates all dependencies needed by the HTTP transport layer: the configuration, the document store from Chapter 4, and the structured logger.

```go
// internal/api/api.go
package api

import (
	"log/slog"

	"github.com/vj/docx/internal/store"
)

// Config holds runtime configuration options for the API service.
type Config struct {
	Port int
	Env  string
}

// Application holds the operational dependencies for our HTTP handlers and middleware.
type Application struct {
	Config Config
	Store  *store.DocumentStore
	Logger *slog.Logger
}

// New constructs an Application instance with injected dependencies.
func New(cfg Config, store *store.DocumentStore, logger *slog.Logger) *Application {
	return &Application{
		Config: cfg,
		Store:  store,
		Logger: logger,
	}
}
```

> [!NOTE]
> If your `go.mod` module path is different from `github.com/vj/docx`, replace the import prefix with your project's exact module path.

---

### Step 5.6: Document Handlers

Create `internal/api/handlers.go`.

Here, we implement:
1. `createDocumentHandler`: Decodes incoming JSON, validates the fields, inserts the document into PostgreSQL with status `PENDING`, and returns `202 Accepted`.
2. `getDocumentHandler`: Extracts the `{id}` route wildcard, parses it into a `uuid.UUID`, fetches the record from PostgreSQL, and returns `200 OK`.

```go
// internal/api/handlers.go
package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/vj/docx/internal/store"
	"github.com/vj/docx/internal/validator"
)

// healthcheckHandler returns a simple JSON status to verify server availability.
func (app *Application) healthcheckHandler(w http.ResponseWriter, r *http.Request) {
	env := envelope{
		"status":      "available",
		"environment": app.Config.Env,
		"version":     "1.0.0",
	}

	err := app.writeJSON(w, http.StatusOK, env, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

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

	// Persist document to database
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

	// Return 202 Accepted per asynchronous ingestion design
	err = app.writeJSON(w, http.StatusAccepted, envelope{"document": doc}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// getDocumentHandler handles GET /v1/documents/{id}.
func (app *Application) getDocumentHandler(w http.ResponseWriter, r *http.Request) {
	// Extract wildcard {id} using Go 1.22+ ServeMux routing
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		app.notFoundResponse(w, r)
		return
	}

	doc, err := app.Store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRecordNotFound) {
			app.notFoundResponse(w, r)
			return
		}
		app.serverErrorResponse(w, r, err)
		return
	}

	err = app.writeJSON(w, http.StatusOK, envelope{"document": doc}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
```

---

### Step 5.7: Routing and Middleware Pipeline

Create `internal/api/routes.go`.

We register our routes on `http.NewServeMux()` and chain the middlewares:
```
recoverPanic -> logRequest -> ServeMux -> Handler
```

```go
// internal/api/routes.go
package api

import (
	"net/http"
)

// Routes sets up application routes and attaches the middleware pipeline.
func (app *Application) Routes() http.Handler {
	mux := http.NewServeMux()

	// Register routes with HTTP method prefix (Go 1.22+ syntax)
	mux.HandleFunc("GET /v1/healthz", app.healthcheckHandler)
	mux.HandleFunc("POST /v1/documents", app.createDocumentHandler)
	mux.HandleFunc("GET /v1/documents/{id}", app.getDocumentHandler)

	// Wrap mux with middleware chain
	return app.recoverPanic(app.logRequest(mux))
}
```

---

### Step 5.8: Application Entry Point & Server Startup

Create `cmd/api/main.go`.

Here, we tie the system together:
1. Parse flags for port, environment, and PostgreSQL DSN.
2. Initialize `slog.NewJSONHandler` for standard output.
3. Establish the database pool using `database.New` from Chapter 3.
4. Instantiate the `store.DocumentStore` from Chapter 4.
5. Create the `api.Application` container and configure `http.Server` with explicit connection timeouts.
6. Start the server.

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
	"github.com/vj/docx/internal/database"
	"github.com/vj/docx/internal/store"
)

func main() {
	var cfg api.Config
	var dsn string

	flag.IntVar(&cfg.Port, "port", 8080, "API server port")
	flag.StringVar(&cfg.Env, "env", "development", "Environment (development|staging|production)")
	flag.StringVar(&dsn, "db-dsn", "postgres://docpub:docpub_secret@localhost:5432/docpub?sslmode=disable", "PostgreSQL DSN")
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

	// Instantiate document store and API application
	docStore := store.NewDocumentStore(dbPool)
	app := api.New(cfg, docStore, logger)

	// Configure HTTP server with resilient production timeouts
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

---

## 4. Verification Playbook

Follow these steps to build, run, and verify your API service against the live infrastructure.

### Check 1: Ensure PostgreSQL Infrastructure is Healthy
Ensure your Docker containers from Chapter 2 are running:

```bash
make ps
```

Verify that `docpub-postgres` has status `Up (healthy)`. If not, start the containers:

```bash
make up
```

---

### Check 2: Start the API Service
Run the API service from your terminal:

```bash
go run ./cmd/api
```

**Expected output (structured JSON logs in terminal):**
```json
{"time":"2026-10-23T15:20:01.123Z","level":"INFO","msg":"database connection pool established"}
{"time":"2026-10-23T15:20:01.124Z","level":"INFO","msg":"starting server","addr":":8080","env":"development"}
```

---

### Check 3: Verify the Healthcheck Endpoint
Open a second terminal window and make a `GET` request to `/v1/healthz`:

```bash
curl -i http://localhost:8080/v1/healthz
```

**Terminal Flag Explanations:**
- `-i`: Includes HTTP response headers in the output.

**Expected output:**
```http
HTTP/1.1 200 OK
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:20:10 GMT
Content-Length: 64

{"environment":"development","status":"available","version":"1.0.0"}
```

---

### Check 4: Test Input Validation Errors (`422 Unprocessable Entity`)
Submit a request with an empty `title` and missing `source_content`:

```bash
curl -i -X POST http://localhost:8080/v1/documents \
  -H "Content-Type: application/json" \
  -d '{"title": ""}'
```

**Terminal Flag Explanations:**
- `-X POST`: Sets the HTTP request method to POST.
- `-H "Content-Type: application/json"`: Informs the server that the payload is JSON.
- `-d '...'`: Supplies the raw request body data.

**Expected output:**
```http
HTTP/1.1 422 Unprocessable Entity
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:21:00 GMT
Content-Length: 85

{"error":{"source_content":"must be provided","title":"must be provided"}}
```

---

### Check 5: Ingest a Valid Markdown Document (`202 Accepted`)
Submit a valid Markdown document payload:

```bash
curl -i -X POST http://localhost:8080/v1/documents \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Architecture Overview",
    "source_content": "# Event-Driven Architecture\n\nDecoupling ingestion from compilation."
  }'
```

**Expected output:**
```http
HTTP/1.1 202 Accepted
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:22:15 GMT
Content-Length: 268

{
  "document": {
    "id": "e3b0c442-98fc-1c14-9afb-4c7b94420000",
    "title": "Architecture Overview",
    "status": "PENDING",
    "source_content": "# Event-Driven Architecture\n\nDecoupling ingestion from compilation.",
    "render_duration_ms": 0,
    "created_at": "2026-10-23T15:22:15.891234Z",
    "updated_at": "2026-10-23T15:22:15.891234Z"
  }
}
```

Copy the returned `id` UUID from the response JSON.

---

### Check 6: Query Document Status (`200 OK`)
Fetch the document state using the UUID copied from Check 5:

```bash
curl -i http://localhost:8080/v1/documents/e3b0c442-98fc-1c14-9afb-4c7b94420000
```

**Expected output:**
```http
HTTP/1.1 200 OK
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:23:00 GMT
Content-Length: 268

{
  "document": {
    "id": "e3b0c442-98fc-1c14-9afb-4c7b94420000",
    "title": "Architecture Overview",
    "status": "PENDING",
    "source_content": "# Event-Driven Architecture\n\nDecoupling ingestion from compilation.",
    "render_duration_ms": 0,
    "created_at": "2026-10-23T15:22:15.891234Z",
    "updated_at": "2026-10-23T15:22:15.891234Z"
  }
}
```

---

### Check 7: Query a Non-Existent Document (`404 Not Found`)
Submit a query for a random UUID that does not exist in the database:

```bash
curl -i http://localhost:8080/v1/documents/00000000-0000-0000-0000-000000000000
```

**Expected output:**
```http
HTTP/1.1 404 Not Found
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:24:00 GMT
Content-Length: 53

{"error":"the requested resource could not be found"}
```

---

### Check 8: Verify Server Structured Logs
Inspect the terminal running `go run ./cmd/api`. You should see structured logs for each request processed:

```json
{"time":"2026-10-23T15:20:10.001Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/healthz","status":200,"duration_ms":0}
{"time":"2026-10-23T15:21:00.042Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":422,"duration_ms":1}
{"time":"2026-10-23T15:22:15.895Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":202,"duration_ms":14}
{"time":"2026-10-23T15:23:00.120Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/documents/e3b0c442-98fc-1c14-9afb-4c7b94420000","status":200,"duration_ms":2}
{"time":"2026-10-23T15:24:00.088Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/documents/00000000-0000-0000-0000-000000000000","status":404,"duration_ms":1}
```

---

## 5. Common Gotchas & Troubleshooting

1. **Port Already in Use (`listen tcp :8080: bind: address already in use`):**
   - *Symptom*: Server exits immediately on startup with `bind: address already in use`.
   - *Cause*: A previously started instance of the API server or another local service is already listening on port 8080.
   - *Fix (Arch Linux)*: Find the process occupying port 8080 using `ss` and terminate it:
     ```bash
     ss -tulpn | grep :8080
     kill -9 <PID>
     ```
     Or start the server on a different port:
     ```bash
     go run ./cmd/api -port=8081
     ```

2. **Malformed JSON or Unknown Fields (`400 Bad Request`):**
   - *Symptom*: Sending `{ "title": "Test", "extra_field": "bad" }` returns `400 Bad Request`.
   - *Cause*: `readJSON()` deliberately calls `dec.DisallowUnknownFields()` to protect your database models from unrecognized payload attributes.
   - *Fix*: Ensure the client sends only expected keys (`title`, `source_content`).

3. **Trailing Slashes in Go 1.22+ Routes:**
   - *Symptom*: Requesting `POST /v1/documents/` redirects to `POST /v1/documents` with `301 Moved Permanently`.
   - *Cause*: In Go's `http.ServeMux`, paths without trailing slashes (`/v1/documents`) and paths with trailing slashes (`/v1/documents/`) represent distinct subtrees.
   - *Fix*: Always omit trailing slashes for specific REST resources.

4. **Missing Production Server Timeouts:**
   - *Symptom*: High memory usage under slow client connections (Slowloris attacks).
   - *Cause*: Go's default `http.ListenAndServe(":8080", mux)` leaves `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` unset, allowing hung client connections to stay open indefinitely.
   - *Fix*: Always instantiate `&http.Server{}` explicitly with defensive timeouts, as demonstrated in `main.go`.

---

## 6. Summary & Next Steps

### Key Takeaways
- Go 1.22+ enhanced `http.ServeMux` eliminates the need for third-party web frameworks, providing native method routing and path parameter extraction.
- Structured logging with `log/slog` produces machine-readable JSON logs ready for centralized aggregation.
- The `Application` struct pattern enables explicit, testable dependency injection without global state.
- Defensive JSON parsing (`http.MaxBytesReader`, `DisallowUnknownFields`) protects memory and models from malformed requests.
- Returning `202 Accepted` decouples synchronous ingestion from asynchronous compilation.

### Self-Check Questions Before Advancing
1. Why does `createDocumentHandler` return `202 Accepted` instead of `201 Created`?
2. What happens to a Go HTTP server when an unrecovered panic occurs in a handler goroutine?
3. How does `http.MaxBytesReader` prevent memory exhaustion attacks?
4. How do Go 1.22+ routing patterns match both HTTP method and URL path wildcards?

---

In **[Chapter 6: Publishing Events to RabbitMQ](06-publishing-events-to-rabbitmq.md)**, we will connect the API Service to RabbitMQ. After inserting the document into PostgreSQL, the API Service will publish a persistent `doc.submitted` event to the `docs` exchange, initiating the asynchronous publishing workflow.
