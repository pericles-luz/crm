package upload

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrEmptyHash is returned by StoragePath if Hash is empty.
var ErrEmptyHash = errors.New("upload: hash must be non-empty")

// extByFormat maps the closed Format set to its canonical extension. Any
// new format must be added here AND to Sniff in the same change.
var extByFormat = map[Format]string{
	FormatPNG:  "png",
	FormatJPEG: "jpg",
	FormatWEBP: "webp",
	FormatPDF:  "pdf",
}

// StoragePath returns the deterministic storage key for a Result. The
// path is fully derived from server-controlled inputs (tenant UUID, the
// time the upload arrived, the SHA-256 hash, the validated Format) — it
// NEVER incorporates client-supplied filename, Content-Disposition, or
// Content-Type headers, which closes path-traversal, null-byte and CRLF
// vectors against object storage.
//
// Layout: media/<tenant_id>/<yyyy-mm>/<hash>.<ext>
func StoragePath(tenantID uuid.UUID, when time.Time, r Result) (string, error) {
	if tenantID == uuid.Nil {
		return "", fmt.Errorf("upload: tenantID must not be uuid.Nil")
	}
	if r.Hash == "" {
		return "", ErrEmptyHash
	}
	ext, ok := extByFormat[r.Format]
	if !ok {
		return "", fmt.Errorf("upload: no extension known for format %q", r.Format)
	}
	// Hashes from Process are already lowercase hex from encoding/hex; the
	// ToLower is belt-and-suspenders for any future caller that constructs
	// a Result manually.
	hash := strings.ToLower(r.Hash)
	yyyymm := when.UTC().Format("2006-01")
	return fmt.Sprintf("media/%s/%s/%s.%s", tenantID.String(), yyyymm, hash, ext), nil
}
