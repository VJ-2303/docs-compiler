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

	if !strings.Contains(htmlStr, "<title>Test Document</title>") {
		t.Errorf("expected title in html, got: %s", htmlStr)
	}
	if !strings.Contains(htmlStr, `<h1 id="introduction">Introduction</h1>`) {
		t.Errorf("expected compiled h1 tag, got: %s", htmlStr)
	}
	if !strings.Contains(htmlStr, "<table>") || !strings.Contains(htmlStr, "Feature</th>") {
		t.Errorf("expected compiled table element, got: %s", htmlStr)
	}
	if !strings.Contains(htmlStr, `<code class="language-go">`) {
		t.Errorf("expected compiled code block, got: %s", htmlStr)
	}
	if result.DurationMS < 0 {
		t.Errorf("invalid render duration: %d", result.DurationMS)
	}
}
