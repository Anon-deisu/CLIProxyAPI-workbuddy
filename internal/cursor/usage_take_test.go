package cursor

import (
	"testing"
	"time"
)

func TestTakeUsageReturnsCountersThatArriveLate(t *testing.T) {
	s := &Session{}
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.recordUsage(StreamEvent{Type: "usage_final", InputTokens: 12, CacheReadTokens: 3400})
	}()
	// This is the tool-call ordering: the caller stops reading the segment
	// before Cursor sends TurnEnded.
	got, ok := s.TakeUsage(2 * time.Second)
	if !ok {
		t.Fatal("late counters were dropped")
	}
	if got.InputTokens != 12 || got.CacheReadTokens != 3400 {
		t.Fatalf("got %+v", got)
	}
}

func TestTakeUsageIsImmediateWhenAlreadySeen(t *testing.T) {
	s := &Session{}
	s.recordUsage(StreamEvent{Type: "usage_final", OutputTokens: 7})
	start := time.Now()
	got, ok := s.TakeUsage(3 * time.Second)
	if !ok || got.OutputTokens != 7 {
		t.Fatalf("ok=%v got=%+v", ok, got)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("waited %s for counters that were already there", elapsed)
	}
	if _, duplicate := s.TakeUsage(0); duplicate {
		t.Fatal("the same turn usage was returned twice")
	}
}

func TestTakeUsageGivesUpWithinTheBudget(t *testing.T) {
	s := &Session{}
	start := time.Now()
	if _, ok := s.TakeUsage(80 * time.Millisecond); ok {
		t.Fatal("reported counters that never arrived")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked for %s, want the 80ms budget", elapsed)
	}
}

// TestSegmentUsageDoesNotLeakAcrossSegments guards the central serialization
// bug: a TurnEnded belonging to a finished segment must never be credited to
// the next segment, whether it arrives early (before the boundary) or late
// (after the next segment has begun).
func TestSegmentUsageDoesNotLeakAcrossSegments(t *testing.T) {
	s := &Session{}
	// Segment 1 finishes and its usage lands in the out-of-band slot.
	s.recordUsage(StreamEvent{Type: "usage_final", InputTokens: 10, OutputTokens: 2})
	s.mu.Lock()
	s.terminalUsage = StreamEvent{Type: "usage_final", SegmentID: 1, InputTokens: 10, OutputTokens: 2}
	s.terminalUsageReady = true
	// Advance to segment 2, mirroring the resume path.
	s.segmentID = 2
	s.mu.Unlock()

	// Segment 2 must not inherit segment 1 usage through the event channel: the
	// terminal slot belongs to segment 1, so emitting for segment 2 is a no-op.
	emitted := captureEmissions(s, 20*time.Millisecond)
	if n := len(emitted); n != 0 {
		t.Fatalf("segment 1 usage leaked into segment 2 stream: %d events", n)
	}

	// The out-of-band path for segment 2 must also come up empty (its own usage
	// never arrived), not fall back to segment 1's counters.
	if _, ok := s.TakeUsageForSegment(2, 30*time.Millisecond); ok {
		t.Fatal("segment 2 received counters that belonged to segment 1")
	}
}

// TestSegmentUsageConsumedOnce verifies a usage delivered through the event
// channel at a boundary can never be re-served by TakeUsageForSegment, and
// that the same segment is not charged twice.
func TestSegmentUsageConsumedOnce(t *testing.T) {
	s := &Session{}
	// Give segment 1 a recorded usage and a terminal slot that matches it.
	s.recordUsage(StreamEvent{Type: "usage_final", InputTokens: 5, CacheReadTokens: 900})
	s.mu.Lock()
	copied := s.usageByID[1]
	copied.SegmentID = 1
	s.terminalUsage = copied
	s.terminalUsageReady = true
	s.mu.Unlock()

	// The boundary consumes it once via the event channel.
	emitted := captureEmissions(s, 20*time.Millisecond)
	if len(emitted) != 1 || emitted[0].InputTokens != 5 || emitted[0].CacheReadTokens != 900 {
		t.Fatalf("boundary did not emit segment 1 usage: %+v", emitted)
	}

	// Attempting to take it again must fail — already consumed.
	if _, ok := s.TakeUsageForSegment(1, 20*time.Millisecond); ok {
		t.Fatal("usage delivered through the event channel was served a second time")
	}
}

// TestTwoSegmentsConsumeInOrder verifies independent segments can each consume
// their own authoritative usage in sequence without one stealing the other's.
func TestTwoSegmentsConsumeInOrder(t *testing.T) {
	s := &Session{}
	s.recordUsage(StreamEvent{Type: "usage_final", InputTokens: 100, OutputTokens: 20})
	s.mu.Lock()
	s.segmentID = 1
	s.mu.Unlock()
	if got, ok := s.TakeUsageForSegment(1, 30*time.Millisecond); !ok || got.InputTokens != 100 {
		t.Fatalf("segment 1 usage: ok=%v got=%+v", ok, got)
	}
	// Second segment records its own usage after the first was consumed. Mirror
	// the resume path advancing the public segment before the next turn.
	s.mu.Lock()
	s.segmentID = 2
	s.mu.Unlock()
	s.recordUsage(StreamEvent{Type: "usage_final", InputTokens: 200, OutputTokens: 40})
	if got, ok := s.TakeUsageForSegment(2, 30*time.Millisecond); !ok || got.InputTokens != 200 {
		t.Fatalf("segment 2 usage: ok=%v got=%+v", ok, got)
	}
}

// captureEmissions runs emitTerminalUsage for the current segment and returns
// everything that lands on the event channel within the deadline.
func captureEmissions(s *Session, wait time.Duration) []StreamEvent {
	s.mu.Lock()
	s.events = make(chan StreamEvent, 8)
	s.done = make(chan struct{})
	s.mu.Unlock()
	s.emitTerminalUsage(s.SegmentID())
	var out []StreamEvent
	deadline := time.After(wait)
	for {
		select {
		case ev := <-s.events:
			out = append(out, ev)
			// Drain up to one more round so a spurious extra event surfaces.
			if len(out) > 1 {
				return out
			}
		case <-deadline:
			return out
		}
	}
}
