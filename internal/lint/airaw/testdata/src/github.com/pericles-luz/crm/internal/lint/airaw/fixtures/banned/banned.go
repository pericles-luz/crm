// Package banned exercises the airaw analyzer with the patterns it MUST flag.
package banned

import (
	"context"

	"github.com/a-h/templ"
	"github.com/pericles-luz/crm/internal/ai/domain"
	"github.com/pericles-luz/crm/internal/ai/port"
)

func RawDirectField(s domain.AISummary) templ.Component {
	return templ.Raw(s.Summary) // want `airaw: templ\.Raw called with AI-derived value`
}

func RawSuggestionBody(s domain.AISummary) templ.Component {
	first := s.Suggestions[0]
	return templ.Raw(first.Body) // want `airaw: templ\.Raw called with AI-derived value`
}

func RawSummarizer(ctx context.Context, p port.Summarizer) templ.Component {
	out, err := p.Summarize(ctx, "conv-123")
	if err != nil {
		return templ.Raw("") // safe — literal
	}
	return templ.Raw(out.Summary) // want `airaw: templ\.Raw called with AI-derived value`
}

func RawConcatenated(s domain.AISummary) templ.Component {
	combined := "<b>" + s.Summary + "</b>"
	return templ.Raw(combined) // want `airaw: templ\.Raw called with AI-derived value`
}

func UnsafeHTMLOnAI(s domain.AISummary) templ.Component {
	return templ.UnsafeHTML(s.Summary) // want `airaw: templ\.UnsafeHTML called with AI-derived value`
}

func RawByParameterType(s domain.Suggestion) templ.Component {
	return templ.Raw(s.Body) // want `airaw: templ\.Raw called with AI-derived value`
}

// MyString is a string-typed alias so we can exercise the ssa.Convert path:
// the conversion creates a new SSA value, but the underlying source still
// resolves to AISummary.Summary.
type MyString string

func RawConvertedTyped(s domain.AISummary) templ.Component {
	return templ.Raw(MyString(s.Summary)) // want `airaw: templ\.Raw called with AI-derived value`
}

func RawSummaryPointer(p *domain.AISummary) templ.Component {
	return templ.Raw(p.Summary) // want `airaw: templ\.Raw called with AI-derived value`
}

// RawNestedSliceIndex exercises ssa.IndexAddr / ssa.Index when the AI-typed
// value lives inside a slice.
func RawNestedSliceIndex(items []domain.Suggestion) templ.Component {
	return templ.Raw(items[0].Body) // want `airaw: templ\.Raw called with AI-derived value`
}

// RawPhi forces an ssa.Phi to merge two AI-tainted incoming edges.
func RawPhi(a domain.AISummary, b bool) templ.Component {
	x := a.Summary
	if b {
		x = a.Suggestions[0].Body
	}
	return templ.Raw(x) // want `airaw: templ\.Raw called with AI-derived value`
}

// RawIfaceInvokeSummarizer ensures the *ssa.Call invoke branch lights up by
// dispatching through a port.Summarizer interface variable that the analyzer
// must recognize as the Summarize method even without a static callee.
func RawIfaceInvokeSummarizer(ctx context.Context, p port.Summarizer) templ.Component {
	out, _ := p.Summarize(ctx, "x")
	body := out.Summary
	return templ.Raw(body) // want `airaw: templ\.Raw called with AI-derived value`
}

// RawDomainCallReturn exercises the *ssa.Call branch with a static AI callee:
// the return value carries no AI-typed wrapper, so the only signal the
// analyzer has is the callee's package path.
func RawDomainCallReturn() templ.Component {
	return templ.Raw(domain.SummarizeText()) // want `airaw: templ\.Raw called with AI-derived value`
}

// RawDomainExtract exercises the *ssa.Extract pass-through: the value is the
// first return of an AI-package call, with no struct field in the chain.
func RawDomainExtract() templ.Component {
	s, _ := domain.SummarizeWithError()
	return templ.Raw(s) // want `airaw: templ\.Raw called with AI-derived value`
}
