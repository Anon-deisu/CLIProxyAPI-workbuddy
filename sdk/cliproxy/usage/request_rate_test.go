package usage

import (
	"sync"
	"testing"
	"time"
)

func TestRequestRateRollingWindowAndIdleDecay(t *testing.T) {
	rate := &RequestRate{}
	now := time.Unix(1000, 0)
	rate.Record(now.Add(-60 * time.Second))
	rate.Record(now.Add(-59 * time.Second))
	rate.Record(now)
	rate.Record(now)
	if got := rate.Count(now); got != 3 {
		t.Fatalf("RPM = %d, want 3", got)
	}
	if got := rate.Count(now.Add(time.Second)); got != 2 {
		t.Fatalf("expired bucket remained in RPM: %d", got)
	}
	if got := rate.Count(now.Add(time.Minute)); got != 0 {
		t.Fatalf("idle RPM did not decay to zero: %d", got)
	}
}

func TestRequestRateConcurrentArrivalsAndContext(t *testing.T) {
	rate := &RequestRate{}
	ctx := WithRequestRate(t.Context(), rate)
	now := time.Now()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { RecordRequestArrival(ctx, now) })
	}
	wg.Wait()
	RecordRequestArrival(t.Context(), now)
	if got := rate.Count(now); got != 100 {
		t.Fatalf("concurrent arrivals lost: %d", got)
	}
}
