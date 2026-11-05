package gmail

import (
	"context"
	"sync"
	"time"
)

// Use current (May 2026) quotas by default. The legacy profile is explicit and
// only appropriate for a project confirmed to retain the old costs and limits.
// Reserve headroom for foreground operations and other account activity.
type Pacer struct {
	mu       sync.Mutex
	next     time.Time
	cooldown time.Time
	gate     chan struct{}
}

func (p *Pacer) Wait(ctx context.Context, units int, profile string, rates ...int) error {
	rate := DefaultRequestPolicy().Rate(profile)
	if len(rates) > 0 && rates[0] > 0 {
		rate = rates[0]
	}
	// One admission waiter owns the timer; others queue without reserving slots.
	// Channel waiters can cancel and foreground requests are not starved by a
	// worker repeatedly winning a race for the next quota interval.
	p.mu.Lock()
	if p.gate == nil {
		p.gate = make(chan struct{}, 1)
	}
	gate := p.gate
	p.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := time.Now()
		when := p.next
		if p.cooldown.After(when) {
			when = p.cooldown
		}
		if !now.Before(when) {
			// Reserve only on admission. Canceled waiters leave no quota debt,
			// and all queued requests recheck cooldown before they can proceed.
			p.next = now.Add(time.Duration(units) * time.Second / time.Duration(rate))
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		if err := wait(ctx, time.Until(when)); err != nil {
			return err
		}
	}
}

// Pause extends the account-wide cooldown, including writes and queued reads.
// Requests already in flight cannot be recalled.
func (p *Pacer) Pause(delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until := time.Now().Add(delay); until.After(p.cooldown) {
		p.cooldown = until
	}
}

func ValidQuotaProfile(profile string) bool { return profile == "current" || profile == "legacy" }

func Cost(method, format, profile string) int {
	switch method {
	case "messages.send":
		return 100
	case "getProfile", "labels.list":
		return 1
	case "history.list":
		return 2
	case "messages.get":
		if profile == "legacy" {
			return 5
		}
		if format == "raw" || format == "full" || format == "html" {
			return 60
		}
		return 20
	case "messages.trash":
		if profile == "legacy" {
			return 5
		}
		return 20
	default:
		return 5
	}
}
