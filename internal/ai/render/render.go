// Package render produces sanitized HTML from untrusted, model-derived text.
//
// The exported SafeText function is the only blessed renderer for AI output in
// templ files. It accepts a strict subset of CommonMark and emits HTML that
// cannot escape the surrounding template context — see ADR 0077 §3.5
// (SIN-62225) and the F29 deliverables in SIN-62237.
package render

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"strings"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// md is configured at package init with no extensions and no auto-link
// (Linkify) so we never get HTML inserted from URL detection. Headings, raw
// HTML blocks, and images are still parsed by the CommonMark parser, but the
// custom renderer below refuses to emit them.
var md = goldmark.New(
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
	),
)

// SafeText returns a templ.Component that renders s as sanitized HTML.
//
// The accepted Markdown subset is intentionally narrow:
//
//   - paragraphs and hard / soft line breaks
//   - **bold** and *italic*
//   - inline `code` and fenced code blocks (language tag is dropped)
//   - ordered and unordered lists, including nested lists
//   - link text and URL rendered as plain text (NEVER as <a href>)
//
// Everything outside that subset is dropped or escaped. Raw HTML, images,
// auto-detected URLs, scripts, style blocks, and any "on*" attributes cannot
// reach the output regardless of the input.
func SafeText(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		return Render(w, s)
	})
}

// Render writes the sanitized HTML form of src to w. It is exported so the
// render package can be exercised by fuzz harnesses without going through the
// templ.Component facade.
func Render(w io.Writer, src string) error {
	source := []byte(src)
	root := md.Parser().Parse(text.NewReader(source))
	rw := &renderer{w: w, source: source}
	return ast.Walk(root, rw.visit)
}

type renderer struct {
	w      io.Writer
	source []byte
	listOL []bool
}

func (r *renderer) visit(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node := n.(type) {

	case *ast.Document:
		return ast.WalkContinue, nil

	case *ast.Paragraph:
		if entering {
			if _, err := io.WriteString(r.w, "<p>"); err != nil {
				return ast.WalkStop, err
			}
		} else {
			if _, err := io.WriteString(r.w, "</p>\n"); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil

	case *ast.TextBlock:
		return ast.WalkContinue, nil

	case *ast.Heading:
		// Headings are NOT in the allowed subset.
		return ast.WalkSkipChildren, nil

	case *ast.Emphasis:
		tag := "em"
		if node.Level == 2 {
			tag = "strong"
		}
		if entering {
			if _, err := fmt.Fprintf(r.w, "<%s>", tag); err != nil {
				return ast.WalkStop, err
			}
		} else {
			if _, err := fmt.Fprintf(r.w, "</%s>", tag); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil

	case *ast.Link:
		// Render link as plain text: "label (url)" with both pieces escaped.
		// We never emit <a href>.
		if !entering {
			return ast.WalkContinue, nil
		}
		var label bytes.Buffer
		collectText(&label, node, r.source)
		dest := string(node.Destination)
		switch {
		case label.Len() == 0:
			if _, err := io.WriteString(r.w, html.EscapeString(dest)); err != nil {
				return ast.WalkStop, err
			}
		case dest == "" || strings.EqualFold(label.String(), dest):
			if _, err := io.WriteString(r.w, html.EscapeString(label.String())); err != nil {
				return ast.WalkStop, err
			}
		default:
			if _, err := fmt.Fprintf(r.w, "%s (%s)",
				html.EscapeString(label.String()),
				html.EscapeString(dest)); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.AutoLink:
		// Plain text only. Linkify-style auto-links cannot become <a href>.
		if !entering {
			return ast.WalkContinue, nil
		}
		dest := string(node.URL(r.source))
		if _, err := io.WriteString(r.w, html.EscapeString(dest)); err != nil {
			return ast.WalkStop, err
		}
		return ast.WalkContinue, nil

	case *ast.Image:
		return ast.WalkSkipChildren, nil

	case *ast.RawHTML:
		// F29 trap: raw inline HTML in the input is silently discarded.
		return ast.WalkSkipChildren, nil

	case *ast.HTMLBlock:
		return ast.WalkSkipChildren, nil

	case *ast.CodeSpan:
		if entering {
			if _, err := io.WriteString(r.w, "<code>"); err != nil {
				return ast.WalkStop, err
			}
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					if _, err := io.WriteString(r.w, html.EscapeString(string(t.Segment.Value(r.source)))); err != nil {
						return ast.WalkStop, err
					}
				}
			}
			if _, err := io.WriteString(r.w, "</code>"); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.FencedCodeBlock:
		if entering {
			if _, err := io.WriteString(r.w, "<pre><code>"); err != nil {
				return ast.WalkStop, err
			}
			r.writeCodeBlockLines(node)
			if _, err := io.WriteString(r.w, "</code></pre>\n"); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.CodeBlock:
		if entering {
			if _, err := io.WriteString(r.w, "<pre><code>"); err != nil {
				return ast.WalkStop, err
			}
			r.writeCodeBlockLines(node)
			if _, err := io.WriteString(r.w, "</code></pre>\n"); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.List:
		tag := "ul"
		if node.IsOrdered() {
			tag = "ol"
		}
		if entering {
			r.listOL = append(r.listOL, node.IsOrdered())
			if _, err := fmt.Fprintf(r.w, "<%s>\n", tag); err != nil {
				return ast.WalkStop, err
			}
		} else {
			if len(r.listOL) > 0 {
				r.listOL = r.listOL[:len(r.listOL)-1]
			}
			if _, err := fmt.Fprintf(r.w, "</%s>\n", tag); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil

	case *ast.ListItem:
		if entering {
			if _, err := io.WriteString(r.w, "<li>"); err != nil {
				return ast.WalkStop, err
			}
		} else {
			if _, err := io.WriteString(r.w, "</li>\n"); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil

	case *ast.Text:
		if entering {
			seg := node.Segment.Value(r.source)
			if _, err := io.WriteString(r.w, html.EscapeString(string(seg))); err != nil {
				return ast.WalkStop, err
			}
			if node.HardLineBreak() {
				if _, err := io.WriteString(r.w, "<br>"); err != nil {
					return ast.WalkStop, err
				}
			} else if node.SoftLineBreak() {
				if _, err := io.WriteString(r.w, "\n"); err != nil {
					return ast.WalkStop, err
				}
			}
		}
		return ast.WalkContinue, nil

	case *ast.String:
		if entering {
			if _, err := io.WriteString(r.w, html.EscapeString(string(node.Value))); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil

	case *ast.ThematicBreak:
		return ast.WalkSkipChildren, nil

	case *ast.Blockquote:
		// Drop the wrapper but keep walking inner text (already escaped).
		return ast.WalkContinue, nil

	default:
		return ast.WalkContinue, nil
	}
}

func (r *renderer) writeCodeBlockLines(n ast.Node) {
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		_, _ = io.WriteString(r.w, html.EscapeString(string(seg.Value(r.source))))
	}
}

// collectText walks the children of n and concatenates the literal text from
// any *ast.Text or *ast.String descendants. It is used to extract a link's
// label without recursing through the renderer (which would otherwise emit
// formatting markup inside the plain-text rendering of the link).
func collectText(buf *bytes.Buffer, n ast.Node, source []byte) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			buf.Write(v.Segment.Value(source))
		case *ast.String:
			buf.Write(v.Value)
		case *ast.CodeSpan:
			for cc := v.FirstChild(); cc != nil; cc = cc.NextSibling() {
				if t, ok := cc.(*ast.Text); ok {
					buf.Write(t.Segment.Value(source))
				}
			}
		default:
			collectText(buf, c, source)
		}
	}
}
