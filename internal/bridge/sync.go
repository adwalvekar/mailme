package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"mailme/internal/gmail"
)

func (b *Bridge) update(fn func(*state)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := clone(b.state)
	fn(next)
	return b.commit(next)
}

func (b *Bridge) Sync(ctx context.Context) error {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := b.refreshLabels(ctx); err != nil {
		return err
	}
	var err error
	b.mu.Lock()
	pending, history := b.state.Pending, b.state.History
	b.mu.Unlock()
	// Validate the pending checkpoint on every resume, without promoting the
	// returned delta cursor. The original cursor must cover the entire backfill.
	if pending != nil {
		_, _, err := b.api.Changes(ctx, pending.History)
		if is404(err) {
			slog.Warn("Pending Gmail history expired; restarting snapshot, retaining cached mail")
			if err := b.update(func(st *state) { st.Pending = nil }); err != nil {
				return err
			}
			pending, history = nil, ""
		} else if err != nil {
			return err
		}
	}
	if pending == nil && history != "" {
		ids, cursor, err := b.api.Changes(ctx, history)
		if err == nil {
			slog.Debug("Gmail history checked", "changed_messages", len(ids))
			if err = b.fetchMetadata(ctx, unique(ids), metadataHistory); err != nil {
				return err
			}
			if cursor == "" {
				return fmt.Errorf("Gmail history response has no checkpoint")
			}
			return b.advanceHistory(cursor)
		}
		if !is404(err) {
			return err
		}
	}
	if pending == nil {
		p, err := b.api.Profile(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(p.Email, b.email) || p.HistoryID == "" {
			return fmt.Errorf("Gmail profile does not match configured account or lacks history")
		}
		// Persist BEFORE listing. A listing failure resumes from this same cursor.
		if err = b.update(func(st *state) {
			st.Pending = &snapshot{History: p.HistoryID, Done: make(map[string]bool), Touched: make(map[string]bool)}
		}); err != nil {
			return err
		}
	}
	b.mu.Lock()
	pending = b.state.Pending
	b.mu.Unlock()
	if !pending.Listed {
		slog.Info("Listing Gmail message IDs; INBOX will be fetched first")
		var inbox []string
		if priority, ok := b.api.(interface {
			InboxIDs(context.Context) ([]string, error)
		}); ok {
			inbox, err = priority.InboxIDs(ctx)
			if err != nil {
				return err
			}
		}
		ids, err := b.api.IDs(ctx)
		if err != nil {
			return err
		}
		// The union also retains inbox arrivals that moved during the two lists.
		ids = unique(append(inbox, ids...))
		if err = b.update(func(st *state) { st.Pending.IDs = ids; st.Pending.Listed = true }); err != nil {
			return err
		}
	}
	b.mu.Lock()
	pending = b.state.Pending
	var todo []string
	for _, id := range pending.IDs {
		if !pending.Done[id] || b.state.Skipped[id] != "" {
			todo = append(todo, id)
		}
	}
	b.mu.Unlock()
	if err := b.fetchMetadata(ctx, todo, metadataSnapshot); err != nil {
		return err
	}
	// Do not garbage collect absent messages before listing+backfill finish. A
	// successful local operation may add a record while this snapshot is running.
	return b.update(func(st *state) {
		present := make(map[string]bool, len(st.Pending.IDs))
		for _, id := range st.Pending.IDs {
			present[id] = true
		}
		for id := range st.Messages {
			if !present[id] && !st.Pending.Touched[id] {
				delete(st.Messages, id)
			}
		}
		for id := range st.Skipped {
			if !present[id] {
				delete(st.Skipped, id)
			}
		}
		st.History = st.Pending.History
		st.Pending = nil
	})
}

func (b *Bridge) refreshLabels(ctx context.Context) error {
	if time.Since(b.lastLabels) < b.labelInterval {
		return nil
	}
	labels, err := b.api.Labels(ctx)
	if err != nil {
		return err
	}
	// Compare only folder definitions before cloning the potentially large index.
	want := &state{Folders: make(map[string]*Folder)}
	want.setLabels(labels)
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for id, folder := range want.Folders {
		old := b.state.Folders[id]
		if old == nil || !old.Active || old.Name != folder.Name || old.Attr != folder.Attr {
			changed = true
			break
		}
	}
	for id, folder := range b.state.Folders {
		if folder.Active && want.Folders[id] == nil {
			changed = true
		}
	}
	if changed {
		next := clone(b.state)
		next.setLabels(labels)
		if err := b.commit(next); err != nil {
			return err
		}
	}
	b.lastLabels = time.Now()
	return nil
}

func (b *Bridge) advanceHistory(cursor string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.History == cursor {
		return nil
	}
	if err := b.disk.saveHistory(cursor); err != nil {
		return err
	}
	next := *b.state
	next.History = cursor
	b.state = &next
	return nil
}

func unique(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}

type metadataResult struct {
	id      string
	version uint64
	message *gmail.Message
	err     error
}

type metadataKind string

const (
	metadataSnapshot metadataKind = "snapshot"
	metadataHistory  metadataKind = "history"
	metadataSkipped  metadataKind = "skipped_retry"
)

func (b *Bridge) fetchMetadata(ctx context.Context, ids []string, kind metadataKind) error {
	if len(ids) == 0 {
		return ctx.Err()
	}
	full, retry := kind == metadataSnapshot, kind == metadataSkipped
	workerCount := b.workers
	if retry {
		// Never fill the shared API pool/queue with a backlog of repair reads.
		workerCount = 1
	} else {
		b.mu.Lock()
		for _, id := range ids {
			b.metadataBusy[id] = true
		}
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for _, id := range ids {
				delete(b.metadataBusy, id)
			}
		}()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	results := make(chan metadataResult, workerCount)
	var workers sync.WaitGroup
	for n := 0; n < workerCount; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				b.mu.Lock()
				version := b.versions[id]
				eligible := !retry || b.state.Pending == nil && b.state.History != "" && b.state.Skipped[id] != "" && !b.metadataBusy[id]
				b.mu.Unlock()
				if !eligible {
					continue
				}
				m, err := b.api.Message(ctx, id, "metadata")
				if err == nil {
					err = gmail.ValidateMessage(m, id, "metadata")
				}
				select {
				case results <- metadataResult{id, version, m, err}:
				case <-ctx.Done():
					return
				}
				if err != nil && !gmail.PerMessage(err) {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	start, fetched := time.Now(), 0
	ticker := time.NewTicker(b.checkpointInterval)
	defer ticker.Stop()
	lastProgress := time.Now()
	progress := func() {
		b.mu.Lock()
		done, total, skipped := fetched, len(ids), len(b.state.Skipped)
		if full && b.state.Pending != nil {
			done, total = len(b.state.Pending.Done), len(b.state.Pending.IDs)
		}
		b.mu.Unlock()
		rate := float64(fetched) / time.Since(start).Seconds()
		eta := "unknown"
		if rate > 0 {
			eta = (time.Duration(float64(total-done)/rate) * time.Second).String()
		}
		slog.Info("Gmail sync progress", "phase", kind, "fetched", done, "total", total, "rate_messages_per_second", rate, "eta", eta, "skipped", skipped)
	}
	progress()
	batch := make([]metadataResult, 0, b.batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var removed []string
		err := b.update(func(st *state) {
			for _, result := range batch {
				// A repair result must not resurrect an ID cleared by history, race
				// a new snapshot, or overwrite a newer history/local operation.
				if retry && (st.Pending != nil || st.History == "" || st.Skipped[result.id] == "" || b.metadataBusy[result.id] || b.versions[result.id] != result.version) {
					continue
				}
				// A network read started before/during a local mutation must never
				// overwrite that mutation's result after it commits.
				if b.versions[result.id] == result.version {
					if result.err == nil {
						putRecord(st, result.message)
						delete(st.Skipped, result.id)
					} else if is404(result.err) {
						delete(st.Messages, result.id)
						delete(st.Skipped, result.id)
						removed = append(removed, result.id)
					} else {
						st.Skipped[result.id] = result.err.Error()
					}
					// Invalidate older skipped reads even if history left the same
					// transient error in the skipped set.
					b.versions[result.id]++
				} else {
					st.Skipped[result.id] = "metadata refresh deferred after concurrent local operation"
				}
				if full {
					st.Pending.Done[result.id] = true
				}
			}
		})
		if err == nil {
			for _, id := range removed {
				slog.Debug("Removed missing Gmail message; will not retry", "id", gmail.SafeIdentifier(id))
			}
		}
		batch = batch[:0]
		return err
	}
	var failure error
	for {
		select {
		case <-ticker.C:
			if failure == nil {
				if err := flush(); err != nil {
					failure = err
					cancel()
				}
			}
			if time.Since(lastProgress) >= 30*time.Second {
				progress()
				lastProgress = time.Now()
			}
		case result, ok := <-results:
			if !ok {
				if err := flush(); err != nil {
					return err
				}
				progress()
				if failure != nil {
					return failure
				}
				return ctx.Err()
			}
			if result.err != nil && !gmail.PerMessage(result.err) {
				if failure == nil {
					failure = result.err
					cancel()
				}
				continue
			}
			if result.err != nil && !is404(result.err) {
				slog.Warn("Skipping Gmail message; will retry", "id", gmail.SafeIdentifier(result.id), "error", result.err)
			}
			batch = append(batch, result)
			fetched++
			// Publish a small first batch for quick INBOX access, then checkpoint
			// at the configured batch/time limits. Cancellation flushes successes too.
			if failure == nil && (len(batch) >= b.batchSize || fetched == workerCount) {
				if err := flush(); err != nil {
					failure = err
					cancel()
				}
			}
		}
	}
}

func (b *Bridge) RunSync(ctx context.Context, interval time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	done := make(chan struct{})
	go func() { defer close(done); b.runSkippedRetries(ctx) }()
	defer func() { stop(); cancel(); <-done }()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	attempt := 0
	for ctx.Err() == nil {
		started := time.Now()
		err := b.Sync(ctx)
		// Cadence is measured between starts, with a floor after slow syncs so
		// an overrun cannot turn into a continuous catch-up loop.
		delay := interval - time.Since(started)
		if delay <= 0 {
			delay = min(interval, time.Second)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			delay = b.retryPolicy.Delay(attempt, err)
			attempt = min(attempt+1, 63)
			slog.Warn("Gmail sync paused; cached mail remains available", "retry_in", delay, "error", err)
		} else {
			attempt = 0
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-b.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
