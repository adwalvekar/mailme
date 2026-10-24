package gws

import (
	"context"
	"sync"
)

// Cancellation-aware reader/writer barrier for the single credential probe.
type authBarrier struct {
	mu               sync.Mutex
	readers, waiting int
	writer           bool
	changed          chan struct{}
}

func (b *authBarrier) signal() {
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
}
func (b *authBarrier) lock(ctx context.Context, write bool) error {
	b.mu.Lock()
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	if write {
		b.waiting++
	}
	for {
		if err := ctx.Err(); err != nil {
			if write {
				b.waiting--
				b.signal()
			}
			b.mu.Unlock()
			return err
		}
		if !b.writer && (write && b.readers == 0 || !write && b.waiting == 0) {
			if write {
				b.waiting--
				b.writer = true
			} else {
				b.readers++
			}
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		b.mu.Lock()
	}
}
func (b *authBarrier) unlock(write bool) {
	b.mu.Lock()
	if write {
		b.writer = false
	} else {
		b.readers--
	}
	b.signal()
	b.mu.Unlock()
}
