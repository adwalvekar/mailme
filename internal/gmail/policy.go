package gmail

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// RequestPolicy is configured before a client starts serving requests. A nil
// policy on the HTTP client selects DefaultRequestPolicy.
type RequestPolicy struct {
	QuotaUnitsPerSecond int
	ReadRetries         int
	RetryMinDelay       time.Duration
	RetryMaxDelay       time.Duration
	RequestTimeout      time.Duration
}

func DefaultRequestPolicy() RequestPolicy {
	return RequestPolicy{ReadRetries: 3, RetryMinDelay: time.Second, RetryMaxDelay: time.Minute, RequestTimeout: time.Minute}
}

func (p RequestPolicy) Validate(profile string) error {
	if !ValidQuotaProfile(profile) {
		return errors.New("quota profile must be current or legacy")
	}
	limit := 100
	if profile == "legacy" {
		limit = 250
	}
	if p.QuotaUnitsPerSecond < 0 || p.QuotaUnitsPerSecond > limit {
		return fmt.Errorf("quota units per second must be between 0 (profile default) and %d for %s", limit, profile)
	}
	if p.ReadRetries < 0 || p.ReadRetries > 10 {
		return errors.New("read retries must be between 0 and 10")
	}
	if p.RetryMinDelay <= 0 || p.RetryMaxDelay < p.RetryMinDelay || p.RetryMaxDelay > time.Hour {
		return errors.New("retry delays must satisfy 0 < minimum <= maximum <= 1h")
	}
	if p.RequestTimeout <= 0 || p.RequestTimeout > 10*time.Minute {
		return errors.New("request timeout must be greater than zero and at most 10m")
	}
	return nil
}

func (p RequestPolicy) Rate(profile string) int {
	if p.QuotaUnitsPerSecond != 0 {
		return p.QuotaUnitsPerSecond
	}
	if profile == "legacy" {
		return 125
	}
	return 80
}

// Delay uses capped exponential backoff with jitter. A server's Retry-After is
// a lower bound, even when it exceeds our configured exponential-backoff cap.
func (p RequestPolicy) Delay(attempt int, err error) time.Duration {
	delay := p.RetryMinDelay
	for n := 0; n < attempt && delay < p.RetryMaxDelay; n++ {
		if delay > p.RetryMaxDelay/2 {
			delay = p.RetryMaxDelay
		} else {
			delay *= 2
		}
	}
	jitter := min(delay/2, p.RetryMaxDelay-delay)
	if jitter > 0 {
		delay += time.Duration(rand.Int64N(int64(jitter) + 1))
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
		delay = apiErr.RetryAfter
	}
	return delay
}

func RateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == 429 || apiErr.Status == 403 && Retryable(err))
}

// WriteGate serializes mutations while allowing queued callers to cancel,
// including when a preceding write is waiting through a long server cooldown.
type WriteGate struct {
	once sync.Once
	slot chan struct{}
}

func (g *WriteGate) Lock(ctx context.Context) error {
	g.once.Do(func() { g.slot = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *WriteGate) Unlock() { <-g.slot }
