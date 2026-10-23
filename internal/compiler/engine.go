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

type Result struct {
	HTMLContent []byte
	DurationMS  int
}

type Engine struct {
	md   goldmark.Markdown
	tmpl *template.Template
}

func New() (*Engine, error) {
	tmpl, err := parseTemplate()
	if err != nil {
		return nil, fmt.Errorf("parsing html template: %w", err)
	}

	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Table,
			extension.TaskList,
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
			html.WithXHTML(),
		),
	)
	return &Engine{
		md:   md,
		tmpl: tmpl,
	}, nil
}

func (e *Engine) Compile(title, sourceContent string) (*Result, error) {
	start := time.Now()

	var bodyBuf bytes.Buffer
	err := e.md.Convert([]byte(sourceContent), &bodyBuf)
	if err != nil {
		return nil, fmt.Errorf("converting markdown to html: %w", err)
	}

	data := struct {
		Title string
		Body  template.HTML
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
