package main

// SIN-62331 F51 — HTTP wiring for media upload (F47/F48) and serve
// (F49). Both bundles' domain code already exists; this file mounts
// them on the HTTP listeners so the OWASP A05 misconfig hole called
// out in the SIN-62328 re-review is closed.
//
// Two muxes:
//
//   - Cookied app mux (the public listener): hosts the upload endpoint
//     `POST /api/tenant/uploads/logo`. The upload pipeline (magic-byte
//     whitelist, polyglot-aware re-encode, decompression-bomb cap) runs
//     here. ADR 0080 §5 is the spec.
//   - Cookieless static-origin mux (separate listener gated by
//     STATIC_HTTP_ADDR): hosts `GET /t/{tenantID}/logo` and
//     `GET /t/{tenantID}/m/{hash}`. The MediaHeaders middleware wraps
//     the entire mount so every byte that traverses the static origin
//     carries nosniff + CSP + CORP. ADR 0080 §6/§7 is the spec.
//
// Persistence (writing the re-encoded bytes to S3/Postgres) is on a
// separate ticket — the upload handler here returns 200 with the
// content hash on success but does NOT yet write to the media table.
// That keeps F47/F48 at-the-boundary defenses fully exercisable in
// integration tests today; SIN-62246 follow-up wires the storage.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	codecstdlib "github.com/pericles-luz/crm/adapters/imagecodec/stdlib"
	mediaserve "github.com/pericles-luz/crm/internal/media/serve"
	mediaupload "github.com/pericles-luz/crm/internal/media/upload"
)

const (
	envStaticOriginAddr = "STATIC_HTTP_ADDR"
	defaultStaticAddr   = ":8082"
	uploadFormField     = "file"
	uploadLogoMaxBytes  = 2 << 20 // 2 MiB — ADR 0080 §5 (logo de tenant).
	uploadLogoMaxWidth  = 1024
	uploadLogoMaxHeight = 1024
)

// logoUploadPolicy is the canonical ADR 0080 §5 policy for the tenant
// logo endpoint. PNG/JPEG/WEBP only — SVG is rejected at the magic-byte
// gate, so the same handler proves the F47 mitigation fires.
var logoUploadPolicy = mediaupload.Policy{
	Allowed:   []mediaupload.Format{mediaupload.FormatPNG, mediaupload.FormatJPEG, mediaupload.FormatWEBP},
	MaxBytes:  uploadLogoMaxBytes,
	MaxWidth:  uploadLogoMaxWidth,
	MaxHeight: uploadLogoMaxHeight,
}

// mediaUploadHandler returns the HTTP handler that runs the upload
// pipeline against the request body. It does not persist the result —
// only validates and reports the hash + format on success. The
// validation-only posture is intentional for SIN-62331: the F47/F48
// mitigations exercise end-to-end without coupling this PR to the
// SIN-62246 storage adapter.
//
// Wire shape:
//
//   - Multipart/form-data, field name `file`. The form is capped at
//     uploadLogoMaxBytes via http.MaxBytesReader before we ever touch
//     the body.
//   - Success: 200 application/json `{"hash": "...", "format": "..."}`.
//   - SVG / unknown magic: 415.
//   - Oversize / decompression bomb: 413.
//   - Format mismatch / decode failure: 400.
//   - Empty body: 400.
//   - Internal: 500.
func mediaUploadHandler(policy mediaupload.Policy) http.Handler {
	codec := codecstdlib.New()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Cap the request body before parsing so an attacker cannot
		// blow memory on the multipart parser. Doubling the policy max
		// gives headroom for multipart envelope bytes; the policy
		// itself enforces the on-the-wire image cap below.
		r.Body = http.MaxBytesReader(w, r.Body, policy.MaxBytes*2)
		if err := r.ParseMultipartForm(policy.MaxBytes); err != nil {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		f, _, err := r.FormFile(uploadFormField)
		if err != nil {
			http.Error(w, "file field required", http.StatusBadRequest)
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, policy.MaxBytes+1))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		res, err := mediaupload.Process(r.Context(), raw, policy, codec, codec)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"hash":   res.Hash,
				"format": string(res.Format),
			})
		case errors.Is(err, mediaupload.ErrEmpty):
			http.Error(w, "empty payload", http.StatusBadRequest)
		case errors.Is(err, mediaupload.ErrTooLarge),
			errors.Is(err, mediaupload.ErrDecompressionBomb),
			errors.Is(err, mediaupload.ErrDimensionsExceeded):
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, mediaupload.ErrUnknownFormat),
			errors.Is(err, mediaupload.ErrFormatNotAllowed):
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		case errors.Is(err, mediaupload.ErrContentTypeMismatch),
			errors.Is(err, mediaupload.ErrDecodeFailed):
			http.Error(w, "invalid image", http.StatusBadRequest)
		default:
			log.Printf("crm: upload pipeline error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
}

// registerUploadRoutes mounts the SIN-62331 upload routes on the
// cookied app mux. The route is `POST /api/tenant/uploads/logo`; new
// upload kinds (attachment, etc.) join here with their own policy.
func registerUploadRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/tenant/uploads/logo", mediaUploadHandler(logoUploadPolicy))
}

// buildMediaServeHandler returns the http.Handler the static-origin
// listener mounts. When the storage backend is not yet configured (the
// usual case until SIN-62246 follow-up wires Postgres + S3) the
// handler returns 404 for every request but still emits the
// MediaHeaders defense-in-depth response headers, so the smoke tests
// can assert the headers fire.
func buildMediaServeHandler() (http.Handler, error) {
	store, blob := nopMediaStorage{}, nopMediaStorage{}
	h, err := mediaserve.New(store, blob)
	if err != nil {
		return nil, err
	}
	return h.Routes(), nil
}

// runStaticOrigin runs the cookieless static-origin listener. It only
// starts when STATIC_HTTP_ADDR is set, mirroring the pattern used by
// the F45 internal listener: docker-internal compose reaches it via
// the service network; the host network never sees it because compose
// does not publish the port.
func runStaticOrigin(ctx context.Context, addr string, handler http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: static origin listener on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// nopMediaStorage satisfies both mediaserve.Store and mediaserve.Blob
// with "not found" answers. The static-origin listener stays running
// before the SIN-62246 follow-up so MediaHeaders is exercisable from a
// smoke test today, and so the catch-all in production fails closed
// (404) rather than 500.
type nopMediaStorage struct{}

func (nopMediaStorage) LookupLogo(context.Context, uuid.UUID) (mediaserve.Media, error) {
	return mediaserve.Media{}, mediaserve.ErrNotFound
}
func (nopMediaStorage) LookupHash(context.Context, uuid.UUID, string) (mediaserve.Media, error) {
	return mediaserve.Media{}, mediaserve.ErrNotFound
}
func (nopMediaStorage) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, mediaserve.ErrNotFound
}

// Compile-time guards.
var (
	_ mediaserve.Store = nopMediaStorage{}
	_ mediaserve.Blob  = nopMediaStorage{}
)
