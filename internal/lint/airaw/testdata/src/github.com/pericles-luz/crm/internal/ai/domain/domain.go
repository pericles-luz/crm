// Package domain is a minimal stand-in for the real internal/ai/domain
// package, used only by the airaw analyzer's analysistest fixtures.
package domain

type Suggestion struct {
	Kind string
	Body string
}

type AISummary struct {
	Summary     string
	Suggestions []Suggestion
}

// SummarizeText is a top-level helper in the AI package; calling it directly
// taints the result. The analyzer must flag templ.Raw(domain.SummarizeText()).
func SummarizeText() string {
	return ""
}

// SummarizeWithError mirrors a typical signature so we can exercise the
// *ssa.Extract branch without the value passing through any struct field.
func SummarizeWithError() (string, error) {
	return "", nil
}
