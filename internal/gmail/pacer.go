package gmail

import (
	"context"
	"sync"
	"time"
)

// Pace at 125 units/s, leaving room below the account's shared 250 units/s.
// messages.get costs 5 units; send costs 100. All workers share this budget.
type Pacer struct {
	mu   sync.Mutex
	next time.Time
}

func (p *Pacer) Wait(ctx context.Context, units int) error {
	p.mu.Lock()
	when := p.next
	if when.Before(time.Now()) {
		when = time.Now()
	}
	p.next = when.Add(time.Duration(units) * 8 * time.Millisecond)
	p.mu.Unlock()
	return wait(ctx, time.Until(when))
}

func Cost(method string) int {
	switch method {
	case "messages.send":
		return 100
	case "getProfile", "labels.list":
		return 1
	case "history.list":
		return 2
	default:
		return 5
	}
}
