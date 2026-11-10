package bridge

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// retrySkipped never acquires syncMu or advances history. Each completed batch
// is durable, so stopping a pass preserves successes and permanent removals.
func (b *Bridge) retrySkipped(ctx context.Context) error {
	b.retryMu.Lock()
	defer b.retryMu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	b.mu.Lock()
	var ids []string
	if b.state.Pending == nil && b.state.History != "" {
		for id := range b.state.Skipped {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	sort.Strings(ids)
	return b.fetchMetadata(ctx, ids, metadataSkipped)
}

func (b *Bridge) runSkippedRetries(ctx context.Context) {
	// Honor the configured interval from startup; repairing the cache must not
	// be a prerequisite for the first history poll after a restart.
	timer := time.NewTimer(b.skippedInterval)
	defer timer.Stop()
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := b.retrySkipped(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := b.skippedInterval
		if err != nil {
			delay = b.retryPolicy.Delay(attempt, err)
			attempt = min(attempt+1, 63)
			slog.Warn("Skipped Gmail retries paused; history polling continues", "retry_in", delay, "error", err)
		} else {
			attempt = 0
		}
		timer.Reset(delay)
	}
}
