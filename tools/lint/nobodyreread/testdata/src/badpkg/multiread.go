// Fixture: every double consumption of *http.Request.Body in the same
// handler must be flagged. Diagnostic-expectation comments use the
// analysistest convention.
package badpkg

import (
	"encoding/json"
	"io"
	"net/http"
)

// Two io.ReadAll(r.Body) in the same handler — both reads should fire.
func handlerDoubleReadAll(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body) // want `r.Body is read more than once`
	_, _ = io.ReadAll(r.Body) // want `r.Body is read more than once`
	_ = w
}

// io.ReadAll then json.NewDecoder — also a double consumption.
func handlerReadAllPlusDecode(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body) // want `r.Body is read more than once`
	var v map[string]any
	_ = json.NewDecoder(r.Body).Decode(&v) // want `r.Body is read more than once`
	_ = w
}

// Direct r.Body.Read calls — both fire.
func handlerDirectRead(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	_, _ = r.Body.Read(buf) // want `r.Body is read more than once`
	_, _ = r.Body.Read(buf) // want `r.Body is read more than once`
	_ = w
}
