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
func WrapReader(ctx context.Context, r io.Reader, limiter *rate.Limiter) io.Reader {
	if limiter == nil || limiter.Limit() == rate.Inf {
		return r
	}
	return &rateLimitedReader{ctx: ctx, r: r, limiter: limiter}
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
	return n, err
}
