package syncer

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// rateLimitedReader wraps an io.Reader with a token-bucket rate limiter.
// The limiter is shared across all concurrent workers so total throughput
// is capped at the configured rate.
type rateLimitedReader struct {
	ctx     context.Context
	r       io.Reader
	limiter *rate.Limiter
	tracker *TransferTracker
}

// NewRateLimiter creates a token-bucket limiter for the given MB/s cap.
// burstMB is the burst size in MB (set equal to rateMbps for a smooth cap).
func NewRateLimiter(rateMbps float64) *rate.Limiter {
	if rateMbps <= 0 {
		// Allow effectively unlimited throughput.
		return rate.NewLimiter(rate.Inf, 0)
	}
	bytesPerSec := rateMbps * 1024 * 1024
	burst := int(bytesPerSec) // 1-second burst
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// WrapReader wraps r with rate limiting from the shared limiter.
func WrapReader(ctx context.Context, r io.Reader, limiter *rate.Limiter, tracker *TransferTracker) io.Reader {
	if limiter == nil || limiter.Limit() == rate.Inf {
		if tracker == nil {
			return r
		}
		return &countingReader{r: r, tracker: tracker}
	}
	return &rateLimitedReader{ctx: ctx, r: r, limiter: limiter, tracker: tracker}
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	// WaitN returns an error when n > burst, so cap the slice to burst size.
	// This ensures a single Read call never requests more tokens than available.
	if burst := r.limiter.Burst(); len(p) > burst {
		p = p[:burst]
	}

	n, err := r.r.Read(p)
	if n <= 0 {
		return n, err
	}

	// Wait for enough tokens to cover the bytes just read.
	if waitErr := r.limiter.WaitN(r.ctx, n); waitErr != nil {
		return n, waitErr
	}
	r.tracker.AddBytes(n)
	return n, err
}

type countingReader struct {
	r       io.Reader
	tracker *TransferTracker
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.tracker.AddBytes(n)
	return n, err
}
