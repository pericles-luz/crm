// Package allowed exercises the airaw analyzer with patterns it MUST NOT flag.
package allowed

import (
	"github.com/a-h/templ"
)

// Allowed: literal HTML the developer authored on the spot. Not AI-derived.
func RawLiteral() templ.Component {
	return templ.Raw("<strong>Hello</strong>")
}

// Allowed: a string built from non-AI sources.
func RawComputed(name string) templ.Component {
	greeting := "<p>Hi " + name + "</p>"
	return templ.Raw(greeting)
}

// Allowed: a struct field on a non-AI type, even if the struct is named in a
// way that looks AI-adjacent, must NOT trigger the analyzer.
type Banner struct {
	HTML string
}

func RawNonAIStructField(b Banner) templ.Component {
	return templ.Raw(b.HTML)
}
