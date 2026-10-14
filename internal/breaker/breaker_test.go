package breaker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/breaker"
)

func TestStaysClosedBelowThreshold(t *testing.T) {
	b := breaker.New(3, time.Minute)
	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow: %v", err)
		}
		b.RecordFailure()
	}
	if err := b.Allow(); err != nil {
		t.Errorf("below threshold: want allow, got %v", err)
	}
	if b.State() != breaker.Closed {
		t.Errorf("State = %v, want closed", b.State())
	}
}

func TestOpensAtThresholdAndRecovers(t *testing.T) {
	now := time.Now()
	current := now
	clock := func() time.Time { return current }
	b := breaker.NewWithClock(2, time.Minute, clock)

	b.RecordFailure()
	b.RecordFailure()
	if b.State() != breaker.Open {
		t.Fatalf("after 2 failures: State = %v, want open", b.State())
	}
	if err := b.Allow(); !errors.Is(err, breaker.ErrOpen) {
		t.Errorf("while open: Allow = %v, want ErrOpen", err)
	}

	// Cooldown passes: one probe is allowed.
	current = now.Add(61 * time.Second)
	if b.State() != breaker.HalfOpen {
		t.Errorf("after cooldown: State = %v, want half-open", b.State())
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("probe Allow: %v", err)
	}
	// A second concurrent request during the probe fails fast.
	if err := b.Allow(); !errors.Is(err, breaker.ErrOpen) {
		t.Errorf("second probe: Allow = %v, want ErrOpen", err)
	}
	// Probe succeeds: breaker closes.
	b.RecordSuccess()
	if b.State() != breaker.Closed {
		t.Errorf("after probe success: State = %v, want closed", b.State())
	}
	if err := b.Allow(); err != nil {
		t.Errorf("after close: Allow = %v", err)
	}
}

func TestProbeFailureReopens(t *testing.T) {
	now := time.Now()
	current := now
	clock := func() time.Time { return current }
	b := breaker.NewWithClock(1, time.Minute, clock)

	b.RecordFailure() // trips open
	if b.State() != breaker.Open {
		t.Fatalf("State = %v, want open", b.State())
	}
	current = now.Add(61 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("probe Allow: %v", err)
	}
	b.RecordFailure() // probe failed: re-open
	if b.State() != breaker.Open {
		t.Errorf("after probe failure: State = %v, want open", b.State())
	}
	if err := b.Allow(); !errors.Is(err, breaker.ErrOpen) {
		t.Errorf("Allow = %v, want ErrOpen", err)
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := breaker.New(2, time.Minute)
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	if b.State() != breaker.Closed {
		t.Errorf("one failure after a success: State = %v, want closed", b.State())
	}
}

func TestZeroThresholdNeverTrips(t *testing.T) {
	b := breaker.New(0, time.Minute)
	for i := 0; i < 100; i++ {
		b.RecordFailure()
	}
	if b.State() != breaker.Closed {
		t.Errorf("threshold 0: State = %v, want closed", b.State())
	}
}
