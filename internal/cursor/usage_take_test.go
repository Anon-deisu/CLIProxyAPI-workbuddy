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
