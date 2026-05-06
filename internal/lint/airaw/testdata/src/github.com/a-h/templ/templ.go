// Package templ is a minimal stand-in for github.com/a-h/templ used by the
// airaw analyzer's analysistest fixtures. It exposes the same API surface
// (Raw, UnsafeHTML, ...) so the analyzer's callee detection runs against
// realistic import paths and function names.
package templ

import (
	"context"
	"io"
)

type Component interface {
	Render(ctx context.Context, w io.Writer) error
}

type ComponentFunc func(ctx context.Context, w io.Writer) error

func (cf ComponentFunc) Render(ctx context.Context, w io.Writer) error {
	return cf(ctx, w)
}

func Raw[T ~string](html T, errs ...error) Component {
	return ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, string(html))
		return err
	})
}

func UnsafeHTML(s string) Component {
	return ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}
