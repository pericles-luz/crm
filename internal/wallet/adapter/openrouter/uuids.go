// Package openrouter holds the OpenRouter cost-API adapter and the
// production IDGenerator. The full HTTP adapter is split to a child
// issue (see SIN-62240 plan); this file ships the IDGenerator now so
// the wiring is complete.
package openrouter

import "github.com/google/uuid"

// UUIDs is the production IDGenerator — a uuid.NewString() wrapper.
type UUIDs struct{}

// NewID returns a freshly generated UUIDv4 as a string.
func (UUIDs) NewID() string { return uuid.NewString() }
