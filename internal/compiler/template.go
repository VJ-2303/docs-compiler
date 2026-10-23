package compiler

import (
	"html/template"
)

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

func parseTemplate() (*template.Template, error) {
	return template.New("document").Parse(documentTemplate)
}
