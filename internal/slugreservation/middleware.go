package slugreservation

import (
	"encoding/json"
	"errors"
	"net/http"
)

// SlugExtractor pulls the candidate slug from the request. The
// signup/rename handlers know how to find it (path value, JSON body,
// form field) so they pass an extractor closure. Returning ("", false)
// from the extractor means "no slug to check"; the middleware skips and
// passes the request through.
type SlugExtractor func(r *http.Request) (string, bool)

// reservedBody is the 409 JSON payload defined in the F46 spec.
type reservedBody struct {
	Slug          string `json:"slug"`
	ReservedUntil string `json:"reservedUntil"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
}

// invalidBody is the 400 JSON payload for malformed slugs caught at the
// middleware. Handlers that want richer validation can run their own
// pass first; we only short-circuit on hard format violations.
type invalidBody struct {
	Error string `json:"error"`
}

// RequireSlugAvailable returns middleware that checks the candidate
// slug against an active reservation. On hit it answers 409 with the
// payload `{slug, reservedUntil, reason, message}` and short-circuits
// the chain. On miss it calls next.
//
// Hard reasons we surface today: "reserved" (active reservation hit).
// More reasons can join the enum later (e.g. "blocklisted") without
// breaking the wire shape.
func RequireSlugAvailable(svc *Service, extract SlugExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug, ok := extract(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			err := svc.CheckAvailable(r.Context(), slug)
			if err == nil {
				next.ServeHTTP(w, r)
				return
			}
			if errors.Is(err, ErrInvalidSlug) {
				writeJSON(w, http.StatusBadRequest, invalidBody{Error: "invalid slug"})
				return
			}
			var reserved *ReservedError
			if errors.As(err, &reserved) {
				writeReservedConflict(w, reserved.Reservation)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
		})
	}
}

func writeReservedConflict(w http.ResponseWriter, res Reservation) {
	until := FormatExpiresAt(res.ExpiresAt)
	body := reservedBody{
		Slug:          res.Slug,
		ReservedUntil: until,
		Reason:        "reserved",
		Message:       "this slug is reserved until " + until,
	}
	writeJSON(w, http.StatusConflict, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// PathValueExtractor returns a SlugExtractor that reads the named path
// segment via Go 1.22 mux PathValue. Use for handlers like
// `POST /tenants/{slug}` or `PATCH /tenants/{slug}/slug`.
func PathValueExtractor(name string) SlugExtractor {
	return func(r *http.Request) (string, bool) {
		v := r.PathValue(name)
		if v == "" {
			return "", false
		}
		return v, true
	}
}
