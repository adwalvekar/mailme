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
	mu   sync.Mutex
	next time.Time
}

func (p *Pacer) Wait(ctx context.Context, units int, profile string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	when := p.next
	if when.Before(time.Now()) {
		when = time.Now()
	}
	rate := 80
	if profile == "legacy" {
		rate = 125
	}
	p.next = when.Add(time.Duration(units) * time.Second / time.Duration(rate))
	p.mu.Unlock()
	return wait(ctx, time.Until(when))
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
