package gmail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPolicyValidationAndBackoff(t *testing.T) {
	p := DefaultRequestPolicy()
	for _, profile := range []string{"current", "legacy"} {
		if err := p.Validate(profile); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		profile string
		change  func(*RequestPolicy)
	}{
		{"current", func(p *RequestPolicy) { p.QuotaUnitsPerSecond = 101 }},
		{"legacy", func(p *RequestPolicy) { p.QuotaUnitsPerSecond = 251 }},
		{"current", func(p *RequestPolicy) { p.ReadRetries = -1 }},
		{"current", func(p *RequestPolicy) { p.ReadRetries = 11 }},
		{"current", func(p *RequestPolicy) { p.RetryMinDelay = 0 }},
		{"current", func(p *RequestPolicy) { p.RetryMaxDelay = time.Millisecond }},
		{"current", func(p *RequestPolicy) { p.RequestTimeout = 0 }},
	} {
		bad := p
		test.change(&bad)
		if bad.Validate(test.profile) == nil {
			t.Fatalf("invalid policy accepted: %+v", bad)
		}
	}
	p.RetryMaxDelay = 4 * time.Second
	for attempt := 0; attempt < 10; attempt++ {
		base := min(time.Second<<attempt, p.RetryMaxDelay)
		delay := p.Delay(attempt, nil)
		if delay < base || delay > min(base+base/2, p.RetryMaxDelay) {
			t.Fatalf("attempt %d delay=%s", attempt, delay)
		}
	}
	if p.Delay(0, fmt.Errorf("wrapped: %w", &APIError{RetryAfter: 2 * time.Minute})) != 2*time.Minute {
		t.Fatal("server cooldown shortened to backoff cap")
	}
}

func TestPacerCancellationDoesNotReserveQuota(t *testing.T) {
	var p Pacer
	if err := p.Wait(context.Background(), 20, "current", 100); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	reserved := p.next
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx, 100, "current", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.next.Equal(reserved) {
		t.Fatal("canceled request left quota debt")
	}
}

func TestQueuedWriteCanCancel(t *testing.T) {
	var gate WriteGate
	if err := gate.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer gate.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := gate.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write ignored cancellation: %v", err)
	}
}

func TestPacerQueuedRequestsRecheckCooldownAndStayPaced(t *testing.T) {
	var p Pacer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Wait(ctx, 10, "current", 100); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	admitted := make(chan time.Time, 3)
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Wait(ctx, 10, "current", 100); err != nil {
				t.Error(err)
			}
			admitted <- time.Now()
		}()
	}
	// Extend cooldown while requests are already waiting on an older deadline.
	time.Sleep(20 * time.Millisecond)
	until := time.Now().Add(200 * time.Millisecond)
	p.Pause(200 * time.Millisecond)
	wg.Wait()
	close(admitted)
	var previous time.Time
	for when := range admitted {
		if when.Before(until) || !previous.IsZero() && when.Sub(previous) < 90*time.Millisecond {
			t.Fatalf("request escaped cooldown or quota spacing: %s, previous=%s", when, previous)
		}
		previous = when
	}
}

func TestHTTPPolicyRetriesAndSharedCooldown(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":429}}`)
			return
		}
		fmt.Fprint(w, `{"emailAddress":"me@example.com","historyId":"10"}`)
	}))
	defer s.Close()
	policy := DefaultRequestPolicy()
	policy.ReadRetries = 0
	c := &Client{HTTP: s.Client(), BaseURL: s.URL, Policy: &policy}
	if _, err := c.Profile(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatalf("zero retries ignored: %v, calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Send(ctx, []byte("test")); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("write escaped read's shared cooldown: %v, calls=%d", err, calls.Load())
	}
	c.pace.mu.Lock()
	remaining := time.Until(c.pace.cooldown)
	c.pace.mu.Unlock()
	if remaining < 119*time.Second {
		t.Fatalf("Retry-After truncated: %s", remaining)
	}
}

func TestHTTPConfiguredRequestTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer s.Close()
	policy := DefaultRequestPolicy()
	policy.RequestTimeout = 30 * time.Millisecond
	c := &Client{HTTP: s.Client(), BaseURL: s.URL, Policy: &policy}
	start := time.Now()
	if _, err := c.Profile(context.Background()); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("request deadline ignored: %v", err)
	}
}

func TestHTTPBodyTimeoutIsNotSkippedAsMalformedMessage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	policy := DefaultRequestPolicy()
	policy.RequestTimeout = 30 * time.Millisecond
	c := &Client{HTTP: s.Client(), BaseURL: s.URL, Policy: &policy}
	if _, err := c.Message(context.Background(), "a", "metadata"); !errors.Is(err, context.DeadlineExceeded) || PerMessage(err) {
		t.Fatalf("timed-out message misclassified as malformed: %v", err)
	}
}
