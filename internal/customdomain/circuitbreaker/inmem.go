package circuitbreaker

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// InMemoryState is a State backed by in-process maps. It implements the
// same atomicity contract as the Redis adapter would, using a single
// mutex per UseCase. Suitable for unit tests AND for single-replica
// deployments where the breaker state can be lost on restart (a 24h
// freeze would just expire silently).
type InMemoryState struct {
	mu       sync.Mutex
	failures map[uuid.UUID][]time.Time
	frozen   map[uuid.UUID]time.Time // tenantID -> freeze deadline
}

// NewInMemoryState returns a freshly-initialised in-memory state store.
func NewInMemoryState() *InMemoryState {
	return &InMemoryState{
		failures: map[uuid.UUID][]time.Time{},
		frozen:   map[uuid.UUID]time.Time{},
	}
}

// RecordFailure implements State.
func (s *InMemoryState) RecordFailure(_ context.Context, tenantID uuid.UUID, _ string, now time.Time, window time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-window)
	kept := s.failures[tenantID][:0]
	for _, t := range s.failures[tenantID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	s.failures[tenantID] = kept
	return len(kept), nil
}

// Trip implements State.
func (s *InMemoryState) Trip(_ context.Context, tenantID uuid.UUID, now time.Time, freezeFor time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen[tenantID] = now.Add(freezeFor)
	return nil
}

// IsOpen implements State.
func (s *InMemoryState) IsOpen(_ context.Context, tenantID uuid.UUID, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.frozen[tenantID]
	if !ok {
		return false, nil
	}
	if now.Before(deadline) {
		return true, nil
	}
	delete(s.frozen, tenantID)
	return false, nil
}

// Reset implements State.
func (s *InMemoryState) Reset(_ context.Context, tenantID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.frozen, tenantID)
	delete(s.failures, tenantID)
	return nil
}

// Compile-time guard.
var _ State = (*InMemoryState)(nil)
