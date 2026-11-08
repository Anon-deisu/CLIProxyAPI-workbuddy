package usage

import (
	"context"
	"sync"
	"time"
)

const requestRateWindowSeconds = 60

type requestRateBucket struct {
	second int64
	count  int64
}

// RequestRate counts downstream arrivals in a rolling minute at one-second
// resolution. Upstream retries never enter this counter.
type RequestRate struct {
	mu      sync.Mutex
	buckets [requestRateWindowSeconds]requestRateBucket
}

func (r *RequestRate) Record(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	second := now.Unix()
	bucket := &r.buckets[second%requestRateWindowSeconds]
	if bucket.second != second {
		*bucket = requestRateBucket{second: second}
	}
	bucket.count++
}

func (r *RequestRate) Count(now time.Time) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, bucket := range r.buckets {
		if age := now.Unix() - bucket.second; age >= 0 && age < requestRateWindowSeconds {
			total += bucket.count
		}
	}
	return total
}

type requestRateContextKey struct{}

func WithRequestRate(ctx context.Context, rate *RequestRate) context.Context {
	return context.WithValue(ctx, requestRateContextKey{}, rate)
}

// RecordRequestArrival lets a WebSocket reader count each model operation,
// independently of its HTTP connection and any subsequent upstream retries.
func RecordRequestArrival(ctx context.Context, now time.Time) {
	if rate, ok := ctx.Value(requestRateContextKey{}).(*RequestRate); ok {
		rate.Record(now)
	}
}
