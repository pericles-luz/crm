package upload

import (
	"fmt"
	"strings"
)

// PT-BR error strings shared with static/upload.js. Keep them byte-identical
// across both sides — the JS unit tests pin the same constants.
const (
	MsgUnsupportedLogo       = "Tipo de arquivo não suportado. Use PNG, JPG ou WEBP."
	MsgUnsupportedAttachment = "Tipo de arquivo não suportado. Use PNG, JPG, WEBP ou PDF."
	MsgServerRejected        = "Tipo de arquivo rejeitado pelo servidor."
	MsgRateLimited           = "Muitos uploads em sequência. Tente novamente em alguns segundos."
	MsgDecompressionBomb     = "Imagem com dimensões não suportadas."
	MsgNetwork               = "Falha de rede. Verifique sua conexão e tente novamente."
	MsgUnknown               = "Não foi possível enviar o arquivo. Tente novamente."
	MsgCancelled             = "Upload cancelado."

	// MsgTooLargePrefix is concatenated with HumanBytes(maxBytes)+"." to
	// build the 413 message. Kept as a prefix (rather than fmt.Sprintf)
	// so JS and Go produce identical output.
	MsgTooLargePrefix = "Arquivo muito grande. Limite: "
)

// MessageContext carries optional context for status/error-code resolution.
// Currently only MaxBytes is used (for 413).
type MessageContext struct {
	MaxBytes int64
}

// MessageForStatus maps an HTTP status code to a PT-BR user-facing string.
// Status 0 means "request didn't reach server" (network failure). Anything
// outside the small documented set falls back to MsgUnknown so the user
// always sees a Portuguese sentence.
//
// Defense-in-depth note: we never echo server response bodies into the
// message — only the status code. This guarantees no internal detail
// (path, stack, hash) leaks to the user even if a buggy upstream layer
// puts one in the body.
func MessageForStatus(status int, ctx MessageContext) string {
	switch status {
	case 413:
		if ctx.MaxBytes > 0 {
			return MsgTooLargePrefix + HumanBytes(ctx.MaxBytes) + "."
		}
		return "Arquivo muito grande."
	case 415:
		return MsgServerRejected
	case 429:
		return MsgRateLimited
	case 0:
		return MsgNetwork
	default:
		return MsgUnknown
	}
}

// MessageForCode maps a server-emitted error code (the JSON body's "code"
// field) plus the HTTP status to a PT-BR string. Recognised codes:
//
//	"decompression_bomb" → MsgDecompressionBomb
//
// Anything else falls back to MessageForStatus(status, ctx). Caller is
// responsible for trimming and lowercasing if their server emits mixed
// case (this fn only matches lowercase canonical codes).
func MessageForCode(code string, status int, ctx MessageContext) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "decompression_bomb":
		return MsgDecompressionBomb
	}
	return MessageForStatus(status, ctx)
}

// MessageForKind returns the unsupported-format message appropriate to a
// form kind. Used by the http handler when it wants to emit a 415 message
// in the same vocabulary the JS uses client-side.
func MessageForKind(k Kind) string {
	if k == KindAttachment {
		return MsgUnsupportedAttachment
	}
	return MsgUnsupportedLogo
}

// FormatTooLarge builds the PT-BR "file too large" message for a given
// byte budget. Exposed so server-side handlers can emit a consistent 413
// body when they choose to write a JSON envelope alongside the status.
func FormatTooLarge(maxBytes int64) string {
	if maxBytes <= 0 {
		return "Arquivo muito grande."
	}
	return fmt.Sprintf("%s%s.", MsgTooLargePrefix, HumanBytes(maxBytes))
}
