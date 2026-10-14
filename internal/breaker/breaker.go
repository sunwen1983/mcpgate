// Package breaker implements a per-backend circuit breaker.
//
// States:
//   - Closed: requests flow through. N consecutive failures trip the breaker.
//   - Open: requests fail fast without touching the backend. After the
//     cooldown, the breaker moves to HalfOpen and lets one probe through.
//   - HalfOpen: a single probe request is allowed. Success closes the
//     breaker; failure re-opens it for another cooldown.
//
// The clock is injectable so tests can control time.
package breaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned by Allow when the breaker is open.
var ErrOpen = errors.New("circuit breaker is open")

// State is a breaker state.
type State int

const (
	// Closed lets requests through.
	Closed State = iota
	// Open fails fast.
	Open
	// HalfOpen allows one probe request.
	HalfOpen
)

// String renders the state name (used in logs and metrics).
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is a single circuit breaker.
type Breaker struct {
	mu               sync.Mutex
	state            State
	consecutiveFail  int
	openedAt         time.Time
	halfOpenInFlight bool

	failureThreshold int
	cooldown         time.Duration
	now              func() time.Time
}

// New builds a Breaker. A failureThreshold <= 0 disables tripping (the
// breaker stays closed forever), which is useful for backends where the
// operator does not want fail-fast behavior.
func New(failureThreshold int, cooldown time.Duration) *Breaker {
	return NewWithClock(failureThreshold, cooldown, time.Now)
}

// NewWithClock builds a Breaker using now as its clock.
func NewWithClock(failureThreshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	return &Breaker{
		state:            Closed,
		failureThreshold: failureThreshold,
		cooldown:         cooldown,
		now:              now,
	}
}

// Allow reports whether a request may proceed. When the breaker is
// half-open, only one probe is allowed at a time.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return nil
	case Open:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = HalfOpen
			b.halfOpenInFlight = true
			return nil
		}
		return ErrOpen
	case HalfOpen:
		if b.halfOpenInFlight {
			return ErrOpen
		}
		b.halfOpenInFlight = true
		return nil
	default:
		return nil
	}
}

// RecordSuccess records a successful request.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFail = 0
	if b.state == HalfOpen {
		b.state = Closed
		b.halfOpenInFlight = false
	}
}

// RecordFailure records a failed request.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFail++
	switch b.state {
	case Closed:
		if b.failureThreshold > 0 && b.consecutiveFail >= b.failureThreshold {
			b.state = Open
			b.openedAt = b.now()
		}
	case HalfOpen:
		b.state = Open
		b.openedAt = b.now()
		b.halfOpenInFlight = false
	}
}

// State returns the current breaker state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Lazily surface the half-open transition so State() reflects reality
	// even when no request arrives exactly at the cooldown boundary.
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}
