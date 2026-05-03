package upload

import "testing"

func TestMessageForStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		ctx    MessageContext
		want   string
	}{
		{"413 with limit", 413, MessageContext{MaxBytes: 2 * 1024 * 1024}, "Arquivo muito grande. Limite: 2MB."},
		{"413 with 20MB limit", 413, MessageContext{MaxBytes: 20 * 1024 * 1024}, "Arquivo muito grande. Limite: 20MB."},
		{"413 no limit", 413, MessageContext{}, "Arquivo muito grande."},
		{"415", 415, MessageContext{}, MsgServerRejected},
		{"429", 429, MessageContext{}, MsgRateLimited},
		{"network", 0, MessageContext{}, MsgNetwork},
		{"500 fallback", 500, MessageContext{}, MsgUnknown},
		{"401 fallback", 401, MessageContext{}, MsgUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := MessageForStatus(c.status, c.ctx)
			if got != c.want {
				t.Errorf("MessageForStatus(%d) = %q, want %q", c.status, got, c.want)
			}
		})
	}
}

func TestMessageForCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   string
		status int
		ctx    MessageContext
		want   string
	}{
		{"decompression_bomb (canonical)", "decompression_bomb", 400, MessageContext{}, MsgDecompressionBomb},
		{"decompression_bomb (mixed case)", "Decompression_Bomb", 400, MessageContext{}, MsgDecompressionBomb},
		{"decompression_bomb (whitespace)", "  decompression_bomb  ", 400, MessageContext{}, MsgDecompressionBomb},
		{"unknown code falls back to 415", "weird_code", 415, MessageContext{}, MsgServerRejected},
		{"empty code falls back to 413", "", 413, MessageContext{MaxBytes: 1024 * 1024}, "Arquivo muito grande. Limite: 1MB."},
		{"empty code falls back to 429", "", 429, MessageContext{}, MsgRateLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := MessageForCode(c.code, c.status, c.ctx)
			if got != c.want {
				t.Errorf("MessageForCode(%q,%d) = %q, want %q", c.code, c.status, got, c.want)
			}
		})
	}
}

func TestMessageForKind(t *testing.T) {
	t.Parallel()
	if got := MessageForKind(KindLogo); got != MsgUnsupportedLogo {
		t.Errorf("logo = %q, want %q", got, MsgUnsupportedLogo)
	}
	if got := MessageForKind(KindAttachment); got != MsgUnsupportedAttachment {
		t.Errorf("attachment = %q, want %q", got, MsgUnsupportedAttachment)
	}
	if got := MessageForKind(Kind("unknown")); got != MsgUnsupportedLogo {
		t.Errorf("unknown kind = %q, want default %q", got, MsgUnsupportedLogo)
	}
}

func TestFormatTooLarge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{2 * 1024 * 1024, "Arquivo muito grande. Limite: 2MB."},
		{20 * 1024 * 1024, "Arquivo muito grande. Limite: 20MB."},
		{0, "Arquivo muito grande."},
		{-1, "Arquivo muito grande."},
		{1500 * 1024, "Arquivo muito grande. Limite: 1.5MB."},
	}
	for _, c := range cases {
		got := FormatTooLarge(c.in)
		if got != c.want {
			t.Errorf("FormatTooLarge(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Pin canonical PT-BR strings as a contract surface — JS unit tests
// pin the same strings, so any drift here surfaces in CI.
func TestMessageConstantsAreNonEmptyAndPortuguese(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{
		"MsgUnsupportedLogo":       MsgUnsupportedLogo,
		"MsgUnsupportedAttachment": MsgUnsupportedAttachment,
		"MsgServerRejected":        MsgServerRejected,
		"MsgRateLimited":           MsgRateLimited,
		"MsgDecompressionBomb":     MsgDecompressionBomb,
		"MsgNetwork":               MsgNetwork,
		"MsgUnknown":               MsgUnknown,
		"MsgCancelled":             MsgCancelled,
		"MsgTooLargePrefix":        MsgTooLargePrefix,
	} {
		if s == "" {
			t.Errorf("%s is empty", name)
		}
	}
	// Sanity-check a couple of PT-BR-specific tokens to make sure these
	// constants are PT-BR (catches an accidental refactor that switches
	// to English strings).
	if !contains(MsgRateLimited, "Tente novamente") {
		t.Errorf("MsgRateLimited not PT-BR: %q", MsgRateLimited)
	}
	if !contains(MsgUnsupportedLogo, "não suportado") {
		t.Errorf("MsgUnsupportedLogo not PT-BR: %q", MsgUnsupportedLogo)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
