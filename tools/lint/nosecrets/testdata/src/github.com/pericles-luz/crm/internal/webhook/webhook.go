// Package webhook is a stub of the production internal/webhook package
// used only by the nosecrets analyzer's testdata fixtures. It exposes the
// gate type whose VerifyApp method ends pre-HMAC scope.
package webhook

import "context"

// Adapter mirrors the production ChannelAdapter port. The lint only cares
// about the VerifyApp method name.
type Adapter struct{}

// VerifyApp simulates the HMAC verify boundary. Calling it inside a
// function body flips the analyzer out of pre-HMAC mode for the rest of
// the body.
func (Adapter) VerifyApp(ctx context.Context, body []byte) error { return nil }
