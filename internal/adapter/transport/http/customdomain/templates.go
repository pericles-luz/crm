package customdomain

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"sync"
)

// templateFS holds the embedded *.html sources. The handler parses them
// once into one *template.Template and reuses it for every render.
//
//go:embed templates/*.html
var templateFS embed.FS

var (
	tmplOnce sync.Once
	tmpl     *template.Template
	tmplErr  error
)

// loadTemplates parses the embedded templates with the boundary's
// custom funcs. Called once on the first render — the package init has
// to stay side-effect free for the test binary.
func loadTemplates() (*template.Template, error) {
	tmplOnce.Do(func() {
		t := template.New("base").Funcs(template.FuncMap{
			// no custom funcs yet; placeholder so the package compiles
			// when we add one.
		})
		t, err := t.ParseFS(templateFS, "templates/*.html")
		if err != nil {
			tmplErr = fmt.Errorf("customdomain: parse templates: %w", err)
			return
		}
		tmpl = t
	})
	return tmpl, tmplErr
}

// renderTemplate executes name into w. Errors are returned to the
// caller; the handler is responsible for crafting an error response.
func renderTemplate(w io.Writer, name string, data any) error {
	t, err := loadTemplates()
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, name, data)
}
