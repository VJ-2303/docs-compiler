# Chapter 5: The API Server and Handlers

Welcome to Chapter 5. In Phase 1, we established our local Docker infrastructure, configured connection pooling with `pgxpool`, ran versioned raw SQL migrations, and built our isolated data-access layer in `internal/store`.

Now, we enter **Phase 2: The API Service (HTTP & Event Ingestion)**.

In production Go microservices, reaching for heavy third-party web frameworks (such as Gin, Fiber, or Echo) is an unnecessary crutch. Frameworks obscure how the HTTP wire protocol works, introduce breaking API changes across versions, and encourage the use of global state or reflection magic.

Instead, we follow the pedagogical standard championed by Alex Edwards: **standard library first, explicit dependency injection, zero-magic routing, and composable middleware**.

By the end of this chapter, your API Service will expose a high-performance REST interface capable of ingesting raw Markdown documents in milliseconds, validating payloads without reflection libraries, emitting machine-readable structured JSON logs using Go's `log/slog`, and catching unexpected runtime panics before they can crash your service.

---

## 1. Objectives & Overview

In this chapter, you will:
- Learn why Go 1.22+ enhanced `net/http.ServeMux` routing supersedes third-party routers like Chi and Gorilla Mux.
- Implement explicit dependency injection via an `Application` struct, eliminating global singletons.
- Build defensive JSON encoding and decoding helpers with strict payload size caps to prevent denial-of-service memory exhaustion.
- Create a lightweight, reflection-free validation engine that returns clear, field-level error messages.
- Construct composable HTTP middleware for panic recovery and structured request logging using `log/slog`.
- Implement `POST /v1/documents` (returning `202 Accepted` for asynchronous processing) and `GET /v1/documents/{id}`.
- Wire all components together in `cmd/api/main.go` with resilient production HTTP timeouts.
- Execute an end-to-end verification playbook against your Arch Linux host using `curl`.

### The HTTP Request Lifecycle

```
                           [ HTTP Client (curl / Web) ]
                                         │
                           POST /v1/documents (JSON)
                                         ▼
                        ┌──────────────────────────────────┐
                        │     net/http.Server (:8080)      │
                        │ - ReadTimeout: 10s               │
                        │ - WriteTimeout: 30s              │
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
                        │ 1. readJSON() (max 1MB cap)      │
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

### Why Native `net/http` Over Web Frameworks?
Beginners often assume Go requires a framework like Gin or Fiber to build web APIs. But web frameworks in Go carry substantial long-term trade-offs:
1. **Hidden Magic**: Frameworks use custom contexts (`gin.Context`), hiding the standard `http.ResponseWriter`, `*http.Request`, and `context.Context` interfaces. When something leaks memory or drops a connection, developers struggle to debug it.
2. **Ecosystem Fragmentation**: Middleware written for Gin cannot be used in standard Go libraries or other routers without complex adapters.
3. **Go 1.22 Native Routing**: Prior to Go 1.22, `http.ServeMux` only performed prefix matching, forcing developers to use third-party routers to match methods (`POST` vs `GET`) or URL parameters (`/v1/documents/{id}`). Go 1.22 introduced method routing and path wildcards natively into the standard library:

```go
mux := http.NewServeMux()
mux.HandleFunc("POST /v1/documents", app.createDocumentHandler)
mux.HandleFunc("GET /v1/documents/{id}", app.getDocumentHandler)
```

Inside your handler, retrieving a path wildcard is a clean one-liner:
```go
id := r.PathValue("id")
```

The standard library router is fast, rock-solid, and supported by Go's compatibility promise forever.

---

### Why Structured Logging with `log/slog`?
In distributed architectures, plain-text logs produced by `log.Printf()` are an operational liability:
```
// ANTI-PATTERN: Unstructured plain text
2026/10/23 15:00:00 POST /v1/documents processed in 12ms status=202
```

Centralized log collectors (such as Grafana Loki, Datadog, or Elasticsearch) cannot reliably query or aggregate plain text without expensive regular expressions. 

Go 1.21 introduced `log/slog` directly into the standard library. By emitting logs in structured JSON, log aggregators can parse fields natively, allowing your team to filter by status code, latency threshold, or endpoint instantaneously:

```json
{"time":"2026-10-23T15:00:00Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":202,"duration_ms":12}
```

---

### The Onion-Ring Middleware Pattern
In Go, idiomatic HTTP middleware follows a simple functional signature:

```go
func(http.Handler) http.Handler
```

Middleware functions wrap around an inner handler, executing code **before** and **after** passing the request down the chain:

```
Request ──► [ recoverPanic ] ──► [ logRequest ] ──► [ ServeMux ] ──► Handler
                                                                        │
Response ◄── [ recoverPanic ] ◄── [ logRequest ] ◄── [ ServeMux ] ◄─────┘
```

Why is **Panic Recovery** mandatory in Go?
In Go, each incoming HTTP request is served in its own independent goroutine. If code inside a handler encounters an unhandled runtime error (such as a nil pointer dereference) and panics without recovery:
1. The panic bubbles up the call stack of that specific goroutine.
2. Because nothing catches it, **the entire operating system process crashes immediately**.
3. All other concurrent requests being handled for other users are violently terminated.

Our panic recovery middleware uses Go's `defer` and `recover()` to intercept runtime panics, close the broken connection, log the error stack trace, and send a clean `500 Internal Server Error` response—keeping the server process alive.

---

### Explicit Dependency Injection via the `Application` Struct
Never use package-level global variables for database connections, stores, or loggers:

```go
// ANTI-PATTERN: Global shared state
var DB *pgxpool.Pool
var Logger *slog.Logger
```

Global variables cause race conditions in concurrent tests, make mocking impossible, and hide dependencies. 

Instead, we follow the pattern taught by Alex Edwards: define an `Application` struct that holds references to all dependencies needed by the handlers:

```go
type Application struct {
    Config Config
    Store  *store.DocumentStore
    Logger *slog.Logger
}
```

Any method declared on `*Application` (e.g., `func (app *Application) createDocumentHandler(...)`) has direct, thread-safe access to `app.Store` and `app.Logger` without relying on global state.

---

### Why Return `202 Accepted` for Markdown Submissions?
Our publishing pipeline is asynchronous. Compiling CommonMark, highlighting syntax, wrapping HTML templates, and uploading rendered artifacts to MinIO takes tens or hundreds of milliseconds.

If `POST /v1/documents` waited for compilation to finish:
- Clients would experience high latency.
- Slow network connections would cause timeouts and duplicate submissions.
- A sudden traffic spike could exhaust the API server's CPU.

Instead, we follow standard REST conventions for background processing:
1. Validate the Markdown syntax and payload size.
2. Insert a record with status `PENDING` into PostgreSQL.
3. Immediately respond with **`202 Accepted`** containing the document UUID and metadata in ~15ms.
4. (In Chapter 6, we will publish a `docs.submitted` event to NATS JetStream to trigger background processing).

Clients can then poll `GET /v1/documents/{id}` to inspect status updates.

---

## 3. Step-by-Step Implementation

We will organize our code across two clean packages:
- `internal/validator`: A reusable, reflection-free validation utility.
- `internal/api`: The application container, routes, middleware, handlers, and JSON helpers.
- `cmd/api/main.go`: The service entry point, flag parsing, and HTTP server lifecycle.

To adhere strictly to our 30–60 line rule, we build each component in bite-sized, digestible functional units.

---

### Step 3.1: The Lightweight Validator

Create `internal/validator/validator.go`.

Rather than pulling in heavy third-party packages that use struct tags and runtime reflection (which cannot be verified at compile time), we create a lightweight validator based on a Go map.

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

**Line-by-line explanation:**
- `type Validator struct`: Holds a map of `field -> error_message`. If multiple errors occur for the same field, the first error is preserved.
- `Check(ok bool, field, message string)`: The core assertion method. If `ok` is false, the error is recorded. This allows writing readable validation rules like `v.Check(len(title) > 0, "title", "must be provided")`.
- `NotBlank`: Uses `strings.TrimSpace` to ensure inputs consisting only of spaces or tabs are rejected.
- `MaxChars`: Converts strings to `[]rune` before checking length, ensuring multi-byte Unicode characters (e.g. emojis or accents) are counted accurately.

---

### Step 3.2: The Application Container & Dependencies

Create `internal/api/api.go`.

This struct holds the operational dependencies for our HTTP transport layer: configuration settings, the PostgreSQL document store built in Chapter 4, and the structured logger.

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

**Line-by-line explanation:**
- `Config`: Stores the listening port and environment name (e.g., `development`, `production`).
- `Store *store.DocumentStore`: Injected reference to our data-access layer. The API layer never interacts with raw database connections directly.
- `Logger *slog.Logger`: Injected structured logger instance.
- `New()`: Explicit constructor enforcing that all required dependencies are supplied at startup.

---

### Step 3.3: JSON Envelope & Defensive Request Decoding

Create `internal/api/helpers.go`. We will build this in two focused parts.

#### Part A: Writing JSON Responses (`writeJSON`)

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

	// Append any custom HTTP response headers
	for key, value := range headers {
		w.Header()[key] = value
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(js)
	return err
}
```

**Line-by-line explanation:**
- `type envelope map[string]any`: Wraps every response in a root JSON object (e.g., `{"document": { ... }}`), preventing JSON array top-level security vulnerabilities in older clients and ensuring consistency.
- `w.Header().Set("Content-Type", "application/json")`: Explicitly sets the MIME type so clients parse the response body as JSON.
- `w.WriteHeader(status)`: Writes the HTTP status code (e.g., `200 OK`, `202 Accepted`). Must be called before `w.Write()`.

---

#### Part B: Defensive Request Body Reading (`readJSON`)

Now add `readJSON` to `internal/api/helpers.go`:

```go
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

**Line-by-line explanation:**
- `http.MaxBytesReader(w, r.Body, maxBytes)`: Hard limit of 1MB (1,048,576 bytes). If a client streams an unbounded payload trying to crash the server, `MaxBytesReader` immediately terminates the stream and sets an error flag.
- `dec.DisallowUnknownFields()`: If a client sends unrecognized fields (e.g., `{"extra": "value"}`), decoding immediately fails. This catches client typos early.
- `dec.Decode(&struct{}{})`: Calling decode a second time verifies that the request body only contains a single JSON value, rejecting malformed streams like `{"title":"A"}{"title":"B"}`.

---

### Step 3.4: Centralized Error Helpers & Response Envelope

Create `internal/api/errors.go`.

Centralizing error responses guarantees that clients always receive identical JSON structures (`{"error": "message"}` or `{"error": { "field": "reason" }}`) for all failures across every endpoint.

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

**Line-by-line explanation:**
- `logError()`: Uses `app.Logger.Error` to emit structured JSON with the failed request method and URI.
- `serverErrorResponse()`: Never leaks raw database error strings or internal file paths to public clients. It logs the internal details privately and sends a safe, generic message.
- `failedValidationResponse()`: Uses HTTP `422 Unprocessable Entity` to return a map of field-specific errors so API consumers know exactly which inputs were invalid.

---

### Step 3.5: Panic Recovery and Structured Request Logging

Create `internal/api/middleware.go`. We will build this in two parts.

#### Part A: Custom `statusResponseWriter` & Panic Recovery

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
```

**Line-by-line explanation:**
- `statusResponseWriter`: Wraps Go's standard `http.ResponseWriter`. By intercepting `WriteHeader(code)`, it records the status code so downstream middleware can read it.
- `recover()`: Evaluated inside a deferred function. If an unhandled panic occurs during `next.ServeHTTP(w, r)`, `recover()` captures the panic value, prevents the Go runtime from aborting the process, and returns a 500 response.
- `w.Header().Set("Connection", "close")`: Informs the client and any intermediate proxies that the underlying TCP connection should be closed immediately following the panic.

---

#### Part B: Structured Request Logging with `log/slog`

Add the `logRequest` middleware to `internal/api/middleware.go`:

```go
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

**Line-by-line explanation:**
- `start := time.Now()`: Captures high-precision monotonic start time.
- `srw := &statusResponseWriter{...}`: Injects our response writer wrapper so we can inspect `srw.statusCode` after handler execution completes.
- `next.ServeHTTP(srw, r)`: Executes the downstream handler chain.
- `app.Logger.Info(...)`: Emits a single structured JSON log containing method, URI, final HTTP status code, and execution duration in milliseconds.

---

### Step 3.6: Implementing the HTTP Handlers

Create `internal/api/handlers.go`. We will implement our handlers across three logical parts.

#### Part A: The Healthcheck Handler & Ingestion Input Model

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
```

**Line-by-line explanation:**
- `healthcheckHandler`: Lightweight endpoint mounted at `/v1/healthz`. Used by load balancers, container orchestrators, and uptime monitors to verify the service is healthy.

---

#### Part B: Ingesting Documents (`createDocumentHandler`)

Add `createDocumentHandler` to `internal/api/handlers.go`:

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

	// Persist document to database with PENDING status
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
```

**Line-by-line explanation:**
- `var input struct`: Anonymous struct scoping request fields strictly to this handler.
- `v.Check(...)`: Performs clean, compile-time verified validations on `title` and `source_content`.
- `r.Context()`: Passes request context to `store.Insert()`. If the HTTP client disconnects while the query is running, PostgreSQL will abort the insert automatically.
- `http.StatusAccepted`: Returns HTTP `202 Accepted` to indicate the document was accepted for asynchronous compilation.

---

#### Part C: Fetching Document Status (`getDocumentHandler`)

Add `getDocumentHandler` to `internal/api/handlers.go`:

```go
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

**Line-by-line explanation:**
- `r.PathValue("id")`: Extracts the `{id}` wildcard from the URL path natively without external routing packages.
- `uuid.Parse(idStr)`: Verifies that the string matches RFC 4122 format. If invalid, returns `404 Not Found` immediately, saving a useless database query.
- `errors.Is(err, store.ErrRecordNotFound)`: Cleanly intercepts our custom sentinel error from Chapter 4 and returns an HTTP `404 Not Found`.

---

### Step 3.7: Routing and Middleware Chaining

Create `internal/api/routes.go`.

We register our endpoints on `http.NewServeMux()` and chain our middlewares.

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

	// Wrap mux with middleware chain: recoverPanic -> logRequest -> mux
	return app.recoverPanic(app.logRequest(mux))
}
```

**Line-by-line explanation:**
- `mux.HandleFunc("METHOD /path", ...)`: Go 1.22+ standard syntax. Explicit method prefixes prevent `GET` requests from accidentally triggering `POST` handlers.
- `app.recoverPanic(app.logRequest(mux))`: Wraps the router in our middleware pipeline. The outer wrapper (`recoverPanic`) executes first, protecting all inner layers.

---

### Step 3.8: Server Lifecycle & Dependency Wiring

Create `cmd/api/main.go`. We build this entrypoint in two focused parts.

#### Part A: Flags, Logger, and PostgreSQL Connection Pool

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

	// Initialize structured logger to standard output
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Connect to PostgreSQL database pool using configuration from Chapter 3
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
```

**Line-by-line explanation:**
- `flag.StringVar(&dsn, ...)`: Allows overriding the database connection string at startup via command-line flags or container arguments.
- `slog.NewJSONHandler(os.Stdout, ...)`: Emits machine-readable JSON logs directly to standard out.
- `database.New()`: Uses our `pgxpool.Pool` constructor from Chapter 3, verifying connectivity with a 5-second timeout.

---

#### Part B: Server Configuration with Production Timeouts

Add the server startup logic to complete `cmd/api/main.go`:

```go
	// Instantiate document store and API application container
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

**Line-by-line explanation:**
- `api.New(cfg, docStore, logger)`: Injects all dependencies explicitly into the `Application` struct.
- `ReadTimeout: 10 * time.Second`: Maximum duration for reading the entire incoming request, including headers and body. Mitigates Slowloris denial-of-service attacks.
- `WriteTimeout: 30 * time.Second`: Maximum duration before timing out writes of the response.
- `IdleTimeout: time.Minute`: Maximum amount of time to wait for the next request when keep-alives are enabled.
- `srv.ListenAndServe()`: Starts the TCP listener and begins serving requests.

---

## 4. Terminal Commands & Infrastructure Execution

### Step 4.1: Update Go Module Dependencies
Ensure all dependencies are resolved in your `go.mod` file:

```bash
go mod tidy
```

### Step 4.2: Verify Infrastructure Readiness
Confirm that your local Docker infrastructure from Chapter 2 is running and healthy:

```bash
make ps
```

Verify that `docpub-postgres` has status `Up (healthy)`. If not, start the containers:

```bash
make up
```

---

## 5. Verification Playbook

Follow these checks to verify the API Service from your Arch Linux terminal.

### Check 1: Start the API Service
Run the API service from the repository root:

```bash
go run ./cmd/api
```

**Expected output (structured JSON logs in terminal):**
```json
{"time":"2026-10-23T15:20:01.123Z","level":"INFO","msg":"database connection pool established"}
{"time":"2026-10-23T15:20:01.124Z","level":"INFO","msg":"starting server","addr":":8080","env":"development"}
```

---

### Check 2: Verify the Healthcheck Endpoint
In a second terminal window, verify the server status using `curl`:

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

### Check 3: Verify Input Validation Rejection (`422 Unprocessable Entity`)
Submit an invalid payload with an empty title and missing source content:

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

### Check 4: Submit a Valid Markdown Document (`202 Accepted`)
Submit a valid document for asynchronous compilation:

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

### Check 5: Query Document Status (`200 OK`)
Fetch the persisted document state using the copied UUID:

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

### Check 6: Query Non-Existent Document UUID (`404 Not Found`)
Submit a query for a valid UUID that does not exist in PostgreSQL:

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

### Check 7: Query Malformed UUID Path (`404 Not Found`)
Submit a query with an invalid UUID string format:

```bash
curl -i http://localhost:8080/v1/documents/not-a-valid-uuid
```

**Expected output:**
```http
HTTP/1.1 404 Not Found
Content-Type: application/json
Date: Fri, 23 Oct 2026 15:24:30 GMT
Content-Length: 53

{"error":"the requested resource could not be found"}
```

---

### Check 8: Inspect Real-Time Structured Logs
Switch back to your server terminal. Notice the clean, machine-readable JSON log output produced by `logRequest`:

```json
{"time":"2026-10-23T15:20:10.001Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/healthz","status":200,"duration_ms":0}
{"time":"2026-10-23T15:21:00.042Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":422,"duration_ms":1}
{"time":"2026-10-23T15:22:15.895Z","level":"INFO","msg":"request processed","method":"POST","uri":"/v1/documents","status":202,"duration_ms":14}
{"time":"2026-10-23T15:23:00.120Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/documents/e3b0c442-98fc-1c14-9afb-4c7b94420000","status":200,"duration_ms":2}
{"time":"2026-10-23T15:24:00.088Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/documents/00000000-0000-0000-0000-000000000000","status":404,"duration_ms":1}
{"time":"2026-10-23T15:24:30.012Z","level":"INFO","msg":"request processed","method":"GET","uri":"/v1/documents/not-a-valid-uuid","status":404,"duration_ms":0}
```

---

## 6. Common Gotchas & Troubleshooting

1. **Port Already Bound (`listen tcp :8080: bind: address already in use`):**
   - *Symptom*: Server exits immediately on startup with `bind: address already in use`.
   - *Cause*: Another process or earlier server instance is already running on port 8080.
   - *Fix (Arch Linux)*: Locate the process occupying port 8080 with `ss` and terminate it:
     ```bash
     ss -tulpn | grep :8080
     kill -9 <PID>
     ```
     Or start the server on a different port:
     ```bash
     go run ./cmd/api -port=8081
     ```

2. **Unknown JSON Fields Rejection (`400 Bad Request`):**
   - *Symptom*: Sending `{ "title": "Test", "extra_field": "bad" }` returns `400 Bad Request`.
   - *Cause*: `readJSON()` deliberately calls `dec.DisallowUnknownFields()` to protect your domain models from unrecognized or malicious payload properties.
   - *Fix*: Ensure clients send only expected fields (`title`, `source_content`).

3. **Trailing Slashes in Go 1.22+ ServeMux:**
   - *Symptom*: Calling `POST /v1/documents/` redirects to `POST /v1/documents` with a `301 Moved Permanently`.
   - *Cause*: In Go's `http.ServeMux`, paths without trailing slashes (`/v1/documents`) and paths with trailing slashes (`/v1/documents/`) represent distinct subtrees.
   - *Fix*: Always omit trailing slashes for specific REST endpoints.

4. **Missing Production Server Timeouts:**
   - *Symptom*: High memory usage under slow or aborted client connections.
   - *Cause*: Go's default `http.ListenAndServe(":8080", mux)` leaves `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` unconfigured, allowing broken client connections to stay open indefinitely.
   - *Fix*: Always instantiate `&http.Server{}` explicitly with defensive timeouts, as demonstrated in `cmd/api/main.go`.

---

## 7. Summary & Next Steps

### Key Takeaways
- Go 1.22+ native routing eliminates third-party routing dependencies, supporting method prefixes and URL path wildcards directly.
- The `Application` struct enables clean, testable dependency injection without global state or race conditions.
- Structured logging via `log/slog` outputs JSON logs formatted for production observability platforms.
- Defensive request body parsing (`http.MaxBytesReader`, `DisallowUnknownFields`) protects memory from denial-of-service payloads.
- Returning `202 Accepted` decouples synchronous ingestion from CPU-intensive Markdown compilation.

### Self-Check Questions Before Advancing
1. Why does `createDocumentHandler` return `202 Accepted` instead of `201 Created`?
2. What happens to a Go HTTP server when an unrecovered panic occurs inside a handler goroutine?
3. How does `http.MaxBytesReader` prevent memory exhaustion attacks?
4. Why do we avoid using global variables for our database pool and logger?

---

In **[Chapter 6: Publishing Events to NATS JetStream](06-publishing-events-to-nats-jetstream.md)**, we will connect the API Service to NATS JetStream. After inserting the document into PostgreSQL, the API Service will publish a persistent `docs.submitted` event to the `DOCS` stream, initiating the asynchronous publishing workflow.
