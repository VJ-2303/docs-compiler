# Chapter 7: MinIO Storage and Markdown Engine

Welcome to Chapter 7. In Phase 2, we completed the API Service: ingesting raw Markdown submissions via `POST /v1/documents`, validating payloads, persisting `PENDING` records in PostgreSQL, and dispatching persistent `docs.submitted` events to NATS JetStream in ~15ms.

Now, we cross the architectural boundary into **Phase 3: The Compiler Worker & MinIO Storage**.

In this chapter, we will build the core processing and storage engines for our autonomous Compiler Service:
1. **The S3 Storage Client** (`internal/storage/s3.go`): Connecting to MinIO using the official `minio-go/v7` driver, ensuring target buckets exist, and streaming rendered artifacts into cloud object storage without temporary disk files.
2. **The Markdown Compilation Engine** (`internal/compiler/engine.go`): Using `goldmark`—Go's premier CommonMark parser—to compile raw Markdown into sanitized HTML and inject it into a self-contained, responsive HTML template with embedded typography and code styling.

By the end of this chapter, you will have a standalone, test-verified compilation engine and S3 storage layer ready to be wired into our background queue consumer in Chapter 8.

---

## 1. Objectives & Overview

In this chapter, you will:
- Understand why cloud object storage (S3 / MinIO) is mandatory for distributed microservices over local container filesystems.
- Learn why streaming in-memory buffers (`io.Reader`) prevents disk wear, file leaks, and race conditions.
- Implement an idempotent S3 client constructor that guarantees bucket existence at boot time.
- Learn the CommonMark specification and why `goldmark` is the industry standard parser in Go.
- Build a responsive, standalone HTML template with modern typography and syntax styling.
- Prevent Cross-Site Scripting (XSS) by properly configuring HTML escaping in the compiler.
- Write isolated automated unit and integration tests to verify rendering and S3 uploads.

### The Compilation and Storage Pipeline

```
[ Raw Markdown Input ]
          │
          ▼
┌──────────────────────────────────────────────┐
│ goldmark CommonMark Parser                   │
│ - Parses headings, lists, tables, code blocks│
│ - Sanitizes and escapes unsafe HTML tags     │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│ html/template Engine                         │
│ - Injects document title                     │
│ - Wraps in responsive CSS stylesheet         │
│ - Produces self-contained standalone HTML    │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│ In-Memory bytes.Buffer                       │
│ - Zero disk I/O, zero temporary files        │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│ MinIO S3 Client (minio-go/v7)                │
│ - PutObject(bucket, key, reader, size)       │
│ - Content-Type: text/html; charset=utf-8     │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│ MinIO Object Storage (Docker)                │
│ Bucket: docs-rendered                        │
│ Key: artifacts/{document_id}.html            │
└──────────────────────────────────────────────┘
```

---

## 2. Conceptual Deep Dive (The "Why")

### Why Object Storage (S3) Over Local File Systems?
In monolithic applications, developers frequently save uploaded or generated files directly to the local disk:

```go
// ANTI-PATTERN in Distributed Systems: Saving to local disk
os.WriteFile(fmt.Sprintf("/var/www/uploads/%s.html", docID), htmlBytes, 0644)
```

In microservices and containerized environments, this approach fails catastrophically for three reasons:

1. **Containers are Ephemeral**: When a container crashes, restarts, or scales to a new version, its writable layer is destroyed. Any files written to the container's local disk disappear permanently.
2. **Worker Scaling & Shared Access**: When we scale the Compiler Service to 5 parallel instances, each worker runs in its own isolated container. If Worker A renders a file to its local `/tmp` folder, Worker B and the API Service cannot see or serve that file. Object storage provides a single, centralized, durable repository accessible across all instances.
3. **The Universal S3 Standard**: MinIO implements the Amazon S3 REST API protocol. By coding against MinIO using `minio-go/v7`, your application is 100% cloud-portable. Migrating from local Docker to AWS S3, Cloudflare R2, Google Cloud Storage, or Backblaze B2 requires changing only two environment variables (`S3_ENDPOINT` and credentials)—zero code modifications.

---

### Why In-Memory Buffer Streaming (`io.Reader`)?
A common rookie mistake when generating files is writing them to temporary disk files before uploading:

```
Generate HTML ──► Write /tmp/out.html ──► Read /tmp/out.html ──► Upload S3 ──► Delete /tmp/out.html
```

This creates severe operational liabilities:
- **Disk I/O Latency**: Writing to NVMe or hard disk adds needless latency to every job.
- **Leaked Temporary Files**: If the process crashes or panics before the cleanup step runs, orphaned files fill the disk until the operating system halts.
- **Race Conditions**: Concurrent worker goroutines can accidentally collide if temporary filenames overlap.

Instead, we stream directly from RAM:
```
Generate HTML ──► in-memory []byte ──► bytes.NewReader ──► MinIO PutObject
```
Our compilation engine renders HTML into a `bytes.Buffer`. We hand that buffer directly to MinIO's `PutObject` via `bytes.NewReader(buf.Bytes())`. Zero disk touches, zero temporary files, and zero disk cleanup required.

---

### Why `goldmark` for Markdown Compilation?
Markdown looks simple on the surface, but it has dozens of ambiguous edge cases (nested lists, code blocks inside blockquotes, raw HTML tags, trailing spaces). In the Go ecosystem, older parsers like `blackfriday` or `russross/blackfriday/v2` are either unmaintained or deviate from official standards.

We use **`goldmark`** (`github.com/yuin/goldmark`) because:
- **100% CommonMark Compliant**: Guarantees predictable, standardized parsing matching GitHub and modern documentation platforms.
- **Fast and Zero-Allocation Optimized**: Written in idiomatic Go with memory-pooling techniques that rival C-based parsers.
- **Extensible Architecture**: Natively supports GitHub Flavored Markdown (GFM) extensions, including tables, task lists, autolinks, and strikethroughs.
- **Built-in Security**: Offers fine-grained control over raw HTML rendering to protect against malicious script injections.

---

### Why Standalone HTML Documents?
When a Markdown parser compiles text like `# Hello World`, it produces an HTML fragment:
```html
<h1>Hello World</h1>
```

If a user downloads or opens this fragment directly in a browser, it renders as an unstyled, raw webpage with default browser serif fonts, touching the edge of the viewport with zero margins or mobile responsiveness.

Our compilation engine wraps the compiled HTML fragment inside an elegant **Standalone HTML Template**:
- Declares `<!DOCTYPE html>`, UTF-8 character encoding, and responsive mobile viewport meta tags.
- Injects a clean, modern CSS stylesheet into the `<head>` using system sans-serif fonts, comfortable reading line-heights, GitHub-style table borders, and styled code blocks.
- The resulting `.html` file stored in MinIO is completely self-contained. It requires no external CDN stylesheets or JavaScript files and opens beautifully on any phone, tablet, or desktop browser.

---

## 3. Step-by-Step Implementation

We will organize our code across two focused packages:
- `internal/storage`: S3 client connection, bucket initialization, and artifact uploading.
- `internal/compiler`: HTML templating and Goldmark compilation engine.

```
internal/
├── compiler/
│   ├── engine.go        # Markdown compilation and template execution
│   └── template.go      # Embedded HTML skeleton and responsive CSS
└── storage/
    └── s3.go            # MinIO S3 client wrapper and PutObject streaming
```

To adhere to our 30–60 line rule, we build each component in bite-sized, digestible functional units.

---

### Step 3.1: S3 Client Configuration & Bucket Initialization

Create `internal/storage/s3.go`. We will build this file in two focused parts.

#### Part A: Struct Definition and Client Constructor

```go
// internal/storage/s3.go
package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds runtime connection parameters for MinIO / S3 storage.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// Client wraps the MinIO S3 API client and targeted bucket name.
type Client struct {
	minioClient *minio.Client
	bucket      string
	logger      *slog.Logger
}

// New establishes an S3 client and idempotently ensures the destination bucket exists.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	// Initialize minio client with static credentials
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("initializing minio client: %w", err)
	}

	// Idempotently verify or create destination bucket
	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("checking bucket %q: %w", cfg.Bucket, err)
	}

	if !exists {
		err = mc.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{})
		if err != nil {
			return nil, fmt.Errorf("creating bucket %q: %w", cfg.Bucket, err)
		}
		logger.Info("created storage bucket", "bucket", cfg.Bucket)
	} else {
		logger.Info("storage bucket verified", "bucket", cfg.Bucket)
	}

	return &Client{
		minioClient: mc,
		bucket:      cfg.Bucket,
		logger:      logger,
	}, nil
}
```

**Line-by-line explanation:**
- `credentials.NewStaticV4(...)`: Configures AWS Signature Version 4 authentication using root credentials (`minioadmin`).
- `Secure: cfg.UseSSL`: Set to `false` for local Docker development (`http://localhost:9000`), or `true` when running with HTTPS in production.
- `mc.BucketExists(ctx, ...)`: Queries the S3 API to check if our bucket (`docs-rendered`) already exists.
- `mc.MakeBucket(...)`: If the bucket does not exist, creates it immediately. This guarantees that workers never fail with `NoSuchBucket` errors when saving documents.

---

### Step 3.2: In-Memory Artifact Streaming

Add the `UploadHTML` method to `internal/storage/s3.go`:

```go
// UploadHTML streams an in-memory HTML document into MinIO under the specified object key.
func (c *Client) UploadHTML(ctx context.Context, key string, content []byte) error {
	reader := bytes.NewReader(content)
	size := int64(len(content))

	opts := minio.PutObjectOptions{
		ContentType: "text/html; charset=utf-8",
	}

	info, err := c.minioClient.PutObject(ctx, c.bucket, key, reader, size, opts)
	if err != nil {
		return fmt.Errorf("uploading object %q to bucket %q: %w", key, c.bucket, err)
	}

	c.logger.Info("uploaded document artifact",
		"bucket", c.bucket,
		"key", key,
		"size_bytes", info.Size,
		"etag", info.ETag,
	)

	return nil
}

// Bucket returns the configured bucket name.
func (c *Client) Bucket() string {
	return c.bucket
}
```

**Line-by-line explanation:**
- `bytes.NewReader(content)`: Implements `io.Reader` and `io.Seeker` over the in-memory byte slice, streaming directly to the network socket without touching the local hard drive.
- `ContentType: "text/html; charset=utf-8"`: Critical metadata! Instructs web browsers fetching this file to render it immediately as styled HTML rather than downloading it as a generic octet-stream binary.
- `c.minioClient.PutObject(...)`: Executes standard S3 `PUT` request with payload size verification and MD5 checksum validation.

---

### Step 3.3: Designing the Standalone HTML Template

Create `internal/compiler/template.go`.

Here, we define our HTML skeleton and embedded CSS styling using Go's standard `html/template` package.

```go
// internal/compiler/template.go
package compiler

import (
	"html/template"
)

// documentTemplate defines the standalone HTML document wrapper with embedded CSS.
const documentTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{ .Title }}</title>
  <style>
    :root {
      --bg: #ffffff;
      --fg: #1f2328;
      --muted: #656d76;
      --border: #d0d7de;
      --code-bg: #f6f8fa;
      --accent: #0969da;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
      line-height: 1.6;
      color: var(--fg);
      background: var(--bg);
      max-width: 860px;
      margin: 0 auto;
      padding: 2rem 1.5rem;
    }
    h1, h2, h3 { border-bottom: 1px solid var(--border); padding-bottom: 0.3em; }
    code { background: var(--code-bg); padding: 0.2em 0.4em; border-radius: 6px; font-size: 85%; }
    pre { background: var(--code-bg); padding: 1rem; border-radius: 8px; overflow-x: auto; }
    pre code { background: none; padding: 0; font-size: 90%; }
    blockquote { border-left: 4px solid var(--border); margin: 0; padding-left: 1rem; color: var(--muted); }
    table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
    th, td { border: 1px solid var(--border); padding: 8px 12px; text-align: left; }
    th { background: var(--code-bg); }
    img { max-width: 100%; height: auto; }
    hr { border: 0; border-top: 1px solid var(--border); margin: 2rem 0; }
  </style>
</head>
<body>
  <article>
    {{ .Body }}
  </article>
</body>
</html>`

// parsedTemplate parses and compiles the raw template string.
func parseTemplate() (*template.Template, error) {
	return template.New("document").Parse(documentTemplate)
}
```

**Line-by-line explanation:**
- `{{ .Title }}`: Injected dynamically into `<title>` and escaped against XSS by Go's `html/template`.
- `{{ .Body }}`: Injects the compiled HTML body output by `goldmark`.
- Embedded CSS variables (`--bg`, `--fg`, `--border`): Modern, lightweight CSS styling mimicking GitHub Markdown styling without requiring external HTTP stylesheet requests.
- Mobile viewport: `<meta name="viewport" ...>` ensures proper scaling on smartphones and tablets.

---

### Step 3.4: The Goldmark Markdown Compiler Engine

Create `internal/compiler/engine.go`.

Here, we configure Goldmark with GitHub Flavored Markdown (GFM) extensions, parse the input text, measure rendering duration, and execute our template.

```go
// internal/compiler/engine.go
package compiler

import (
	"bytes"
	"fmt"
	"html/template"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// Result represents the compiled HTML document artifact and rendering statistics.
type Result struct {
	HTMLContent []byte
	DurationMS  int
}

// Engine encapsulates the Goldmark parser and compiled HTML template.
type Engine struct {
	md   goldmark.Markdown
	tmpl *template.Template
}

// New constructs an Engine with GFM extensions and strict HTML escaping.
func New() (*Engine, error) {
	tmpl, err := parseTemplate()
	if err != nil {
		return nil, fmt.Errorf("parsing html template: %w", err)
	}

	// Configure goldmark with GitHub Flavored Markdown (GFM) extensions
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,            // Tables, strikethrough, autolinks, task lists
			extension.Table,          // Explicit table support
			extension.TaskList,       // Checkbox task lists [x]
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(), // Adds slug IDs to headings (e.g. #intro -> id="intro")
		),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),     // Render newlines as <br>
			html.WithXHTML(),         // Strict self-closing tags
		),
	)

	return &Engine{
		md:   md,
		tmpl: tmpl,
	}, nil
}

// Compile parses CommonMark text into a standalone styled HTML document.
func (e *Engine) Compile(title, sourceContent string) (*Result, error) {
	start := time.Now()

	// 1. Convert CommonMark text to sanitized HTML fragment
	var bodyBuf bytes.Buffer
	err := e.md.Convert([]byte(sourceContent), &bodyBuf)
	if err != nil {
		return nil, fmt.Errorf("converting markdown to html: %w", err)
	}

	// 2. Wrap compiled HTML fragment inside our standalone template
	data := struct {
		Title string
		Body  template.HTML // Mark as safe HTML since goldmark produced it
	}{
		Title: title,
		Body:  template.HTML(bodyBuf.String()),
	}

	var outputBuf bytes.Buffer
	err = e.tmpl.Execute(&outputBuf, data)
	if err != nil {
		return nil, fmt.Errorf("executing html template: %w", err)
	}

	duration := time.Since(start).Milliseconds()

	return &Result{
		HTMLContent: outputBuf.Bytes(),
		DurationMS:  int(duration),
	}, nil
}
```

**Line-by-line explanation:**
- `extension.GFM`: Enables GitHub Flavored Markdown extensions (tables, autolinks, task lists, and strikethroughs).
- `parser.WithAutoHeadingID()`: Automatically turns `# Architecture Overview` into `<h1 id="architecture-overview">`, enabling anchor links in documents.
- `template.HTML(...)`: By default, Go's `html/template` escapes all strings. Converting `bodyBuf.String()` to `template.HTML` informs Go that this string contains trusted HTML produced by Goldmark, rendering elements like `<h1>` and `<code>` instead of escaping them as `&lt;h1&gt;`.
- `time.Since(start).Milliseconds()`: Captures rendering latency in milliseconds. This metric will be recorded in PostgreSQL in Chapter 8.

---

## 4. Terminal Commands & Infrastructure Execution

### Step 4.1: Download Required Go Packages
Install `minio-go/v7` and `goldmark` into your project:

```bash
go get github.com/minio/minio-go/v7
go get github.com/yuin/goldmark
go mod tidy
```

### Step 4.2: Verify MinIO Infrastructure Status
Confirm that your local Docker infrastructure from Chapter 2 is active and healthy:

```bash
make ps
```

Verify that `docpub-minio` shows `Up (healthy)`. If not, start the containers:

```bash
make up
```

---

## 5. Verification Playbook

To adhere to our senior development discipline, we verify our engine and S3 client with focused, automated unit and integration tests.

### Check 1: Verify MinIO S3 API Reachability
Query the MinIO live health endpoint from your Arch Linux terminal:

```bash
curl -i http://localhost:9000/minio/health/live
```

**Terminal Flag Explanations:**
- `-i`: Includes HTTP response headers in output.

**Expected output:**
```http
HTTP/1.1 200 OK
Server: MinIO
...
```

---

### Check 2: Write an Automated Unit Test for the Markdown Engine
Create `internal/compiler/engine_test.go` to verify compilation, GFM extensions, and styling:

```go
// internal/compiler/engine_test.go
package compiler_test

import (
	"strings"
	"testing"

	"github.com/vj/docx/internal/compiler"
)

func TestEngine_Compile(t *testing.T) {
	engine, err := compiler.New()
	if err != nil {
		t.Fatalf("failed to initialize compiler engine: %v", err)
	}

	title := "Test Document"
	markdown := `
# Introduction

This is a **bold** paragraph with a [link](https://example.com).

| Feature | Status |
| :--- | :--- |
| CommonMark | Supported |
| Tables | Enabled |

` + "```go\nfunc main() {}\n```"

	result, err := engine.Compile(title, markdown)
	if err != nil {
		t.Fatalf("Compile() returned unexpected error: %v", err)
	}

	htmlStr := string(result.HTMLContent)

	// Verify title injection
	if !strings.Contains(htmlStr, "<title>Test Document</title>") {
		t.Errorf("expected title in html, got: %s", htmlStr)
	}

	// Verify markdown heading compilation
	if !strings.Contains(htmlStr, `<h1 id="introduction">Introduction</h1>`) {
		t.Errorf("expected compiled h1 tag, got: %s", htmlStr)
	}

	// Verify GFM table compilation
	if !strings.Contains(htmlStr, "<table>") || !strings.Contains(htmlStr, "<th>Feature</th>") {
		t.Errorf("expected compiled table element, got: %s", htmlStr)
	}

	// Verify code block compilation
	if !strings.Contains(htmlStr, `<code class="language-go">`) {
		t.Errorf("expected compiled code block, got: %s", htmlStr)
	}

	if result.DurationMS < 0 {
		t.Errorf("invalid render duration: %d", result.DurationMS)
	}
}
```

Run the unit test:
```bash
go test -v ./internal/compiler/...
```

**Expected output:**
```
=== RUN   TestEngine_Compile
--- PASS: TestEngine_Compile (0.00s)
PASS
ok      github.com/vj/docx/internal/compiler    0.012s
```

---

### Check 3: Write an Integration Test for MinIO S3 Storage
Create `internal/storage/s3_test.go` to test bucket auto-creation and artifact streaming against our local Docker container:

```go
// internal/storage/s3_test.go
package storage_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/vj/docx/internal/storage"
)

func TestStorage_UploadHTML(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := storage.Config{
		Endpoint:  "localhost:9000",
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Bucket:    "docs-rendered",
		UseSSL:    false,
	}

	client, err := storage.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("failed to initialize s3 storage client: %v", err)
	}

	key := "artifacts/test-verify.html"
	content := []byte("<!DOCTYPE html><html><body><h1>Verification Successful</h1></body></html>")

	err = client.UploadHTML(ctx, key, content)
	if err != nil {
		t.Fatalf("UploadHTML failed: %v", err)
	}
}
```

Run the storage integration test:
```bash
go test -v ./internal/storage/...
```

**Expected output:**
```
=== RUN   TestStorage_UploadHTML
{"time":"2026-10-23T18:00:00.000Z","level":"INFO","msg":"storage bucket verified","bucket":"docs-rendered"}
{"time":"2026-10-23T18:00:00.010Z","level":"INFO","msg":"uploaded document artifact","bucket":"docs-rendered","key":"artifacts/test-verify.html","size_bytes":72,"etag":"..."}
--- PASS: TestStorage_UploadHTML (0.02s)
PASS
ok      github.com/vj/docx/internal/storage     0.035s
```

---

### Check 4: Inspect Uploaded Artifact in MinIO Web Console
1. Open your web browser and navigate to `http://localhost:9001`.
2. Log in with root credentials:
   - **Username**: `minioadmin`
   - **Password**: `minioadmin`
3. Click on **Buckets** in the left navigation sidebar.
4. Verify that the `docs-rendered` bucket exists.
5. Click on `docs-rendered` and navigate into the `artifacts/` folder.
6. Notice `test-verify.html` is listed with Content-Type `text/html; charset=utf-8`!

---

## 6. Common Gotchas & Troubleshooting

1. **`minio: Get "https://localhost:9000": http: server gave HTTP response to HTTPS client`:**
   - *Symptom*: S3 client constructor fails immediately with TLS handshake error.
   - *Cause*: `UseSSL` is set to `true`, but local MinIO in Docker runs plain HTTP without TLS certificates.
   - *Fix*: Ensure `UseSSL: false` is configured for local development.

2. **Cross-Site Scripting (XSS) via Raw HTML in Markdown:**
   - *Symptom*: Submitting `<script>alert('xss')</script>` in Markdown executes JavaScript when the HTML file is opened.
   - *Cause*: By default, `goldmark` escapes raw HTML. However, if `html.WithUnsafe()` is accidentally enabled, raw HTML tags pass through unescaped.
   - *Fix*: Never pass `html.WithUnsafe()` to Goldmark in user-facing ingestion pipelines.

3. **Missing `Content-Type: text/html` on S3 Objects:**
   - *Symptom*: Opening the S3 object URL in a browser downloads a `.bin` or `.html` file instead of rendering the page.
   - *Cause*: `PutObjectOptions` did not set `ContentType`, causing MinIO to default to `application/octet-stream`.
   - *Fix*: Always set `opts.ContentType = "text/html; charset=utf-8"` in `UploadHTML()`.

---

## 7. Summary & Next Steps

### Key Takeaways
- Cloud object storage (S3 / MinIO) decouples persistent file artifacts from ephemeral worker containers.
- Streaming directly from memory (`bytes.NewReader`) eliminates disk I/O latency, temporary file leaks, and race conditions.
- Goldmark provides 100% CommonMark compliance and fast, memory-efficient Markdown rendering in pure Go.
- Standalone HTML templates ensure compiled Markdown files are self-contained with responsive styling ready for browser display.

### Self-Check Questions Before Advancing
1. Why is storing generated files on a container's local filesystem an anti-pattern in distributed architectures?
2. What is the operational benefit of handing `bytes.NewReader(buf)` to MinIO instead of writing to `/tmp`?
3. How does `extension.GFM` enhance Goldmark beyond baseline CommonMark?
4. Why must we explicitly set `ContentType: "text/html; charset=utf-8"` when uploading to S3?

---

In **[Chapter 8: Queue Consumer and Event Feedback Loop](08-queue-consumer-and-event-feedback.md)**, we will build the Compiler Service background worker in `cmd/compiler/main.go`. We will implement a durable NATS JetStream pull consumer that pulls `docs.submitted` events, compiles the Markdown using our engine, uploads the artifact to MinIO, and publishes `docs.compiled` back to NATS to update PostgreSQL.
