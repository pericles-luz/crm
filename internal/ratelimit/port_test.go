package ratelimit_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit"
)

func TestLimit_IsZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   ratelimit.Limit
		want bool
	}{
		{"zero value", ratelimit.Limit{}, true},
		{"only window", ratelimit.Limit{Window: time.Second}, false},
		{"only max", ratelimit.Limit{Max: 1}, false},
		{"both fields", ratelimit.Limit{Window: time.Minute, Max: 5}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.IsZero(); got != tc.want {
				t.Fatalf("Limit{%v,%d}.IsZero() = %v, want %v", tc.in.Window, tc.in.Max, got, tc.want)
			}
		})
	}
}

func TestErrUnavailable_IsErrorsIsCompatible(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("redis: %w", ratelimit.ErrUnavailable)
	if !errors.Is(wrapped, ratelimit.ErrUnavailable) {
		t.Fatal("ErrUnavailable must be wrappable by errors.Is consumers")
	}
	if errors.Is(wrapped, ratelimit.ErrInvalidLimit) {
		t.Fatal("ErrUnavailable and ErrInvalidLimit must be distinguishable")
	}
}
