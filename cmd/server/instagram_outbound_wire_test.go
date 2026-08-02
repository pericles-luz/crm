package main

// Composition-root tests for buildInstagramOutboundEntry, mirroring
// messenger_wire_test.go's coverage.
//
// Deliberately does NOT test the token-present ("ok=true") path here: that
// would call the real channelinstagram.New against prometheus.DefaultRegisterer,
// and a second call anywhere else in this test binary would panic with
// "duplicate metrics collector registration attempted" — the exact bug
// class fixed earlier for WhatsApp. The sender's own behavior (including
// its metrics) is already covered at the package level by
// internal/adapter/channel/instagram/sender_test.go against a fresh,
// isolated registry per test.

import (
	"testing"
)

func TestBuildInstagramOutboundEntry_DisabledWhenTokenMissing(t *testing.T) {
	t.Parallel()
	entry, ok := buildInstagramOutboundEntry(func(string) string { return "" }, nil, nil, nil)
	if ok {
		t.Fatalf("ok = true, want false when no graph token is set")
	}
	if entry != nil {
		t.Fatalf("entry = %v, want nil when disabled", entry)
	}
}

func TestInstagramOutboundGraphToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unset", env: map[string]string{}, want: ""},
		{
			// No fallback to META_GRAPH_TOKEN: that's a Business Manager
			// System User / Page Access Token (graph.facebook.com), a
			// different auth family from the Instagram User access token
			// this sender needs (graph.instagram.com) — see the const's
			// doc comment.
			name: "shared META_GRAPH_TOKEN is NOT used as a fallback",
			env:  map[string]string{"META_GRAPH_TOKEN": "whatsapp-token"},
			want: "",
		},
		{
			name: "dedicated token",
			env:  map[string]string{"META_INSTAGRAM_GRAPH_TOKEN": "instagram-only-token"},
			want: "instagram-only-token",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tc.env[k] }
			if got := instagramOutboundGraphToken(getenv); got != tc.want {
				t.Fatalf("instagramOutboundGraphToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstagramOutboundRateMaxPerMin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want int
	}{
		{name: "unset falls back to default", env: map[string]string{}, want: defaultInstagramRateMaxPerMin},
		{name: "invalid falls back to default", env: map[string]string{"INSTAGRAM_OUTBOUND_RATE_MAX_PER_MIN": "abc"}, want: defaultInstagramRateMaxPerMin},
		{name: "zero falls back to default", env: map[string]string{"INSTAGRAM_OUTBOUND_RATE_MAX_PER_MIN": "0"}, want: defaultInstagramRateMaxPerMin},
		{name: "valid override", env: map[string]string{"INSTAGRAM_OUTBOUND_RATE_MAX_PER_MIN": "42"}, want: 42},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tc.env[k] }
			if got := instagramOutboundRateMaxPerMin(getenv); got != tc.want {
				t.Fatalf("instagramOutboundRateMaxPerMin = %d, want %d", got, tc.want)
			}
		})
	}
}
